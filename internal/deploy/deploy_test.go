package deploy

import (
	"fmt"
	"net"
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

func TestServerPrefixKeepsBootstrap(t *testing.T) {
	for _, first := range []string{"m1", "master", "server-0", "srv-a", "zeta", "server-1", "server-10"} {
		p := serverPrefix(first)
		// Mirrors Add's nextName: the first free "<prefix>-N".
		cand := p + "-1"
		for i := 2; cand == first; i++ {
			cand = fmt.Sprintf("%s-%d", p, i)
		}
		if cand < first {
			t.Errorf("first=%s: added name %s sorts before bootstrap", first, cand)
		}
	}
}

func TestCheckSnapshotName(t *testing.T) {
	for _, ok := range []string{"searke-20261108-013752", "pre-upgrade.db"} {
		if err := checkSnapshotName(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", ".", "..", "../x", "a/b", `a\b`} {
		if err := checkSnapshotName(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestCheckUpgradePath(t *testing.T) {
	ok := [][2]string{
		{"v1.36.5+rke2r1", "v1.36.6+rke2r1"}, // patch bump
		{"v1.36.5+rke2r1", "v1.36.5+rke2r2"}, // revision bump
		{"v1.36.5+rke2r1", "v1.37.0+rke2r1"}, // one minor
		{"v1.36.5+rke2r1", "v1.37.1+rke2r1"},
	}
	for _, tt := range ok {
		if err := checkUpgradePath(tt[0], tt[1]); err != nil {
			t.Errorf("%s -> %s: %v", tt[0], tt[1], err)
		}
	}
	bad := [][2]string{
		{"v1.36.6+rke2r1", "v1.36.5+rke2r1"}, // patch downgrade
		{"v1.37.0+rke2r1", "v1.36.9+rke2r1"}, // minor downgrade
		{"v1.36.5+rke2r1", "v1.38.0+rke2r1"}, // skips a minor
		{"v1.36.5+rke2r1", "v2.0.0+rke2r1"},  // major bump
	}
	for _, tt := range bad {
		if err := checkUpgradePath(tt[0], tt[1]); err == nil {
			t.Errorf("%s -> %s: expected error", tt[0], tt[1])
		}
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

func TestSingleNodeCluster(t *testing.T) {
	home := t.TempDir()
	c, file, err := SingleNodeCluster(RunOptions{Bundle: "./x.oci", HomeDir: home})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Spec.Servers) != 1 || len(c.Spec.Agents) != 0 {
		t.Fatalf("want exactly one server, got %+v", c.Spec.Servers)
	}
	n := c.Spec.Servers[0]
	if !cluster.IsLocalAddress(n.Address) {
		t.Fatalf("node address %s must be local", n.Address)
	}
	if c.Spec.Endpoints.RegistrationAddress == "" || c.Spec.Endpoints.APIAddress == "" {
		t.Fatal("endpoints not populated")
	}
	if c.Spec.RKE2Config["cluster-cidr"] != "10.42.0.0/16" {
		t.Fatal("default cluster-cidr missing")
	}
	// the emitted file loads back to an equivalent cluster
	loaded, err := cluster.Load(file)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Name() != c.Name() || loaded.Spec.RKE2.Bundle != c.Spec.RKE2.Bundle ||
		loaded.Spec.Servers[0].Name != n.Name {
		t.Fatalf("reloaded cluster differs: %+v", loaded.Spec)
	}
}

func TestSingleNodeClusterRerun(t *testing.T) {
	home := t.TempDir()
	c, file, err := SingleNodeCluster(RunOptions{Bundle: "./x.oci", HomeDir: home})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(file)

	// rerun with the same bundle reuses the file untouched
	c2, _, err := SingleNodeCluster(RunOptions{Bundle: "./x.oci", HomeDir: home})
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(file)
	if string(before) != string(after) || c2.Spec.RKE2.Bundle != "./x.oci" {
		t.Fatal("rerun must reuse the generated file unchanged")
	}
	if c2.Name() != c.Name() {
		t.Fatal("rerun returned a different cluster")
	}

	// a new bundle is the one allowed change: file gets updated
	c3, _, err := SingleNodeCluster(RunOptions{Bundle: "./y.oci", HomeDir: home})
	if err != nil {
		t.Fatal(err)
	}
	if c3.Spec.RKE2.Bundle != "./y.oci" {
		t.Fatal("rerun with a new bundle must update the file")
	}
	reloaded, _ := cluster.Load(file)
	if reloaded.Spec.RKE2.Bundle != "./y.oci" {
		t.Fatal("file was not updated to the new bundle")
	}

	// a conflicting flag is rejected rather than silently applied
	if _, _, err := SingleNodeCluster(RunOptions{Bundle: "./y.oci", HomeDir: home, ClusterCIDR: "10.99.0.0/16"}); err == nil {
		t.Fatal("conflicting --cluster-cidr on rerun must fail")
	}
	if _, _, err := SingleNodeCluster(RunOptions{Bundle: "./y.oci", HomeDir: home, Address: "10.9.9.9"}); err == nil {
		t.Fatal("conflicting --address on rerun must fail")
	}
	// matching flags still pass
	if _, _, err := SingleNodeCluster(RunOptions{Bundle: "./y.oci", HomeDir: home, ClusterCIDR: "10.42.0.0/16"}); err != nil {
		t.Fatalf("matching --cluster-cidr must pass: %v", err)
	}
}

func TestSanitizeAndPrimaryIP(t *testing.T) {
	if got := cluster.SanitizeNodeName("My_Server.Host"); got != "my-server.host" {
		t.Fatalf("sanitize = %q", got)
	}
	if got := cluster.SanitizeNodeName("___"); got != "node" {
		t.Fatalf("sanitize empty = %q", got)
	}
	ip := cluster.PrimaryIPv4()
	if net.ParseIP(ip) == nil {
		t.Fatalf("PrimaryIPv4 = %q", ip)
	}
}

func TestConnectLocalReturnsLocalExecutor(t *testing.T) {
	ex, err := Connect("127.0.0.1", SSHConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ex.(*LocalExecutor); !ok {
		t.Fatalf("127.0.0.1 must return *LocalExecutor, got %T", ex)
	}
	defer ex.Close()
}

func TestLocalExecutorCommands(t *testing.T) {
	ex, err := NewLocalExecutor("127.0.0.1", SSHConfig{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := ex.Run("printf hello && printf oops >&2")
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || res.Stdout != "hello" || res.Stderr != "oops" {
		t.Fatalf("got %+v", res)
	}
	// non-zero exit is reported, not returned as error
	res, err = ex.Run("printf still-ran; exit 3")
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 || res.Stdout != "still-ran" {
		t.Fatalf("got %+v", res)
	}
	// stdin feed reaches the command
	res, err = ex.RunStdin("cat", []byte("piped"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "piped" {
		t.Fatalf("stdin not fed: %+v", res)
	}
	// RunSudo: as root it runs directly; as non-root without sudo
	// configured it runs unprivileged — either way the command works.
	if res, err = ex.RunSudo("printf ok"); err != nil || res.ExitCode != 0 {
		t.Fatalf("RunSudo: %+v %v", res, err)
	}
}

func TestLocalExecutorFiles(t *testing.T) {
	ex, err := NewLocalExecutor("127.0.0.1", SSHConfig{})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()

	// UploadFile copies into place and applies the mode (non-sudo path).
	src := filepath.Join(dir, "src.bin")
	if err := os.WriteFile(src, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "deep", "nested", "dst.bin")
	if err := ex.UploadFile(src, dst, 0o640); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "payload" {
		t.Fatalf("content = %q", data)
	}
	if fi, _ := os.Stat(dst); fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %o", fi.Mode().Perm())
	}

	// WriteFile puts content at a path; callers create the parent dir
	// first, matching the remote flow (mkdir /etc/rancher/rke2).
	wf := filepath.Join(dir, "conf", "cfg.yaml")
	if err := os.MkdirAll(filepath.Dir(wf), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ex.WriteFile(wf, []byte("a: b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(wf); string(data) != "a: b\n" {
		t.Fatalf("write content = %q", data)
	}

	// DownloadFile copies a readable file to a local path.
	dl := filepath.Join(dir, "dl.bin")
	if err := ex.DownloadFile(src, dl); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(dl); string(data) != "payload" {
		t.Fatalf("downloaded = %q", data)
	}
}
