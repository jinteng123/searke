package ociimg

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLoginLogoutAndInsecure(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("DOCKER_CONFIG", tmp)
	t.Setenv("HOME", tmp)

	if err := Login("https://Reg.Example.com:5000/ns", "u", "p", true); err != nil {
		t.Fatal(err)
	}
	// docker-compatible config written under normalized host key
	data, err := os.ReadFile(filepath.Join(tmp, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	want := base64.StdEncoding.EncodeToString([]byte("u:p"))
	if cfg.Auths["reg.example.com:5000"].Auth != want {
		t.Fatalf("auths = %+v", cfg.Auths)
	}
	if !IsInsecure("reg.example.com:5000") {
		t.Fatal("insecure marker not stored")
	}
	// ParseSource honours the stored marker
	s, err := ParseSource("reg.example.com:5000/x/y:1", false)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Insecure {
		t.Fatal("ParseSource did not pick up stored insecure marker")
	}
	if err := Logout("reg.example.com:5000"); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(tmp, "config.json"))
	var cfg2 struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	json.Unmarshal(data, &cfg2)
	if _, ok := cfg2.Auths["reg.example.com:5000"]; ok {
		t.Fatal("credential survived logout")
	}
	if err := Logout("reg.example.com:5000"); err == nil {
		t.Fatal("second logout should report missing credential")
	}
}
