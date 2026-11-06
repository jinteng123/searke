package bundle

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRelease serves the minimal set of files Fetch needs.
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
	for name, body := range files {
		n, b := name, body
		mux.HandleFunc("/releases/download/"+tag+"/"+n, func(w http.ResponseWriter, r *http.Request) {
			w.Write(b)
		})
	}
	mux.HandleFunc("/"+tag+"/install.sh", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("#!/bin/sh\n"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func testBundle() *Bundle {
	return &Bundle{APIVersion: APIVersion, Kind: Kind, Spec: Spec{
		Version: "v1.36.5+rke2r1", CNI: "canal",
		Platforms: []string{"linux/amd64", "linux/arm64"},
	}}
}

func TestFetchEndToEnd(t *testing.T) {
	srv := fakeRelease(t)
	oldBase, oldRaw, oldMirrors := ReleaseBase, RawBase, ScriptMirrors
	defer func() { ReleaseBase, RawBase, ScriptMirrors = oldBase, oldRaw, oldMirrors }()
	ReleaseBase = srv.URL
	RawBase = srv.URL
	ScriptMirrors = nil

	out := filepath.Join(t.TempDir(), "bundle")
	if err := Fetch(context.Background(), testBundle(), FetchOptions{OutputDir: out}); err != nil {
		t.Fatal(err)
	}
	// layout
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
	// lock file records URLs + hashes
	lock, err := LoadLock(out)
	if err != nil {
		t.Fatal(err)
	}
	if lock.Spec.Version != "v1.36.5+rke2r1" || len(lock.Spec.Assets) != 7 {
		t.Fatalf("bad lock: %+v", lock.Spec)
	}
	// re-verify passes
	if _, err := VerifyBundleDir(out); err != nil {
		t.Fatalf("re-verify: %v", err)
	}
}

func TestFetchFailsOnMissingAsset(t *testing.T) {
	srv := fakeRelease(t)
	oldBase, oldRaw, oldMirrors := ReleaseBase, RawBase, ScriptMirrors
	defer func() { ReleaseBase, RawBase, ScriptMirrors = oldBase, oldRaw, oldMirrors }()
	ReleaseBase = srv.URL
	RawBase = srv.URL
	ScriptMirrors = nil

	b := testBundle()
	// break one asset: server has no cilium images
	b.Spec.CNI = "cilium"
	out := filepath.Join(t.TempDir(), "bundle")
	err := Fetch(context.Background(), b, FetchOptions{OutputDir: out, Retries: 1})
	if err == nil {
		t.Fatal("missing cilium assets must fail")
	}
	// no lock must be written
	if _, err := os.Stat(filepath.Join(out, LockFileName)); !os.IsNotExist(err) {
		t.Fatal("failed fetch must not leave a lock file")
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

	opts := FetchOptions{Client: srv.Client(), Retries: 1}
	tmp := filepath.Join(t.TempDir(), "f.part")
	// pre-seed a partial file to exercise the Range path
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
