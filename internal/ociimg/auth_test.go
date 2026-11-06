package ociimg

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// authRegistry serves /v2/ requiring basic auth u:p.
func authRegistry(t *testing.T) (host string, cleanup func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, _ := r.BasicAuth()
		if u != "u" || p != "p" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	return strings.TrimPrefix(srv.URL, "http://"), srv.Close
}

func authsFile(t *testing.T, dir string) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestLoginLogoutAndInsecure(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("DOCKER_CONFIG", tmp)
	t.Setenv("HOME", tmp)
	host, cleanup := authRegistry(t)
	defer cleanup()

	warns, err := Login(context.Background(), "https://"+host+"/ns", "u", "p", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings: %v", warns)
	}
	// docker-compatible config written under normalized host key
	doc := authsFile(t, tmp)
	var auths map[string]struct {
		Auth string `json:"auth"`
	}
	if err := json.Unmarshal(doc["auths"], &auths); err != nil {
		t.Fatal(err)
	}
	want := base64.StdEncoding.EncodeToString([]byte("u:p"))
	if auths[host].Auth != want {
		t.Fatalf("auths = %+v", auths)
	}
	if !IsInsecure(host) {
		t.Fatal("insecure marker not stored")
	}
	// ParseSource honours the stored marker
	s, err := ParseSource(host+"/x/y:1", false)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Insecure {
		t.Fatal("ParseSource did not pick up stored insecure marker")
	}
	if err := Logout(host); err != nil {
		t.Fatal(err)
	}
	doc = authsFile(t, tmp)
	var auths2 map[string]json.RawMessage
	if err := json.Unmarshal(doc["auths"], &auths2); err != nil {
		t.Fatal(err)
	}
	if _, ok := auths2[host]; ok {
		t.Fatal("credential survived logout")
	}
	if err := Logout(host); err == nil {
		t.Fatal("second logout should report missing credential")
	}
}

func TestLoginPreservesUnknownKeys(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("DOCKER_CONFIG", tmp)
	t.Setenv("HOME", tmp)
	host, cleanup := authRegistry(t)
	defer cleanup()

	seed := `{"auths":{"other.example.com":{"auth":"eA=="}},"currentContext":"colima","credsStore":"osxkeychain","proxies":{"default":{"httpProxy":"http://p:3128"}}}`
	if err := os.WriteFile(filepath.Join(tmp, "config.json"), []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	warns, err := Login(context.Background(), host, "u", "p", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(warns) == 0 {
		t.Fatal("credsStore must produce a warning")
	}
	doc := authsFile(t, tmp)
	for _, k := range []string{"currentContext", "credsStore", "proxies"} {
		if _, ok := doc[k]; !ok {
			t.Fatalf("key %q lost after login: %v", k, doc)
		}
	}
	var auths map[string]json.RawMessage
	if err := json.Unmarshal(doc["auths"], &auths); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"other.example.com", host} {
		if _, ok := auths[k]; !ok {
			t.Fatalf("auths entry %q lost: %v", k, auths)
		}
	}
}

func TestLoginBadCredentialRejected(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("DOCKER_CONFIG", tmp)
	t.Setenv("HOME", tmp)
	host, cleanup := authRegistry(t)
	defer cleanup()

	if _, err := Login(context.Background(), host, "u", "wrong", true); err == nil {
		t.Fatal("bad credential must fail at login")
	}
	if _, err := os.Stat(filepath.Join(tmp, "config.json")); !os.IsNotExist(err) {
		t.Fatal("config written despite rejected credential")
	}
}

func TestRegistryAuthKey(t *testing.T) {
	if got := registryAuthKey("docker.io"); got != dockerHubKey {
		t.Fatalf("docker.io key = %q", got)
	}
	if got := registryAuthKey("r.example.com:5000"); got != "r.example.com:5000" {
		t.Fatalf("plain registry key = %q", got)
	}
}

func TestPushRejectsEmpty(t *testing.T) {
	// sanity: ParseSource still rejects empty refs
	if _, err := ParseSource("", false); err == nil {
		t.Fatal("empty source accepted")
	}
}
