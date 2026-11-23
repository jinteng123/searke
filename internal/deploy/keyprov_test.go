package deploy

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureManagedKey(t *testing.T) {
	dir := t.TempDir()
	priv, pub, err := ensureManagedKey(dir, "test-cluster")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(priv, filepath.Join(".searke", "id_ed25519")) {
		t.Errorf("unexpected key path %s", priv)
	}
	if !strings.HasPrefix(pub, "ssh-ed25519 ") || !strings.HasSuffix(pub, " searke-test-cluster") {
		t.Errorf("bad authorized_keys line %q", pub)
	}
	fi, err := os.Stat(priv)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("private key mode = %o, want 600", fi.Mode().Perm())
	}
	// Second call loads the same key rather than regenerating.
	priv2, pub2, err := ensureManagedKey(dir, "test-cluster")
	if err != nil {
		t.Fatal(err)
	}
	if priv2 != priv || pub2 != pub {
		t.Error("ensureManagedKey must be idempotent")
	}
}

func TestPubLineFromIdentity(t *testing.T) {
	dir := t.TempDir()
	priv, pub, err := ensureManagedKey(dir, "c")
	if err != nil {
		t.Fatal(err)
	}
	got, err := pubLineFromIdentity(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSuffix(pub, " searke-c") != got {
		t.Errorf("derived pub %q does not match generated %q", got, pub)
	}
	withComment, err := pubLineFromIdentity(priv, "searke-c")
	if err != nil {
		t.Fatal(err)
	}
	if withComment != pub {
		t.Errorf("pub with comment = %q, want %q", withComment, pub)
	}
	if _, err := pubLineFromIdentity(filepath.Join(dir, "missing"), ""); err == nil {
		t.Error("missing key must error")
	}
}

func TestAgentNameFromHostname(t *testing.T) {
	taken := func() map[string]bool {
		return map[string]bool{"server-01": true, "worker-a": true}
	}
	cases := []struct {
		host, gen, want, why string
	}{
		{"Worker-X", "agent-01", "worker-x", ""},
		{"web-01.local", "agent-01", "web-01.local", ""},
		{"worker-a", "agent-01", "agent-01", "already a node name"},
		{"localhost", "agent-01", "agent-01", "not usable"},
		{"", "agent-01", "agent-01", "not usable"},
	}
	for _, tc := range cases {
		got, why := agentNameFromHostname(tc.host, tc.gen, taken())
		if got != tc.want {
			t.Errorf("agentNameFromHostname(%q) = %q, want %q", tc.host, got, tc.want)
		}
		if tc.why != "" && !strings.Contains(why, tc.why) {
			t.Errorf("agentNameFromHostname(%q) reason %q, want contains %q", tc.host, why, tc.why)
		}
	}
	// A hostname needing sanitization still wins over the generated name.
	if got, _ := agentNameFromHostname("Bad_Name!!", "agent-01", taken()); got == "agent-01" {
		t.Error("hostname Bad_Name!! should sanitize to a usable name")
	}
}

func TestIsAuthErr(t *testing.T) {
	for _, s := range []string{
		"ssh dial 1.2.3.4:22: ssh: handshake failed: ssh: unable to authenticate, attempted methods [none password]",
		"no SSH credentials: set spec.ssh.identityFile",
	} {
		if !isAuthErr(errors.New(s)) {
			t.Errorf("isAuthErr(%q) = false", s)
		}
	}
	for _, s := range []string{
		"ssh dial 1.2.3.4:22: dial tcp: i/o timeout",
		"ssh dial: connection refused",
	} {
		if isAuthErr(errors.New(s)) {
			t.Errorf("isAuthErr(%q) = true", s)
		}
	}
}

func TestProvisionKey(t *testing.T) {
	var logs []string
	opts := &Options{Log: func(f string, a ...any) { logs = append(logs, f) }}
	provisionKey(&fakeExecutor{}, "n1", "ssh-ed25519 AAAA searke-c", opts)
	found := false
	for _, l := range logs {
		if strings.Contains(l, "authorized cluster SSH key") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected authorized log, got %v", logs)
	}
	logs = nil
	provisionKey(&fakeExecutor{fail: []string{"grep"}}, "n1", "ssh-ed25519 AAAA", opts)
	found = false
	for _, l := range logs {
		if strings.Contains(l, "install cluster SSH key failed") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected failure warning, got %v", logs)
	}
}
