package deploy

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/jint/searke/internal/bundle"
)

// PreflightReport is the collected remote state of one node.
type PreflightReport struct {
	Hostname         string
	Arch             string // normalized: amd64 | arm64
	OS               string // distro id
	OSVersion        string
	HasRKE2          bool   // /var/lib/rancher/rke2 exists
	Service          string // rke2-server | rke2-agent | ""
	ServiceState     string // systemd is-active output
	ConfigExists     bool
	ExistingURL      string // server: line in existing config.yaml, if any
	InstalledVersion string // rke2 --version, if installed
	DiskAvailKiB     int64  // on the /var/lib filesystem
	HasRoute         bool
	Warnings         []string
}

// Platform converts the discovered arch into a bundle platform.
func (r PreflightReport) Platform() (bundle.Platform, error) {
	return bundle.ParsePlatform("linux/" + r.Arch)
}

// Preflight gathers remote facts and verifies them against the local
// requirements. It makes no changes.
func Preflight(c Executor, nodeName string) (*PreflightReport, error) {
	rep := &PreflightReport{}

	out, err := mustOK(c.Run("uname -s && uname -m"))
	if err != nil {
		return nil, fmt.Errorf("%s: uname: %w", nodeName, err)
	}
	parts := strings.Fields(out)
	if len(parts) < 2 || parts[0] != "Linux" {
		return nil, fmt.Errorf("%s: unsupported OS %q (need Linux)", nodeName, parts[0])
	}
	switch parts[1] {
	case "x86_64":
		rep.Arch = "amd64"
	case "aarch64", "arm64":
		rep.Arch = "arm64"
	default:
		return nil, fmt.Errorf("%s: unsupported machine arch %q", nodeName, parts[1])
	}

	host, err := mustOK(c.Run("hostname"))
	if err != nil {
		return nil, fmt.Errorf("%s: hostname: %w", nodeName, err)
	}
	rep.Hostname = strings.TrimSpace(host)

	osr, err := mustOK(c.Run(". /etc/os-release && echo \"$ID $VERSION_ID\""))
	if err == nil {
		f := strings.Fields(osr)
		if len(f) >= 2 {
			rep.OS, rep.OSVersion = f[0], f[1]
		}
	}
	// RKE2's tar install is distro-agnostic; Ubuntu 22.04 is simply the
	// validated baseline. Warn on anything else instead of blocking.
	switch rep.OS {
	case "ubuntu":
		if rep.OSVersion != "22.04" {
			rep.Warnings = append(rep.Warnings,
				fmt.Sprintf("Ubuntu %s is not the validated version (22.04); proceeding anyway", rep.OSVersion))
		}
	case "":
		rep.Warnings = append(rep.Warnings, "could not identify distro (/etc/os-release missing); proceeding anyway")
	default:
		rep.Warnings = append(rep.Warnings,
			fmt.Sprintf("distro %s %s is not in the validated set (Ubuntu 22.04); RKE2 binaries are distro-agnostic, proceeding anyway", rep.OS, rep.OSVersion))
	}

	// The tar install ships no SELinux policy (that lives in the
	// rke2-selinux RPM). Enforcing mode breaks it outright on RHEL-family
	// distros, so refuse early with an actionable message.
	if r, _ := c.RunSudo("getenforce 2>/dev/null"); r != nil && r.ExitCode == 0 &&
		strings.TrimSpace(r.Stdout) == "Enforcing" {
		return nil, fmt.Errorf("%s: SELinux is Enforcing but the tar install carries no SELinux policy: `setenforce 0` (and disable in /etc/selinux/config) or install rke2-selinux first", nodeName)
	}

	// kubelet and Canal program rules through iptables (the nft frontend
	// is fine); a missing binary surfaces much later inside RKE2.
	if r, _ := c.RunSudo("command -v iptables >/dev/null 2>&1"); r == nil || r.ExitCode != 0 {
		rep.Warnings = append(rep.Warnings, "iptables not found; kubelet/Canal require it")
	}

	// RHEL-family firewalls sit in front of the CNI datapath; upstream
	// docs recommend disabling firewalld for RKE2.
	if r, _ := c.RunSudo("systemctl is-active firewalld 2>/dev/null"); r != nil && r.ExitCode == 0 &&
		strings.TrimSpace(r.Stdout) == "active" {
		rep.Warnings = append(rep.Warnings, "firewalld is active; RKE2 docs recommend disabling it or opening the CNI datapath ports (8472/udp, 179/tcp)")
	}

	// NetworkManager will reconfigure the Canal interfaces unless told
	// not to; upstream ships /etc/NetworkManager/conf.d/rke2-canal.conf.
	if r, _ := c.RunSudo("systemctl is-active NetworkManager 2>/dev/null"); r != nil && r.ExitCode == 0 &&
		strings.TrimSpace(r.Stdout) == "active" {
		if r2, _ := c.RunSudo("test -f /etc/NetworkManager/conf.d/rke2-canal.conf"); r2 == nil || r2.ExitCode != 0 {
			rep.Warnings = append(rep.Warnings,
				"NetworkManager is active; add /etc/NetworkManager/conf.d/rke2-canal.conf marking cali*/flannel* unmanaged, or NM may reconfigure CNI interfaces")
		}
	}

	// Verify a real root shell, not just that the command ran: on a
	// local executor without sudo, RunSudo degrades to an unprivileged
	// run and a bare `true` would pass.
	res, err := c.RunSudo(`test "$(id -u)" -eq 0`)
	if err != nil || res.ExitCode != 0 {
		return nil, fmt.Errorf("%s: root privileges required: use spec.ssh.user=root, passwordless sudo (sudo -n), or run searke with sudo on the node itself", nodeName)
	}

	// Disk check is read-only: pick the deepest existing ancestor.
	df, err := mustOK(c.Run(
		"for d in /var/lib/rancher /var/lib /var /; do [ -d \"$d\" ] && df -P -k \"$d\" | tail -1 && break; done"))
	if err == nil {
		f := strings.Fields(df)
		if len(f) >= 4 {
			rep.DiskAvailKiB, _ = strconv.ParseInt(f[len(f)-3], 10, 64)
		}
	}
	if rep.DiskAvailKiB < 3*1024*1024 {
		return nil, fmt.Errorf("%s: insufficient disk on /var/lib/rancher (%d KiB free, need >= 3 GiB)", nodeName, rep.DiskAvailKiB)
	}

	route, err := c.Run("ip route show default")
	rep.HasRoute = err == nil && route.ExitCode == 0 && strings.Contains(route.Stdout, "default")
	if !rep.HasRoute {
		// Not fatal: an air-gap node on a single subnet can install and
		// run without one; it usually indicates broken networking though.
		rep.Warnings = append(rep.Warnings, "no default route configured; off-subnet traffic (external services, cross-subnet nodes) will fail")
	}

	if r, _ := c.RunSudo("test -d /var/lib/rancher/rke2 && echo yes"); r != nil && strings.TrimSpace(r.Stdout) == "yes" {
		rep.HasRKE2 = true
	}
	if r, _ := c.RunSudo("test -f /etc/rancher/rke2/config.yaml && cat /etc/rancher/rke2/config.yaml"); r != nil && r.ExitCode == 0 {
		rep.ConfigExists = true
		for _, l := range strings.Split(r.Stdout, "\n") {
			l = strings.TrimSpace(l)
			if strings.HasPrefix(l, "server:") {
				rep.ExistingURL = strings.TrimSpace(strings.TrimPrefix(l, "server:"))
			}
		}
	}
	for _, svc := range []string{"rke2-server", "rke2-agent"} {
		if r, _ := c.RunSudo("systemctl is-enabled " + svc + " 2>/dev/null"); r != nil && r.ExitCode == 0 && strings.TrimSpace(r.Stdout) == "enabled" {
			rep.Service = svc
			if r2, _ := c.RunSudo("systemctl is-active " + svc); r2 != nil {
				rep.ServiceState = strings.TrimSpace(r2.Stdout)
			}
			break
		}
	}
	if r, _ := c.RunSudo(rke2Exe + " --version 2>/dev/null | head -1"); r != nil && r.ExitCode == 0 {
		// "rke2 version v1.36.5+rke2r1 (rev abc)"
		if f := strings.Fields(strings.TrimSpace(r.Stdout)); len(f) >= 3 {
			rep.InstalledVersion = f[2]
		}
	}

	// Required ports must be free on a fresh node. On a node that already
	// runs rke2, occupied ports are expected.
	if rep.Service == "" {
		if r, _ := c.Run(`ss -tulnH | awk '{print $5}' | grep -E ':(6443|9345|10250|8472)$'`); r != nil {
			if used := strings.TrimSpace(r.Stdout); used != "" {
				return nil, fmt.Errorf("%s: required ports already in use: %s", nodeName, used)
			}
		}
	}
	return rep, nil
}

// CheckConsistency verifies an already-installed node belongs to this
// cluster rather than a different one. Returns an error on conflict.
func (rep *PreflightReport) CheckConsistency(nodeName, regURL string) error {
	if !rep.ConfigExists {
		return nil
	}
	if rep.ExistingURL == "" {
		// First-server style config (no server: line) is consistent only
		// for the bootstrap node; callers compare hostname separately.
		return nil
	}
	if rep.ExistingURL != regURL {
		return fmt.Errorf("%s: node already joined a different cluster (server: %s); refusing to reset, fix manually", nodeName, rep.ExistingURL)
	}
	return nil
}

func mustOK(r *Result, err error) (string, error) {
	if err != nil {
		return "", err
	}
	if r.ExitCode != 0 {
		return "", fmt.Errorf("exit %d: %s", r.ExitCode, strings.TrimSpace(r.Stderr))
	}
	return r.Stdout, nil
}
