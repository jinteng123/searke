package ociimg

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

// Credentials are stored docker-compatible in ~/.docker/config.json so
// `docker login` and `searke login` are interchangeable — remote.*
// calls already use authn.DefaultKeychain which reads the same file.
// Plain-HTTP registries are recorded separately under
// ~/.searke/registries.json because docker keeps that policy in the
// daemon config, not the user config.
//
// The docker config is edited as a map of raw entries so settings the
// CLI does not know about (currentContext, proxies, credsStore) survive
// a rewrite; writes are atomic via temp-file + rename.

// dockerHubKey is the auths key authn.DefaultKeychain looks up for the
// Docker Hub; docker login stores credentials under it too.
const dockerHubKey = "https://index.docker.io/v1/"

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

// registryAuthKey maps a normalized registry to the auths key both
// docker and the go-containerregistry keychain look up.
func registryAuthKey(registry string) string {
	switch registry {
	case "docker.io", "index.docker.io", "registry-1.docker.io":
		return dockerHubKey
	}
	return registry
}

func loadDockerConfig(path string) (map[string]json.RawMessage, error) {
	doc := map[string]json.RawMessage{}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return doc, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return doc, nil
}

func authsOf(doc map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	auths := map[string]json.RawMessage{}
	if raw, ok := doc["auths"]; ok {
		if err := json.Unmarshal(raw, &auths); err != nil {
			return nil, fmt.Errorf("parse auths: %w", err)
		}
	}
	return auths, nil
}

func saveAuths(doc map[string]json.RawMessage, auths map[string]json.RawMessage) error {
	raw, err := json.Marshal(auths)
	if err != nil {
		return err
	}
	doc["auths"] = raw
	return nil
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err == nil {
		err = f.Close()
	} else {
		f.Close()
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// credStoreWarnings reports when the config defers to a credential
// helper (credsStore or credHelpers[registry]): authn.DefaultKeychain
// then ignores the plain auths entry searke just wrote.
func credStoreWarnings(doc map[string]json.RawMessage, registry string) []string {
	var warns []string
	if raw, ok := doc["credsStore"]; ok {
		var store string
		if json.Unmarshal(raw, &store) == nil && store != "" {
			warns = append(warns, fmt.Sprintf(
				"credsStore %q is configured: the keychain ignores auths entries; "+
					"if pushes fail with 401, use `docker login` so the helper stores the credential", store))
		}
	}
	if raw, ok := doc["credHelpers"]; ok {
		var helpers map[string]string
		if json.Unmarshal(raw, &helpers) == nil {
			if _, ok := helpers[registry]; ok {
				warns = append(warns, fmt.Sprintf(
					"a credHelper is configured for %s: the keychain ignores the stored auths entry", registry))
			}
		}
	}
	return warns
}

// checkCredential makes one authenticated /v2/ request so a wrong
// password fails at login instead of surfacing as a 401 at push time.
// Registries that allow anonymous pulls can still answer 200 here —
// the push-scope token exchange covers those.
func checkCredential(ctx context.Context, registry, user, pass string, insecure bool) error {
	var opts []name.Option
	if insecure || IsInsecure(registry) {
		opts = append(opts, name.Insecure)
	}
	reg, err := name.NewRegistry(registry, opts...)
	if err != nil {
		return err
	}
	auth := &authn.Basic{Username: user, Password: pass}
	tr, err := transport.NewWithContext(ctx, reg, auth, http.DefaultTransport,
		[]string{reg.Scope("pull")})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		reg.Scheme()+"://"+reg.RegistryStr()+"/v2/", nil)
	if err != nil {
		return err
	}
	resp, err := tr.RoundTrip(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized:
		return fmt.Errorf("registry rejected the credential for %s@%s", user, registry)
	default:
		return fmt.Errorf("credential check: registry answered %s", resp.Status)
	}
}

// Login verifies the credential, then stores it docker-compatibly. When
// insecure is set the registry is also recorded as plain-HTTP so every
// later searke command reaches it without TLS. Warnings are returned
// for cases where the stored entry will not be honoured.
func Login(ctx context.Context, registry, user, pass string, insecure bool) ([]string, error) {
	registry = NormalizeRegistry(registry)
	if registry == "" {
		return nil, fmt.Errorf("empty registry")
	}
	if err := checkCredential(ctx, registry, user, pass, insecure); err != nil {
		return nil, err
	}
	path, err := dockerConfigPath()
	if err != nil {
		return nil, err
	}
	doc, err := loadDockerConfig(path)
	if err != nil {
		return nil, err
	}
	auths, err := authsOf(doc)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	authJSON, err := json.Marshal(struct {
		Auth string `json:"auth"`
	}{Auth: base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))})
	if err != nil {
		return nil, err
	}
	auths[registryAuthKey(registry)] = authJSON
	if err := saveAuths(doc, auths); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomic(path, data, 0o600); err != nil {
		return nil, err
	}
	warns := credStoreWarnings(doc, registry)
	if insecure {
		return warns, SetInsecure(registry, true)
	}
	return warns, nil
}

// Logout removes the stored credential for a registry.
func Logout(registry string) error {
	registry = NormalizeRegistry(registry)
	path, err := dockerConfigPath()
	if err != nil {
		return err
	}
	doc, err := loadDockerConfig(path)
	if err != nil {
		return err
	}
	auths, err := authsOf(doc)
	if err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	key := registryAuthKey(registry)
	if _, ok := auths[key]; !ok {
		return fmt.Errorf("no stored credential for %s (a credStore/helper may hold it instead)", registry)
	}
	delete(auths, key)
	if err := saveAuths(doc, auths); err != nil {
		return err
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, out, 0o600)
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
	list, err := readInsecureList(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	found := map[string]bool{}
	for _, r := range list {
		found[r] = true
	}
	if insecure {
		found[registry] = true
	} else {
		delete(found, registry)
	}
	out := []string{}
	for r := range found {
		out = append(out, r)
	}
	sort.Strings(out)
	data, err := json.MarshalIndent(struct {
		Insecure []string `json:"insecure"`
	}{Insecure: out}, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, data, 0o600)
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
