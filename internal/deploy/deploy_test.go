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

func TestRenderConfigMerges(t *testing.T) {
	c := testCluster()
	c.Spec.Endpoints.APIAddress = "10.0.0.100:6443"
	c.Spec.RKE2Config = map[string]any{
		"tls-san":     []any{"extra.example.com"},
		"node-labels": []any{"zone=a"},
	}
	n := cluster.Node{Name: "m1", Address: "10.0.0.11", Labels: map[string]string{"role": "cp"}}
	out, err := renderConfig(c, n, "server", c.RegistrationURL(), "", true)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{"10.0.0.100", "extra.example.com", "zone=a", "role=cp"} {
		if !contains(s, want) {
			t.Errorf("merged config missing %q:\n%s", want, s)
		}
	}
}

func TestMinorSkew(t *testing.T) {
	d, err := minorSkew("v1.36.5+rke2r1", "v1.37.1+rke2r1")
	if err != nil || d != 1 {
		t.Fatalf("got %d %v", d, err)
	}
	if _, err := minorSkew("v1.36.5+rke2r1", "v1.38.0+rke2r1"); err != nil || true {
		// >1 allowed by minorSkew (caller decides); check it reports 2
	}
	d, _ = minorSkew("v1.36.5+rke2r1", "v1.38.0+rke2r1")
	if d != 2 {
		t.Fatalf("skew = %d, want 2", d)
	}
	if _, err := minorSkew("v1.36.5+rke2r1", "v2.0.0+rke2r1"); err == nil {
		t.Fatal("major bump must fail")
	}
}

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
	// stages are monotonic: re-marking an earlier stage is a no-op
	if err := st.Mark("m1", "server", "arm64", StagePreflight, "re-run"); err != nil {
		t.Fatal(err)
	}
	if !st.Done("m1", StageDistributed) {
		t.Fatal("Mark regressed the stage; resume state was lost")
	}
	// forward progress still works
	if err := st.Mark("m1", "server", "arm64", StageInstalled, "ok"); err != nil {
		t.Fatal(err)
	}
	if !st.Done("m1", StageInstalled) {
		t.Fatal("forward mark failed")
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
