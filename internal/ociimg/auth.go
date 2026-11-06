package ociimg

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
)

// Credentials are stored docker-compatible in ~/.docker/config.json so
// `docker login` and `searke login` are interchangeable — remote.*
// calls already use authn.DefaultKeychain which reads the same file.
// Plain-HTTP registries are recorded separately under
// ~/.searke/registries.json because docker keeps that policy in the
// daemon config, not the user config.

// dockerConfig keeps every auth entry as raw JSON so entries written
// by other tools (identitytoken, credHelpers results) survive a rewrite.
type dockerConfig struct {
	Auths map[string]json.RawMessage `json:"auths"`
}

func dockerConfigPath() (string, error) {
	if p := os.Getenv("DOCKER_CONFIG"); p != "" {
		return filepath.Join(p, "config.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".docker", "config.json"), nil
}

func registriesPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".searke", "registries.json"), nil
}

// Login stores registry credentials docker-compatibly and, when
// insecure is set, records the registry as plain-HTTP so every later
// searke command reaches it without TLS.
func Login(registry, user, pass string, insecure bool) error {
	registry = NormalizeRegistry(registry)
	if registry == "" {
		return fmt.Errorf("empty registry")
	}
	path, err := dockerConfigPath()
	if err != nil {
		return err
	}
	cfg := dockerConfig{Auths: map[string]json.RawMessage{}}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		if cfg.Auths == nil {
			cfg.Auths = map[string]json.RawMessage{}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	authJSON, err := json.Marshal(struct {
		Auth string `json:"auth"`
	}{Auth: base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))})
	if err != nil {
		return err
	}
	cfg.Auths[registry] = authJSON
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	if insecure {
		return SetInsecure(registry, true)
	}
	return nil
}

// Logout removes the stored credential for a registry.
func Logout(registry string) error {
	registry = NormalizeRegistry(registry)
	path, err := dockerConfigPath()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var cfg dockerConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if _, ok := cfg.Auths[registry]; !ok {
		return fmt.Errorf("no stored credential for %s", registry)
	}
	delete(cfg.Auths, registry)
	if len(cfg.Auths) == 0 {
		cfg.Auths = map[string]json.RawMessage{}
	}
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o600)
}

// NormalizeRegistry strips scheme and path so "https://host:5000/x"
// becomes "host:5000" — the same form docker uses as an auths key.
func NormalizeRegistry(registry string) string {
	r := strings.TrimSpace(registry)
	r = strings.TrimPrefix(r, "http://")
	r = strings.TrimPrefix(r, "https://")
	if i := strings.Index(r, "/"); i >= 0 {
		r = r[:i]
	}
	return strings.ToLower(r)
}

// SetInsecure records or clears a plain-HTTP registry marker.
func SetInsecure(registry string, insecure bool) error {
	registry = NormalizeRegistry(registry)
	path, err := registriesPath()
	if err != nil {
		return err
	}
	list, _ := readInsecureList(path)
	found := map[string]bool{}
	for _, r := range list {
		found[r] = true
	}
	if insecure {
		found[registry] = true
	} else {
		delete(found, registry)
	}
	var out []string
	for r := range found {
		out = append(out, r)
	}
	data, err := json.MarshalIndent(struct {
		Insecure []string `json:"insecure"`
	}{Insecure: out}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// IsInsecure reports whether a registry host was marked plain-HTTP,
// either by `searke login --insecure` or a previous flag.
func IsInsecure(registry string) bool {
	path, err := registriesPath()
	if err != nil {
		return false
	}
	list, _ := readInsecureList(path)
	registry = NormalizeRegistry(registry)
	for _, r := range list {
		if r == registry {
			return true
		}
	}
	return false
}

func readInsecureList(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var v struct {
		Insecure []string `json:"insecure"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	return v.Insecure, nil
}

// parseRef parses an OCI reference, honouring the insecure flag and
// any stored plain-HTTP marker for its registry.
func parseRef(refStr string, insecure bool) (name.Reference, error) {
	ref, err := name.ParseReference(refStr)
	if err != nil {
		return nil, err
	}
	if insecure || IsInsecure(ref.Context().RegistryStr()) {
		return name.ParseReference(refStr, name.Insecure)
	}
	return ref, nil
}
