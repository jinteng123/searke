package ociimg

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/jint/searke/internal/bundle"
)

// makeBundleDir creates a fake but fully verifiable bundle directory.
func makeBundleDir(t *testing.T, version string, arches ...string) string {
	t.Helper()
	dir := t.TempDir()
	b := fmt.Sprintf(`apiVersion: %s
kind: %s
spec:
  version: %s
  cni: canal
  platforms:
`, bundle.APIVersion, bundle.Kind, version)
	for _, a := range arches {
		b += fmt.Sprintf("    - linux/%s\n", a)
	}
	if err := os.WriteFile(filepath.Join(dir, "bundle.yaml"), []byte(b), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "install.sh"), []byte("#!/bin/sh\necho install\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, arch := range arches {
		pd := filepath.Join(dir, "payload", "linux-"+arch)
		if err := os.MkdirAll(pd, 0o755); err != nil {
			t.Fatal(err)
		}
		files := map[string]string{
			fmt.Sprintf("rke2.linux-%s.tar.gz", arch):         "binary-" + arch,
			fmt.Sprintf("rke2-images.linux-%s.tar.zst", arch): "images-" + arch,
		}
		var sums string
		for name, content := range files {
			if err := os.WriteFile(filepath.Join(pd, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			sums += fmt.Sprintf("%x  %s\n", sha256.Sum256([]byte(content)), name)
		}
		if err := os.WriteFile(filepath.Join(pd, "sha256sum-"+arch+".txt"), []byte(sums), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestBuildAndExtractRoundTrip(t *testing.T) {
	src := makeBundleDir(t, "v1.36.5+rke2r1", "amd64", "arm64")
	out := filepath.Join(t.TempDir(), "oci")
	meta, err := Build(src, out, "")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if meta.Version != "v1.36.5+rke2r1" || len(meta.Platforms) != 2 {
		t.Fatalf("bad meta: %+v", meta)
	}

	// Reopen the layout and check index metadata + platforms.
	s := Source{Path: out}
	ii, cleanup, err := s.OpenIndex()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	m2, err := IndexMeta(ii)
	if err != nil {
		t.Fatal(err)
	}
	if m2.Version != "v1.36.5+rke2r1" || m2.CNI != "canal" {
		t.Fatalf("bad index meta: %+v", m2)
	}

	// Extract arm64 payload and verify contents.
	dest := t.TempDir()
	p := bundle.Platform{OS: "linux", Arch: "arm64"}
	if err := ExtractPayload(ii, p, dest); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "opt", "searke", "payload", "rke2-images.linux-arm64.tar.zst"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "images-arm64" {
		t.Fatalf("payload content = %q", got)
	}
	// amd64 payload must NOT be present in arm64 sub-image.
	if _, err := os.Stat(filepath.Join(dest, "opt", "searke", "payload", "rke2.linux-amd64.tar.gz")); !os.IsNotExist(err) {
		t.Fatal("amd64 payload leaked into arm64 sub-image")
	}
	// extracted payload verifies against its manifest
	if err := bundle.VerifyPlatform(filepath.Join(dest, "opt", "searke", "payload"), "canal", p); err != nil {
		t.Fatalf("extracted payload failed verification: %v", err)
	}

	// unsupported platform errors clearly
	if err := ExtractPayload(ii, bundle.Platform{OS: "linux", Arch: "amd64"}, dest); err != nil {
		t.Fatal(err)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	src := makeBundleDir(t, "v1.36.5+rke2r1", "amd64")
	layoutDir := filepath.Join(t.TempDir(), "oci")
	if _, err := Build(src, layoutDir, ""); err != nil {
		t.Fatal(err)
	}
	s := Source{Path: layoutDir}
	ii, cleanup, err := s.OpenIndex()
	if err != nil {
		t.Fatal(err)
	}
	cleanup()

	tarPath := filepath.Join(t.TempDir(), "img.oci.tar")
	if err := SaveTar(ii, tarPath); err != nil {
		t.Fatal(err)
	}
	loaded := filepath.Join(t.TempDir(), "loaded")
	if err := LoadTar(tarPath, loaded); err != nil {
		t.Fatal(err)
	}
	s2 := Source{Path: loaded}
	ii2, cl2, err := s2.OpenIndex()
	if err != nil {
		t.Fatal(err)
	}
	defer cl2()
	m, err := IndexMeta(ii2)
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != "v1.36.5+rke2r1" {
		t.Fatalf("loaded meta: %+v", m)
	}
}

func TestBuildReproducible(t *testing.T) {
	src := makeBundleDir(t, "v1.36.5+rke2r1", "amd64")
	m1, err := Build(src, filepath.Join(t.TempDir(), "a"), "")
	if err != nil {
		t.Fatal(err)
	}
	m2, err := Build(src, filepath.Join(t.TempDir(), "b"), "")
	if err != nil {
		t.Fatal(err)
	}
	for p, d := range m1.Digests {
		if m2.Digests[p] != d {
			t.Fatalf("%s: digests differ across builds: %s vs %s", p, d, m2.Digests[p])
		}
	}
}

func TestPrepareLayoutDir(t *testing.T) {
	// refuses non-empty non-layout dir
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "keep.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := prepareLayoutDir(d); err == nil {
		t.Fatal("must refuse non-layout dir")
	}
	if _, err := os.Stat(filepath.Join(d, "keep.txt")); err != nil {
		t.Fatal("file was deleted despite refusal")
	}
	// allows existing layout dir
	if err := os.WriteFile(filepath.Join(d, "oci-layout"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := prepareLayoutDir(d); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(d); err != nil {
		t.Fatal("dir removed")
	}
	// creates missing dir
	if err := prepareLayoutDir(filepath.Join(d, "new")); err != nil {
		t.Fatal(err)
	}
}

func TestParseSource(t *testing.T) {
	d := t.TempDir()
	s, err := ParseSource(d)
	if err != nil || s.Path != d {
		t.Fatalf("local dir: %+v %v", s, err)
	}
	s, err = ParseSource("registry.example.com/searke/rke2:v1.36.5-rke2r1")
	if err != nil || s.Ref == "" {
		t.Fatalf("registry ref: %+v %v", s, err)
	}
	if _, err := ParseSource("not a ref at all:::"); err == nil {
		t.Fatal("garbage should fail")
	}
}
