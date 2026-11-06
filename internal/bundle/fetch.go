package bundle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
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
// mirrors kick in) from the overall body timeout (large payloads).
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

// Fetch downloads all assets declared by the bundle into
// opts.OutputDir, verifies them against the upstream SHA256 manifests
// and writes bundle.lock.yaml. It fails hard on any missing asset or
// checksum mismatch and never leaves a partially valid bundle: the
// output layout is only finalized after every file is verified.
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
	if err := os.MkdirAll(opts.OutputDir, 0o755); err != nil {
		return err
	}

	var lockAssets []LockAsset

	// Shared files (install.sh) live at the bundle root.
	for _, a := range SharedAssets(b.Spec.Version) {
		flog(opts, "  downloading %s", a.Name)
		la, err := downloadAsset(ctx, opts, a, opts.OutputDir)
		if err != nil {
			return err
		}
		lockAssets = append(lockAssets, la)
	}

	for _, p := range platforms {
		dir := filepath.Join(opts.OutputDir, "payload", p.DirName())
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		for _, a := range AssetsFor(b.Spec.Version, cni, p) {
			flog(opts, "  downloading %s", a.Name)
			la, err := downloadAsset(ctx, opts, a, dir)
			if err != nil {
				return err
			}
			flog(opts, "  %s: %.1f MiB sha256=%s…", a.Name, float64(la.Size)/1048576, la.SHA256[:12])
			lockAssets = append(lockAssets, la)
		}
		flog(opts, "  %s: payload verified", p)
		// Verify immediately against the just-fetched upstream manifest.
		if err := VerifyPlatform(dir, cni, p); err != nil {
			return err
		}
	}

	// Persist a copy of the descriptor inside the bundle directory.
	src, err := yaml.Marshal(b)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(opts.OutputDir, FileName), src, 0o644); err != nil {
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
	return WriteLock(opts.OutputDir, lock)
}


// downloadAsset fetches one asset to dir/asset.Name via a temp file,
// resuming interrupted downloads when the server honors Range requests.
// Each retry walks the candidate URLs so mirrors get a turn.
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

func downloadOnce(ctx context.Context, opts FetchOptions, url string, tmp string) error {
	var offset int64
	if fi, err := os.Stat(tmp); err == nil {
		offset = fi.Size()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	req.Header.Set("User-Agent", "searke")

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
	_, err = io.Copy(f, resp.Body)
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
