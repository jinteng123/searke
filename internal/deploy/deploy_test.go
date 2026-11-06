package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jint/searke/internal/cluster"
)

func testCluster() *cluster.Cluster {
	c := &cluster.Cluster{APIVersion: cluster.APIVersion, Kind: cluster.Kind}
	c.Metadata.Name = "t"
	c.Spec.Endpoints.RegistrationAddress = "10.0.0.100:9345"
	c.Spec.RKE2Config = map[string]any{"cluster-cidr": "10.42.0.0/16"}
	c.Spec.Servers = []cluster.Node{{Name: "m1", Address: "10.0.0.11"}}
	return c
}

func TestRenderConfigFirstServer(t *testing.T) {
	c := testCluster()
	out, err := renderConfig(c, c.Spec.Servers[0], "server", c.RegistrationURL(), "", true)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{"node-name: m1", "cluster-cidr", "tls-san", "10.0.0.100", "cni: canal", "write-kubeconfig-mode"} {
		if !contains(s, want) {
			t.Errorf("config missing %q:\n%s", want, s)
		}
	}
	if contains(s, "server:") || contains(s, "token:") {
		t.Errorf("first server config must not contain server/token:\n%s", s)
	}
}

func TestRenderConfigJoinNode(t *testing.T) {
	c := testCluster()
	n := cluster.Node{Name: "w1", Address: "10.0.0.21", Labels: map[string]string{"role": "gpu"}}
	out, err := renderConfig(c, n, "agent", c.RegistrationURL(), "SECRETOKEN", false)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{"server: https://10.0.0.100:9345", "token: SECRETOKEN", "node-name: w1", "role=gpu"} {
		if !contains(s, want) {
			t.Errorf("join config missing %q:\n%s", want, s)
		}
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func TestStateStages(t *testing.T) {
	dir := t.TempDir()
	st, err := LoadState(StatePath(dir, "t"), "t", "./b.oci", "v1.36.5+rke2r1", "canal")
	if err != nil {
		t.Fatal(err)
	}
	if st.Done("m1", StageInstalled) {
		t.Fatal("fresh state should have nothing done")
	}
	if err := st.Mark("m1", "server", "arm64", StageDistributed, "ok"); err != nil {
		t.Fatal(err)
	}
	if !st.Done("m1", StagePreflight) || !st.Done("m1", StageDistributed) {
		t.Fatal("stage ordering broken")
	}
	if st.Done("m1", StageConfigured) {
		t.Fatal("configured should not be done yet")
	}
	// reload persists
	st2, err := LoadState(StatePath(dir, "t"), "t", "./b.oci", "v1.36.5+rke2r1", "canal")
	if err != nil {
		t.Fatal(err)
	}
	if !st2.Done("m1", StageDistributed) {
		t.Fatal("state not persisted")
	}
	// version drift rejected
	if _, err := LoadState(StatePath(dir, "t"), "t", "./b.oci", "v1.99.0+rke2r1", "canal"); err == nil {
		t.Fatal("version drift must fail")
	}
	// state file permissions
	fi, err := os.Stat(filepath.Join(dir, ".searke", "state-t.json"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode = %o, want 600", fi.Mode().Perm())
	}
}
