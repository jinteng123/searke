package bundle

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRelease serves the minimal set of files Fetch needs, including
// the release-metadata endpoint and API fallbacks. It deliberately
// does NOT serve sha256sum-arm64.txt or install.sh on their primary
// URLs so the test exercises the API fallback path.
func fakeRelease(t *testing.T) *httptest.Server {
	t.Helper()
	tag := EscapedTag("v1.36.5+rke2r1")
	files := map[string][]byte{
		"rke2.linux-amd64.tar.gz":         []byte("bin-amd64"),
		"rke2-images.linux-amd64.tar.zst": []byte("img-amd64"),
		"rke2.linux-arm64.tar.gz":         []byte("bin-arm64"),
		"rke2-images.linux-arm64.tar.zst": []byte("img-arm64"),
	}
	mux := http.NewServeMux()
	for _, arch := range []string{"amd64", "arm64"} {
		var sums string
		for name, body := range files {
			if strings.HasSuffix(name, arch+".tar.gz") || strings.HasSuffix(name, arch+".tar.zst") {
				sums += fmt.Sprintf("%x  %s\n", sha256.Sum256(body), name)
			}
		}
		files["sha256sum-"+arch+".txt"] = []byte(sums)
	}
	var srvURL string // set after NewServer; handlers read it at request time

	// primary release-asset URLs (arm64 sum intentionally absent)
	for name, body := range files {
		if name == "sha256sum-arm64.txt" {
			continue // only reachable via the API fallback
		}
		n, b := name, body
		mux.HandleFunc("/releases/download/"+tag+"/"+n, func(w http.ResponseWriter, r *http.Request) {
			w.Write(b)
		})
	}
	// API: release metadata
	mux.HandleFunc("/releases/tags/"+tag, func(w http.ResponseWriter, r *http.Request) {
		var assets []map[string]string
		for name := range files {
			assets = append(assets, map[string]string{
				"name": name,
				"url":  srvURL + "/releases/assets/" + name,
			})
		}
		json.NewEncoder(w).Encode(map[string]any{"tag_name": "v1.36.5+rke2r1", "assets": assets})
	})
	// API: raw asset bytes
	mux.HandleFunc("/releases/assets/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/releases/assets/")
		w.Write(files[name])
	})
	// API: contents (install.sh fallback)
	mux.HandleFunc("/contents/install.sh", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{
			"encoding": "base64",
			"content":  base64.StdEncoding.EncodeToString([]byte("#!/bin/sh\ntrue\n")),
		})
	})
	srv := httptest.NewServer(mux)
	srvURL = srv.URL
	t.Cleanup(srv.Close)
	return srv
}

func testBundle() *Bundle {
	return &Bundle{APIVersion: APIVersion, Kind: Kind, Spec: Spec{
		Version: "v1.36.5+rke2r1", CNI: "canal",
		Platforms: []string{"linux/amd64", "linux/arm64"},
	}}
}

func stubBases(t *testing.T, srv *httptest.Server) {
	t.Helper()
	oldR, oldRaw, oldAPI, oldMirrors := ReleaseBase, RawBase, APIBase, ReleaseMirrors
	t.Cleanup(func() { ReleaseBase, RawBase, APIBase, ReleaseMirrors = oldR, oldRaw, oldAPI, oldMirrors })
	ReleaseBase = srv.URL
	RawBase = srv.URL
	APIBase = srv.URL
	ReleaseMirrors = nil
}

func TestFetchEndToEnd(t *testing.T) {
	srv := fakeRelease(t)
	stubBases(t, srv)

	out := filepath.Join(t.TempDir(), "bundle")
	if err := Fetch(context.Background(), testBundle(), FetchOptions{OutputDir: out}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		"bundle.yaml", "bundle.lock.yaml", "install.sh",
		"payload/linux-amd64/rke2.linux-amd64.tar.gz",
		"payload/linux-amd64/rke2-images.linux-amd64.tar.zst",
		"payload/linux-amd64/sha256sum-amd64.txt",
		"payload/linux-arm64/rke2.linux-arm64.tar.gz",
		"payload/linux-arm64/rke2-images.linux-arm64.tar.zst",
		"payload/linux-arm64/sha256sum-arm64.txt",
	} {
		if _, err := os.Stat(filepath.Join(out, p)); err != nil {
			t.Errorf("missing %s", p)
		}
	}
	lock, err := LoadLock(out)
	if err != nil {
		t.Fatal(err)
	}
	if lock.Spec.Version != "v1.36.5+rke2r1" || len(lock.Spec.Assets) != 7 {
		t.Fatalf("bad lock: %+v", lock.Spec)
	}
	// install.sh must have come through the contents API fallback
	for _, a := range lock.Spec.Assets {
		if a.Name == "install.sh" && !strings.Contains(a.URL, "/contents/") {
			t.Fatalf("install.sh fetched from %s, want contents API fallback", a.URL)
		}
		if a.Name == "sha256sum-arm64.txt" && !strings.Contains(a.URL, "/releases/assets/") {
			t.Fatalf("arm64 sum fetched from %s, want API asset fallback", a.URL)
		}
	}
	if _, err := VerifyBundleDir(out); err != nil {
		t.Fatalf("re-verify: %v", err)
	}
}

func TestFetchFailsOnMissingAsset(t *testing.T) {
	srv := fakeRelease(t)
	stubBases(t, srv)

	b := testBundle()
	b.Spec.CNI = "cilium"
	out := filepath.Join(t.TempDir(), "bundle")
	err := Fetch(context.Background(), b, FetchOptions{OutputDir: out, Retries: 1})
	if err == nil {
		t.Fatal("missing cilium assets must fail")
	}
	// nothing may be left in the output dir
	entries, _ := os.ReadDir(out)
	if len(entries) != 0 {
		t.Fatalf("failed fetch left files in output dir: %v", entries)
	}
}

func TestFetchRefusesNonBundleDir(t *testing.T) {
	srv := fakeRelease(t)
	stubBases(t, srv)
	out := t.TempDir()
	if err := os.WriteFile(filepath.Join(out, "unrelated.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Fetch(context.Background(), testBundle(), FetchOptions{OutputDir: out}); err == nil {
		t.Fatal("fetch must refuse to clobber a non-bundle directory")
	}
}

func TestFetchResumes(t *testing.T) {
	tag := EscapedTag("v1.36.5+rke2r1")
	body := []byte("0123456789abcdef-partial-download-body")
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/download/"+tag+"/rke2.linux-amd64.tar.gz", func(w http.ResponseWriter, r *http.Request) {
		rg := r.Header.Get("Range")
		if rg == "" {
			w.Write(body)
			return
		}
		var off int
		fmt.Sscanf(rg, "bytes=%d-", &off)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", off, len(body)-1, len(body)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(body[off:])
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	stubBases(t, srv)

	opts := FetchOptions{Client: srv.Client(), Retries: 1}
	tmp := filepath.Join(t.TempDir(), "f.part")
	if err := os.WriteFile(tmp, body[:10], 0o644); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(srv.URL + "/releases/download/" + tag + "/rke2.linux-amd64.tar.gz")
	if err := downloadOnce(context.Background(), opts, u.String(), tmp); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(tmp)
	if string(got) != string(body) {
		t.Fatalf("resumed file = %q", got)
	}
}
