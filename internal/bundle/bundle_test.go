package bundle

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestValidateVersion(t *testing.T) {
	good := []string{"v1.36.5+rke2r1", "v1.28.12+rke2r1", "v1.36.5-rc1+rke2r2"}
	for _, v := range good {
		if err := ValidateVersion(v); err != nil {
			t.Errorf("version %q should be valid: %v", v, err)
		}
	}
	bad := []string{"", "latest", "stable", "v1.36.5", "1.36.5+rke2r1", "v1.36+rke2r1", "v1.36.5+rke2"}
	for _, v := range bad {
		if err := ValidateVersion(v); err == nil {
			t.Errorf("version %q should be rejected", v)
		}
	}
}

func TestParsePlatform(t *testing.T) {
	p, err := ParsePlatform("linux/arm64")
	if err != nil || p.Arch != "arm64" || p.DirName() != "linux-arm64" {
		t.Fatalf("unexpected: %v %v", p, err)
	}
	for _, s := range []string{"darwin/arm64", "linux/ppc64le", "linux", "amd64", ""} {
		if _, err := ParsePlatform(s); err == nil {
			t.Errorf("platform %q should be rejected", s)
		}
	}
}

func TestEscapedTag(t *testing.T) {
	if got := EscapedTag("v1.36.5+rke2r1"); got != "v1.36.5%2Brke2r1" {
		t.Fatalf("got %s", got)
	}
	want := "https://github.com/rancher/rke2/releases/download/v1.36.5%2Brke2r1/rke2-images.linux-arm64.tar.zst"
	a := AssetsFor("v1.36.5+rke2r1", "canal", Platform{OS: "linux", Arch: "arm64"})
	var found bool
	for _, as := range a {
		if as.URL == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected URL %s in assets %+v", want, a)
	}
}

func TestAssetsForCNI(t *testing.T) {
	p := Platform{OS: "linux", Arch: "amd64"}
	canal := AssetsFor("v1.36.5+rke2r1", "canal", p)
	var names []string
	for _, a := range canal {
		names = append(names, a.Name)
	}
	want := []string{"rke2.linux-amd64.tar.gz", "sha256sum-amd64.txt", "rke2-images.linux-amd64.tar.zst"}
	if fmt.Sprint(names) != fmt.Sprint(want) {
		t.Fatalf("canal assets = %v, want %v", names, want)
	}
	cilium := AssetsFor("v1.36.5+rke2r1", "cilium", p)
	var hasCore, hasCilium bool
	for _, a := range cilium {
		if a.Name == "rke2-images-core.linux-amd64.tar.zst" {
			hasCore = true
		}
		if a.Name == "rke2-images-cilium.linux-amd64.tar.zst" {
			hasCilium = true
		}
	}
	if !hasCore || !hasCilium {
		t.Fatalf("cilium assets missing core/cni archives: %+v", cilium)
	}
}

func TestParseSumFile(t *testing.T) {
	data := []byte("e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855  file.tar.gz\n" +
		"2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae  *other.txt\n\n")
	m, err := ParseSumFile(data)
	if err != nil {
		t.Fatal(err)
	}
	if m["file.tar.gz"] != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" ||
		m["other.txt"] != "2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae" {
		t.Fatalf("bad parse: %v", m)
	}
	if _, err := ParseSumFile([]byte("garbage line here\n")); err == nil {
		t.Fatal("malformed manifest should fail")
	}
}

func TestVerifyPlatform(t *testing.T) {
	dir := t.TempDir()
	p := Platform{OS: "linux", Arch: "amd64"}
	writeSum := func(entries map[string][]byte) {
		var body string
		for name, content := range entries {
			body += fmt.Sprintf("%x  %s\n", sha256.Sum256(content), name)
		}
		// include an upstream entry we did NOT download — must be ignored
		body += fmt.Sprintf("%x  %s\n", sha256.Sum256([]byte("x")), "rke2-images-multus.linux-amd64.tar.zst")
		if err := os.WriteFile(filepath.Join(dir, "sha256sum-amd64.txt"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string][]byte{
		"rke2.linux-amd64.tar.gz":            []byte("binary-tar"),
		"rke2-images.linux-amd64.tar.zst":    []byte("images-tar"),
	}
	for n, c := range files {
		if err := os.WriteFile(filepath.Join(dir, n), c, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeSum(files)
	if err := VerifyPlatform(dir, "canal", p); err != nil {
		t.Fatalf("verify should pass: %v", err)
	}

	// corrupt one file
	if err := os.WriteFile(filepath.Join(dir, "rke2.linux-amd64.tar.gz"), []byte("corrupted"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := VerifyPlatform(dir, "canal", p); err == nil {
		t.Fatal("corrupted file must fail verification")
	}
}

func TestBundleValidate(t *testing.T) {
	b := &Bundle{APIVersion: APIVersion, Kind: Kind, Spec: Spec{
		Version: "v1.36.5+rke2r1", Platforms: []string{"linux/amd64", "linux/arm64"},
	}}
	if err := b.Validate(); err != nil {
		t.Fatal(err)
	}
	if b.Spec.CNI != "canal" {
		t.Fatalf("default cni should be canal, got %s", b.Spec.CNI)
	}
	b.Spec.Platforms = append(b.Spec.Platforms, "linux/amd64")
	if err := b.Validate(); err == nil {
		t.Fatal("duplicate platform must fail")
	}
}
