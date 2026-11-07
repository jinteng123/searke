package ociimg

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// serve.go: a minimal read-only OCI registry that exposes one local
// OCI layout directory over plain HTTP. Purpose: let air-gapped nodes
// pull the bundle index when no real registry is available —
//
//	searke image serve dist/rke2-oci --address 0.0.0.0:15000
//	searke run <host>:15000/searke/rke2:v1 --insecure
//
// Deliberately dumb: any repo name resolves to the same index; pushes
// are rejected. No auth — bind it to a trusted interface.
//
// Endpoints implemented (what go-containerregistry pull needs):
//
//	GET /v2/                                        API version probe
//	GET /v2/<repo>/manifests/<tag>                  -> index.json (as OCI index)
//	GET /v2/<repo>/manifests/<sha256:...>           -> content-addressed blob
//	HEAD+GET /v2/<repo>/blobs/<sha256:...>          -> layer/config/manifest blob

const (
	mtIndex    = "application/vnd.oci.image.index.v1+json"
	mtManifest = "application/vnd.oci.image.manifest.v1+json"
)

// ServeRegistry serves the layout directory until the server stops.
func ServeRegistry(layoutDir, addr string, logf func(string, ...any)) error {
	if _, err := os.Stat(filepath.Join(layoutDir, "index.json")); err != nil {
		return fmt.Errorf("%s is not an OCI layout directory: %w", layoutDir, err)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	logf("serving %s on http://%s (read-only, no auth)", layoutDir, ln.Addr())
	return http.Serve(ln, &layoutRegistry{dir: layoutDir})
}

type layoutRegistry struct{ dir string }

func (l *layoutRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPut || r.Method == http.MethodPost ||
		r.Method == http.MethodPatch || r.Method == http.MethodDelete {
		http.Error(w, "read-only registry", http.StatusMethodNotAllowed)
		return
	}
	switch {
	case r.URL.Path == "/v2/" || r.URL.Path == "/v2":
		w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
		w.WriteHeader(http.StatusOK)

	case strings.Contains(r.URL.Path, "/manifests/"):
		ref := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		l.serveManifest(w, r, ref)

	case strings.Contains(r.URL.Path, "/blobs/"):
		digest := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		l.serveBlob(w, r, digest)

	default:
		http.NotFound(w, r)
	}
}

// serveManifest resolves a tag to index.json, a digest to the matching
// content blob (manifest or index — media type sniffed from content).
func (l *layoutRegistry) serveManifest(w http.ResponseWriter, r *http.Request, ref string) {
	if strings.HasPrefix(ref, "sha256:") {
		l.serveBlobAs(w, r, ref, "")
		return
	}
	body, err := os.ReadFile(filepath.Join(l.dir, "index.json"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", mtIndex)
	w.Header().Set("Docker-Content-Digest", fileDigest(body))
	w.Write(body)
}

// serveBlob streams blobs/sha256/<hex>.
func (l *layoutRegistry) serveBlob(w http.ResponseWriter, r *http.Request, digest string) {
	l.serveBlobAs(w, r, digest, "application/octet-stream")
}

// serveBlobAs reads a content-addressed blob and reports a guessed
// manifest media type when none is forced.
func (l *layoutRegistry) serveBlobAs(w http.ResponseWriter, r *http.Request, digest, mediaType string) {
	hex := strings.TrimPrefix(digest, "sha256:")
	if len(hex) != 64 || !isHexLower(hex) {
		http.NotFound(w, r)
		return
	}
	p := filepath.Join(l.dir, "blobs", "sha256", hex)
	f, err := os.Open(p)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	if mediaType == "" {
		// Manifests are a few KB at most; read the whole thing so a
		// large index is not misclassified by a truncated probe.
		body, err := io.ReadAll(io.LimitReader(f, 1<<20))
		if err != nil {
			http.Error(w, "read blob", http.StatusInternalServerError)
			return
		}
		if _, err := f.Seek(0, 0); err != nil {
			http.Error(w, "seek blob", http.StatusInternalServerError)
			return
		}
		mediaType = sniffManifestType(body)
	}
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Content-Length", fmt.Sprint(st.Size()))
	if r.Method == http.MethodHead {
		return
	}
	http.ServeContent(w, r, "", st.ModTime(), f)
}

// fileDigest returns the sha256:hex digest header for a byte blob.
func fileDigest(b []byte) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256(b))
}

// sniffManifestType distinguishes an image index from a single
// manifest (blobs store both content-addressed). The declared
// mediaType field wins; structure is the fallback.
func sniffManifestType(body []byte) string {
	var probe struct {
		MediaType string            `json:"mediaType"`
		Manifests []json.RawMessage `json:"manifests"`
		Config    json.RawMessage   `json:"config"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return mtManifest
	}
	switch probe.MediaType {
	case mtIndex, "application/vnd.docker.distribution.manifest.list.v2+json":
		return mtIndex
	case mtManifest, "application/vnd.docker.distribution.manifest.v2+json":
		return mtManifest
	}
	if len(probe.Manifests) > 0 && len(probe.Config) == 0 {
		return mtIndex
	}
	return mtManifest
}

func isHexLower(s string) bool {
	for i := 0; i < len(s); i++ {
		b := s[i]
		if (b < '0' || b > '9') && (b < 'a' || b > 'f') {
			return false
		}
	}
	return true
}
