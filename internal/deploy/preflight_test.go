package deploy

import (
	"os"
	"strings"
	"testing"
)

// fakeExecutor answers Preflight probes from a substring table.
type fakeExecutor struct {
	outs map[string]string // substring → stdout (exit 0)
	fail []string          // substring → exit 1, no output
}

func (f *fakeExecutor) resp(cmd string) (*Result, error) {
	for k, v := range f.outs {
		if strings.Contains(cmd, k) {
			return &Result{Stdout: v}, nil
		}
	}
	for _, m := range f.fail {
		if strings.Contains(cmd, m) {
			return &Result{ExitCode: 1}, nil
		}
	}
	return &Result{}, nil
}

func (f *fakeExecutor) Run(cmd string) (*Result, error)     { return f.resp(cmd) }
func (f *fakeExecutor) RunSudo(cmd string) (*Result, error) { return f.resp(cmd) }
func (f *fakeExecutor) RunStdin(cmd string, _ []byte) (*Result, error) {
	return f.resp(cmd)
}
func (f *fakeExecutor) PrepareStaging() error                             { return nil }
func (f *fakeExecutor) UploadFile(_, _ string, _ os.FileMode) error       { return nil }
func (f *fakeExecutor) DownloadFile(_, _ string) error                    { return nil }
func (f *fakeExecutor) WriteFile(_ string, _ []byte, _ os.FileMode) error { return nil }
func (f *fakeExecutor) Close() error                                      { return nil }
func (f *fakeExecutor) Addr() string                                      { return "fake" }

// healthyNode scripts a fresh Ubuntu 22.04 amd64 node: no rke2, no
// SELinux, no firewalld/NM, iptables present, default route present.
func healthyNode() *fakeExecutor {
	return &fakeExecutor{
		outs: map[string]string{
			"uname -s":      "Linux\nx86_64",
			"os-release":    "ubuntu 22.04",
			"df -P":         "dev 100000000 1 90000000 1% /var/lib/rancher",
			"ip route show": "default via 10.0.0.1 dev eth0",
		},
		fail: []string{
			"rke2 && echo yes", // no /var/lib/rancher/rke2 dir
			"config.yaml && cat",
			"is-enabled",
			"rke2 --version",
			"getenforce", // command absent
			"is-active",
		},
	}
}

func hasWarning(rep *PreflightReport, sub string) bool {
	for _, w := range rep.Warnings {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

func TestPreflightHealthyNode(t *testing.T) {
	rep, err := Preflight(healthyNode(), "n1")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Arch != "amd64" || rep.OS != "ubuntu" || rep.OSVersion != "22.04" {
		t.Fatalf("bad report: %+v", rep)
	}
	if len(rep.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", rep.Warnings)
	}
}

func TestPreflightCgroupV1AndSwapWarn(t *testing.T) {
	ex := healthyNode()
	ex.fail = append(ex.fail, "cgroup.controllers")
	ex.outs["/proc/swaps"] = "Filename\tType\tSize\n/dev/zram0\tpartition\t1048576"
	rep, err := Preflight(ex, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if rep.CgroupV2 {
		t.Fatal("cgroup v1 host must report CgroupV2=false")
	}
	if !hasWarning(rep, "cgroup v1") || !hasWarning(rep, "swap") {
		t.Fatalf("missing cgroup/swap warnings: %v", rep.Warnings)
	}
}

func TestPreflightNonUbuntuWarns(t *testing.T) {
	ex := healthyNode()
	ex.outs["os-release"] = "alinux 3"
	rep, err := Preflight(ex, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if !hasWarning(rep, "not in the validated set") {
		t.Fatalf("missing distro warning: %v", rep.Warnings)
	}
}

func TestPreflightSELinuxEnforcingRefused(t *testing.T) {
	ex := healthyNode()
	ex.outs["getenforce"] = "Enforcing"
	ex.fail = nil // outs wins anyway; keep table clean
	if _, err := Preflight(ex, "n1"); err == nil ||
		!strings.Contains(err.Error(), "SELinux") {
		t.Fatalf("Enforcing must be refused, got %v", err)
	}
}

func TestPreflightFirewalldAndNMWarn(t *testing.T) {
	ex := healthyNode()
	ex.outs["is-active firewalld"] = "active"
	ex.outs["is-active NetworkManager"] = "active"
	ex.fail = []string{
		"rke2 && echo yes", "config.yaml && cat", "is-enabled", "rke2 --version", "getenforce",
		"rke2-canal.conf", // no NetworkManager exemption file
	}
	rep, err := Preflight(ex, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if !hasWarning(rep, "firewalld") || !hasWarning(rep, "NetworkManager") {
		t.Fatalf("missing firewall/NM warnings: %v", rep.Warnings)
	}

	// rke2-canal.conf present suppresses the NM warning
	ex2 := healthyNode()
	ex2.outs["is-active NetworkManager"] = "active"
	ex2.outs["rke2-canal.conf"] = ""
	ex2.fail = []string{
		"rke2 && echo yes", "config.yaml && cat", "is-enabled", "rke2 --version", "getenforce",
		"firewalld",
	}
	rep, err = Preflight(ex2, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if hasWarning(rep, "NetworkManager") {
		t.Fatalf("NM warning not suppressed by conf: %v", rep.Warnings)
	}
}
