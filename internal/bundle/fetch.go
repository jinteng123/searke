package bundle

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

func flog(opts FetchOptions, f string, a ...any) {
	if opts.Log != nil {
		opts.Log(f, a...)
	}
}

// FetchOptions controls a bundle fetch run.
type FetchOptions struct {
	OutputDir string
	Timeout   time.Duration // per-request timeout; 0 => 10m
	Retries   int           // download attempts per asset; 0 => 3
	Client    *http.Client  // optional override (tests)
	Log       func(format string, args ...any)
}

// defaultTransport separates connection-level timeouts (fail fast, so
// fallbacks kick in) from the overall body timeout (large payloads).
var defaultTransport = &http.Transport{
	Proxy: http.ProxyFromEnvironment,
	DialContext: (&net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext,
	TLSHandshakeTimeout:   10 * time.Second,
	ResponseHeaderTimeout: 30 * time.Second,
	ExpectContinueTimeout: 5 * time.Second,
}

// releaseMeta maps release asset name -> API asset URL. Fetching the
// metadata up front doubles as the "tag and assets exist" precheck.
type releaseMeta struct {
	Tag    string
	Assets map[string]string // name -> api.github.com/releases/assets/<id>
}

// fetchReleaseMeta queries the GitHub API for the pinned tag. A nil
// meta with nil error means the API was unreachable; callers then fall
// back to primary-URL-only candidates.
func fetchReleaseMeta(ctx context.Context, opts FetchOptions, version string) (*releaseMeta, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiReleaseURL(version), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "searke")
	client := *opts.Client
	client.Timeout = 30 * time.Second
	resp, err := client.Do(req)
	if err != nil {
		return nil, err // caller treats as "no meta"
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("release tag %s not found upstream", version)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("release metadata: %s", resp.Status)
	}
	var body struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	m := &releaseMeta{Tag: body.TagName, Assets: map[string]string{}}
	for _, a := range body.Assets {
		m.Assets[a.Name] = a.URL
	}
	return m, nil
}

// candidatesFor builds the URL list for an asset. Trust anchors get the
// GitHub API asset URL as fallback; large payloads additionally get
// mirror URLs. A trust anchor missing from the release metadata keeps
// only its primary URL (a failed primary then fails the fetch — better
// than silently trusting a mirror for the checksum manifest).
func candidatesFor(a Asset, meta *releaseMeta) []string {
	out := []string{a.URL}
	if a.TrustAnchor {
		if meta != nil {
			if u, ok := meta.Assets[a.Name]; ok {
				out = append(out, u)
			}
		}
		// Pre-seeded API fallbacks (e.g. install.sh contents API).
		if len(a.URLs) > 1 {
			out = append(out, a.URLs[1:]...)
		}
		return out
	}
	for _, tmpl := range ReleaseMirrors {
		out = append(out, fmt.Sprintf(tmpl, a.URL))
	}
	return out
}

// Fetch downloads all assets declared by the bundle into opts.OutputDir
// via a staging directory, verifies them against the upstream SHA256
// manifests and writes bundle.lock.yaml. The output directory is only
// swapped in after every file verifies, so a failed run never leaves a
// half-updated bundle next to a stale lock.
func Fetch(ctx context.Context, b *Bundle, opts FetchOptions) error {
	if opts.Timeout == 0 {
		opts.Timeout = 10 * time.Minute
	}
	if opts.Retries == 0 {
		opts.Retries = 3
	}
	if opts.Client == nil {
		opts.Client = &http.Client{Transport: defaultTransport}
	}
	platforms, err := b.PlatformList()
	if err != nil {
		return err
	}
	cni, err := NormalizeCNI(b.Spec.CNI)
	if err != nil {
		return err
	}

	// Refuse to clobber a non-bundle directory.
	if entries, err := os.ReadDir(opts.OutputDir); err == nil && len(entries) > 0 {
		if _, err := os.Stat(filepath.Join(opts.OutputDir, FileName)); err != nil {
			return fmt.Errorf("%s is not empty and not a bundle directory", opts.OutputDir)
		}
	}

	meta, err := fetchReleaseMeta(ctx, opts, b.Spec.Version)
	if err != nil {
		flog(opts, "  warning: release metadata unavailable (%v); using primary URLs only", err)
		meta = nil
	} else {
		flog(opts, "  release %s confirmed upstream", meta.Tag)
	}

	parent := filepath.Dir(filepath.Clean(opts.OutputDir))
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(parent, ".searke-fetch-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)

	var lockAssets []LockAsset
	dl := func(a Asset, dir string) error {
		a.URLs = candidatesFor(a, meta)
		if len(a.URLs) == 1 && a.TrustAnchor && meta != nil {
			flog(opts, "  warning: %s missing from release metadata; primary URL only", a.Name)
		}
		flog(opts, "  downloading %s", a.Name)
		la, err := downloadAsset(ctx, opts, a, dir)
		if err != nil {
			return err
		}
		if len(a.URLs) > 1 && la.URL != a.URLs[0] {
			flog(opts, "  note: %s served by fallback %s", a.Name, hostOf(la.URL))
		}
		lockAssets = append(lockAssets, la)
		return nil
	}

	// Shared files (install.sh) live at the bundle root.
	for _, a := range SharedAssets(b.Spec.Version) {
		a.TrustAnchor = true
		if err := dl(a, stage); err != nil {
			return err
		}
	}

	for _, p := range platforms {
		dir := filepath.Join(stage, "payload", p.DirName())
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		for _, a := range AssetsFor(b.Spec.Version, cni, p) {
			if err := dl(a, dir); err != nil {
				return err
			}
		}
		// Verify against the just-fetched (trusted) upstream manifest.
		if err := VerifyPlatform(dir, cni, p); err != nil {
			return err
		}
		flog(opts, "  %s: payload verified", p)
	}

	src, err := yaml.Marshal(b)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(stage, FileName), src, 0o644); err != nil {
		return err
	}
	lock := &Lock{
		APIVersion: APIVersion,
		Kind:       LockKind,
		Spec: LockSpec{
			Version: b.Spec.Version,
			CNI:     cni,
			Source:  ReleaseBase,
			Assets:  lockAssets,
		},
	}
	if err := WriteLock(stage, lock); err != nil {
		return err
	}

	// Everything verified: swap into place.
	if err := os.RemoveAll(opts.OutputDir); err != nil {
		return err
	}
	return os.Rename(stage, opts.OutputDir)
}

func hostOf(u string) string {
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	}
	if i := strings.Index(u, "/"); i >= 0 {
		u = u[:i]
	}
	return u
}

// downloadAsset fetches one asset to dir/asset.Name via a temp file,
// resuming interrupted downloads when the server honors Range requests.
// Each retry walks the candidate URLs so fallbacks get a turn.
func downloadAsset(ctx context.Context, opts FetchOptions, a Asset, dir string) (LockAsset, error) {
	dest := filepath.Join(dir, a.Name)
	tmp := dest + ".part"
	candidates := a.Candidates()

	var usedURL string
	var size int64
	for attempt := 1; attempt <= opts.Retries; attempt++ {
		url := candidates[(attempt-1)%len(candidates)]
		if err := downloadOnce(ctx, opts, url, tmp); err != nil {
			if attempt == opts.Retries {
				return LockAsset{}, fmt.Errorf("download %s: %w (tried %v)", a.Name, err, candidates)
			}
			select {
			case <-ctx.Done():
				return LockAsset{}, ctx.Err()
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			}
			continue
		}
		usedURL = url
		break
	}

	fi, err := os.Stat(tmp)
	if err != nil {
		return LockAsset{}, err
	}
	size = fi.Size()
	if size == 0 {
		os.Remove(tmp)
		return LockAsset{}, fmt.Errorf("download %s: empty response body", a.URL)
	}
	if err := os.Rename(tmp, dest); err != nil {
		return LockAsset{}, err
	}
	sum, err := FileSHA256(dest)
	if err != nil {
		return LockAsset{}, err
	}
	return LockAsset{
		Name:     a.Name,
		Platform: a.Platform,
		URL:      usedURL,
		Size:     size,
		SHA256:   sum,
	}, nil
}

// apiContentsReply is the GitHub contents API envelope.
type apiContentsReply struct {
	Encoding string `json:"encoding"`
	Content  string `json:"content"`
}

// stallTimeout kills a download whose body stops delivering bytes —
// a slow-but-alive connection otherwise never falls back to a mirror.
const stallTimeout = 45 * time.Second

type stallReader struct {
	r      io.Reader
	onRead func()
}

func (s *stallReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 {
		s.onRead()
	}
	return n, err
}

func downloadOnce(ctx context.Context, opts FetchOptions, url string, tmp string) error {
	var offset int64
	if fi, err := os.Stat(tmp); err == nil {
		offset = fi.Size()
	}

	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	req.Header.Set("User-Agent", "searke")
	isAPI := strings.HasPrefix(url, APIBase)
	switch {
	case strings.HasPrefix(url, APIBase+"/contents/"):
		req.Header.Set("Accept", "application/vnd.github+json")
	case isAPI:
		// /releases/assets/<id> — API serves raw bytes for octet-stream.
		req.Header.Set("Accept", "application/octet-stream")
	}

	client := *opts.Client
	client.Timeout = opts.Timeout
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK && offset == 0:
		// fresh download
	case resp.StatusCode == http.StatusPartialContent && offset > 0:
		// resume
	case resp.StatusCode == http.StatusOK && offset > 0:
		// server ignored Range; restart
		offset = 0
	default:
		return fmt.Errorf("unexpected HTTP status %s", resp.Status)
	}

	// Abort the request if the body stalls — retry picks the next URL.
	stall := time.AfterFunc(stallTimeout, func() { cancel() })
	defer stall.Stop()

	// The contents API wraps file bytes in JSON; decode before writing.
	var body io.Reader = &stallReader{r: resp.Body, onRead: func() { stall.Reset(stallTimeout) }}
	if strings.HasPrefix(url, APIBase+"/contents/") {
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		var c apiContentsReply
		if err := json.Unmarshal(data, &c); err != nil {
			return fmt.Errorf("contents API reply is not JSON: %w", err)
		}
		if c.Encoding != "base64" {
			return fmt.Errorf("contents API encoding %q unsupported", c.Encoding)
		}
		raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(c.Content, "\n", ""))
		if err != nil {
			return fmt.Errorf("contents API base64: %w", err)
		}
		body = strings.NewReader(string(raw))
		offset = 0
	}

	flag := os.O_CREATE | os.O_WRONLY
	if offset > 0 {
		flag |= os.O_APPEND
	} else {
		flag |= os.O_TRUNC
	}
	f, err := os.OpenFile(tmp, flag, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	_, err = io.Copy(f, body)
	return err
}

// FileSHA256 streams a file and returns its hex SHA256.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
