package bundle

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	// APIVersion is the only schema version currently accepted.
	APIVersion = "searke.io/v1alpha1"
	// Kind is the bundle document kind.
	Kind = "RKE2Bundle"
	// LockKind marks the generated bundle.lock.yaml manifest.
	LockKind = "RKE2BundleLock"

	// FileName is the conventional bundle descriptor name.
	FileName = "bundle.yaml"
	// LockFileName is the generated lock manifest name.
	LockFileName = "bundle.lock.yaml"
)

// versionRe matches fully pinned RKE2 versions such as v1.36.5+rke2r1,
// including pre-release forms like v1.36.5-rc1+rke2r1. Floating refs
// (latest, stable) and partial versions are rejected.
var versionRe = regexp.MustCompile(`^v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?\+rke2r\d+$`)

// Platform is a normalized OS/arch pair.
type Platform struct {
	OS   string `yaml:"os" json:"os"`
	Arch string `yaml:"arch" json:"arch"`
}

func (p Platform) String() string { return p.OS + "/" + p.Arch }

// DirName is the payload subdirectory name for the platform
// (e.g. linux-amd64).
func (p Platform) DirName() string { return p.OS + "-" + p.Arch }

// ParsePlatform normalizes "linux/amd64" style strings.
func ParsePlatform(s string) (Platform, error) {
	var p Platform
	parts := strings.Split(s, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return p, fmt.Errorf("invalid platform %q, expected os/arch", s)
	}
	p.OS, p.Arch = parts[0], parts[1]
	if p.OS != "linux" {
		return p, fmt.Errorf("unsupported platform OS %q: only linux is supported", p.OS)
	}
	switch p.Arch {
	case "amd64", "arm64":
	default:
		return p, fmt.Errorf("unsupported architecture %q: supported: amd64, arm64", p.Arch)
	}
	return p, nil
}

// Spec is the bundle.yaml spec section.
type Spec struct {
	Version   string   `yaml:"version" json:"version"`
	CNI       string   `yaml:"cni" json:"cni"`
	Platforms []string `yaml:"platforms" json:"platforms"`
}

// Bundle is the parsed bundle.yaml document.
type Bundle struct {
	APIVersion string `yaml:"apiVersion" json:"apiVersion"`
	Kind       string `yaml:"kind" json:"kind"`
	Spec       Spec   `yaml:"spec" json:"spec"`
}

// Load parses and validates a bundle.yaml file.
func Load(path string) (*Bundle, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var b Bundle
	if err := yaml.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := b.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &b, nil
}

// Validate enforces the bundle schema rules.
func (b *Bundle) Validate() error {
	if b.APIVersion != APIVersion {
		return fmt.Errorf("unsupported apiVersion %q (want %q)", b.APIVersion, APIVersion)
	}
	if b.Kind != Kind {
		return fmt.Errorf("unsupported kind %q (want %q)", b.Kind, Kind)
	}
	if err := ValidateVersion(b.Spec.Version); err != nil {
		return err
	}
	if b.Spec.CNI == "" {
		b.Spec.CNI = "canal"
	}
	if _, err := NormalizeCNI(b.Spec.CNI); err != nil {
		return err
	}
	if len(b.Spec.Platforms) == 0 {
		return fmt.Errorf("spec.platforms must list at least one platform")
	}
	seen := map[string]bool{}
	for _, s := range b.Spec.Platforms {
		p, err := ParsePlatform(s)
		if err != nil {
			return err
		}
		if seen[p.String()] {
			return fmt.Errorf("duplicate platform %q", s)
		}
		seen[p.String()] = true
	}
	return nil
}

// ValidateVersion rejects unpinned or malformed versions.
func ValidateVersion(v string) error {
	if v == "" {
		return fmt.Errorf("spec.version is required (e.g. v1.36.5+rke2r1); floating versions like latest/stable are not allowed")
	}
	if !versionRe.MatchString(v) {
		return fmt.Errorf("version %q is not a fully pinned RKE2 version (want vX.Y.Z+rke2rN)", v)
	}
	return nil
}

// NormalizeCNI validates and lowercases the CNI name.
func NormalizeCNI(cni string) (string, error) {
	switch cni {
	case "canal", "calico", "cilium", "flannel", "none":
		return cni, nil
	default:
		return "", fmt.Errorf("unsupported cni %q: supported: canal, calico, cilium, flannel, none", cni)
	}
}

// PlatformList returns parsed, sorted platforms.
func (b *Bundle) PlatformList() ([]Platform, error) {
	out := make([]Platform, 0, len(b.Spec.Platforms))
	for _, s := range b.Spec.Platforms {
		p, err := ParsePlatform(s)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Arch < out[j].Arch })
	return out, nil
}

// LockAsset records one fetched artifact in the lock manifest.
type LockAsset struct {
	Name     string `yaml:"name" json:"name"`
	Platform string `yaml:"platform,omitempty" json:"platform,omitempty"`
	URL      string `yaml:"url" json:"url"`
	Size     int64  `yaml:"size" json:"size"`
	SHA256   string `yaml:"sha256" json:"sha256"`
}

// Lock is the generated bundle.lock.yaml content.
type Lock struct {
	APIVersion string      `yaml:"apiVersion" json:"apiVersion"`
	Kind       string      `yaml:"kind" json:"kind"`
	Spec       LockSpec    `yaml:"spec" json:"spec"`
}

// LockSpec records the exact inputs used to build a bundle.
type LockSpec struct {
	Version string      `yaml:"version" json:"version"`
	CNI     string      `yaml:"cni" json:"cni"`
	Source  string      `yaml:"source" json:"source"`
	Assets  []LockAsset `yaml:"assets" json:"assets"`
}

// WriteLock serializes the lock manifest into the bundle directory.
func WriteLock(dir string, lock *Lock) error {
	data, err := yaml.Marshal(lock)
	if err != nil {
		return err
	}
	return os.WriteFile(dir+"/"+LockFileName, data, 0o644)
}

// LoadLock reads a bundle.lock.yaml.
func LoadLock(dir string) (*Lock, error) {
	data, err := os.ReadFile(dir + "/" + LockFileName)
	if err != nil {
		return nil, err
	}
	var l Lock
	if err := yaml.Unmarshal(data, &l); err != nil {
		return nil, err
	}
	return &l, nil
}
