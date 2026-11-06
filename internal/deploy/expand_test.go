package deploy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const expandDoc = `apiVersion: searke.io/v1alpha1
kind: Cluster
metadata:
  name: t
spec:
  rke2:
    bundle: /tmp/nope
  ssh:
    user: ubuntu
  endpoints:
    registrationAddress: 10.0.0.1:9345
  servers:
    - { name: m1, address: 10.0.0.1 }
  agents:
    - { name: w1, address: 10.0.0.5 }
`

func writeExpandDoc(t *testing.T) (string, *Options) {
	t.Helper()
	f := filepath.Join(t.TempDir(), "cluster.yaml")
	if err := os.WriteFile(f, []byte(expandDoc), 0o600); err != nil {
		t.Fatal(err)
	}
	return f, &Options{ClusterFile: f, Log: func(string, ...any) {}}
}

func TestAddDuplicateRefused(t *testing.T) {
	f, opts := writeExpandDoc(t)
	before, _ := os.ReadFile(f)
	err := Add(context.Background(), opts, &AddOptions{Agents: []string{"10.0.0.5"}})
	if err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("expected duplicate refusal, got %v", err)
	}
	after, _ := os.ReadFile(f)
	if string(before) != string(after) {
		t.Fatal("descriptor modified on refused add")
	}
}

func TestAddKeepsOddServerCount(t *testing.T) {
	f, opts := writeExpandDoc(t)
	before, _ := os.ReadFile(f)
	err := Add(context.Background(), opts, &AddOptions{Servers: []string{"10.0.0.2"}})
	if err == nil || !strings.Contains(err.Error(), "odd") {
		t.Fatalf("expected odd-count refusal, got %v", err)
	}
	after, _ := os.ReadFile(f)
	if string(before) != string(after) {
		t.Fatal("descriptor modified on refused add")
	}
}

func TestUninstallNeedsConfirm(t *testing.T) {
	_, opts := writeExpandDoc(t)
	if err := Uninstall(context.Background(), opts, false, false); err == nil {
		t.Fatal("uninstall ran without confirmation")
	}
}
