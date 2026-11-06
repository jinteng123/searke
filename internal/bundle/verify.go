package bundle

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SumFile is a parsed sha256sum-<arch>.txt manifest: name -> hex digest.
type SumFile map[string]string

// ParseSumFile parses upstream sha256sum output. Lines look like
// "<64-hex>  <name>" or "<64-hex> *<name>".
func ParseSumFile(data []byte) (SumFile, error) {
	m := SumFile{}
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || len(fields[0]) != 64 {
			return nil, fmt.Errorf("sha256sum manifest: malformed line %d: %q", i+1, line)
		}
		name := strings.TrimPrefix(fields[1], "*")
		m[name] = strings.ToLower(fields[0])
	}
	if len(m) == 0 {
		return nil, fmt.Errorf("sha256sum manifest: no entries")
	}
	return m, nil
}

// VerifyPlatform checks a payload/linux-<arch> directory against its
// sha256sum-<arch>.txt. Only files the bundle requires are checked;
// extra manifest entries (optional upstream assets we did not fetch)
// are ignored, but a downloaded file with a wrong hash is always an
// error.
func VerifyPlatform(dir string, cni string, p Platform) error {
	sumPath := filepath.Join(dir, fmt.Sprintf("sha256sum-%s.txt", p.Arch))
	data, err := os.ReadFile(sumPath)
	if err != nil {
		return fmt.Errorf("%s: %w", p, err)
	}
	sums, err := ParseSumFile(data)
	if err != nil {
		return fmt.Errorf("%s: %w", p, err)
	}

	for _, name := range RequiredFiles(cni, p) {
		if name == filepath.Base(sumPath) {
			continue
		}
		want, ok := sums[name]
		if !ok {
			return fmt.Errorf("%s: upstream sha256 manifest has no entry for required file %s", p, name)
		}
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("%s: missing required file %s", p, name)
		}
		got, err := FileSHA256(path)
		if err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("%s: sha256 mismatch for %s (want %s, got %s)", p, name, want, got)
		}
	}
	return nil
}

// VerifyBundleDir verifies a complete bundle directory: descriptor,
// shared files and every declared platform payload.
func VerifyBundleDir(dir string) (*Bundle, error) {
	b, err := Load(filepath.Join(dir, FileName))
	if err != nil {
		return nil, err
	}
	cni, _ := NormalizeCNI(b.Spec.CNI)

	if fi, err := os.Stat(filepath.Join(dir, "install.sh")); err != nil || fi.Size() == 0 {
		return nil, fmt.Errorf("%s: missing or empty install.sh", dir)
	}
	platforms, err := b.PlatformList()
	if err != nil {
		return nil, err
	}
	for _, p := range platforms {
		if err := VerifyPlatform(filepath.Join(dir, "payload", p.DirName()), cni, p); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// Summarize returns the sorted list of files in a directory (for diagnostics).
func Summarize(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, nil
}
