package deploy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeDeleteCluster renders a valid 3-server/2-agent descriptor.
func writeDeleteCluster(t *testing.T) string {
	t.Helper()
	yaml := `apiVersion: searke.io/v1alpha1
kind: Cluster
metadata:
  name: deltest
spec:
  rke2:
    version: v1.36.5+rke2r1
    cni: canal
    bundle: /tmp/nothing
  endpoints:
    registrationAddress: 10.0.0.11:9345
  ssh:
    user: root
  servers:
    - { name: m1, address: 10.0.0.11 }
    - { name: m2, address: 10.0.0.12 }
    - { name: m3, address: 10.0.0.13 }
  agents:
    - { name: w1, address: 10.0.1.1 }
    - { name: w2, address: 10.0.1.2 }
`
	p := filepath.Join(t.TempDir(), "cluster.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDeleteGuards(t *testing.T) {
	cases := []struct {
		name    string
		del     DeleteOptions
		wantErr string
	}{
		{"nothing", DeleteOptions{DryRun: true}, "nothing to delete"},
		{"bootstrap", DeleteOptions{DryRun: true, Servers: []string{"m1"}}, "bootstrap server"},
		{"registration", DeleteOptions{DryRun: true, Servers: []string{"10.0.0.11"}}, "bootstrap server"},
		{"all-servers", DeleteOptions{DryRun: true, Servers: []string{"m1", "m2", "m3"}}, "bootstrap server"},
		{"even-result", DeleteOptions{DryRun: true, Servers: []string{"m3"}}, "odd server count"},
		{"wrong-role", DeleteOptions{DryRun: true, Servers: []string{"w1"}}, "agent node"},
		{"unknown", DeleteOptions{DryRun: true, Agents: []string{"ghost"}}, "not a agent"},
		{"need-yes", DeleteOptions{Agents: []string{"w1"}}, "--yes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Delete(context.Background(), &Options{ClusterFile: writeDeleteCluster(t)}, tc.del)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestDeleteDryRun(t *testing.T) {
	var logged []string
	err := Delete(context.Background(), &Options{
		ClusterFile: writeDeleteCluster(t),
		Log:         func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) },
	}, DeleteOptions{DryRun: true, Servers: []string{"m2", "m3"}, Agents: []string{"w1"}})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(logged, "\n")
	for _, want := range []string{"dry-run", "w1,m2,m3", "remaining servers: 1"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("log missing %q:\n%s", want, joined)
		}
	}
}
