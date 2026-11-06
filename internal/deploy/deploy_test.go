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
