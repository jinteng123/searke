package deploy

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/jint/searke/internal/bundle"
)

// PreflightReport is the collected remote state of one node.
type PreflightReport struct {
	Hostname     string
	Arch         string // normalized: amd64 | arm64
	OS           string // distro id
	OSVersion    string
	HasRKE2      bool   // /var/lib/rancher/rke2 exists
	Service      string // rke2-server | rke2-agent | ""
	ServiceState string // systemd is-active output
	ConfigExists bool
	ExistingURL  string // server: line in existing config.yaml, if any
	DiskAvailKiB int64  // on /var/lib/rancher (or /)
	HasRoute     bool
}

// Platform converts the discovered arch into a bundle platform.
func (r PreflightReport) Platform() (bundle.Platform, error) {
	return bundle.ParsePlatform("linux/" + r.Arch)
}

// Preflight gathers remote facts and verifies them against the local
// requirements. It makes no changes.
func Preflight(c *Client, nodeName string) (*PreflightReport, error) {
	rep := &PreflightReport{}

	out, err := mustOK(c.Run("uname -s && uname -m"))
	if err != nil {
		return nil, fmt.Errorf("%s: uname: %w", nodeName, err)
	}
	parts := strings.Fields(out)
	if len(parts) < 2 || parts[0] != "Linux" {
		return nil, fmt.Errorf("%s: unsupported OS %q (need Linux)", nodeName, out)
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

	if _, err := c.RunSudo("true"); err != nil {
		return nil, fmt.Errorf("%s: passwordless sudo required (or spec.ssh.user=root): %w", nodeName, err)
	}

	df, err := mustOK(c.RunSudo("mkdir -p /var/lib/rancher && df -P -k /var/lib/rancher | tail -1"))
	if err == nil {
		f := strings.Fields(df)
		if len(f) >= 4 {
			rep.DiskAvailKiB, _ = strconv.ParseInt(f[len(f)-3], 10, 64)
		}
	}
	if rep.DiskAvailKiB < 3*1024*1024 {
		return nil, fmt.Errorf("%s: insufficient disk on /var/lib/rancher (%d KiB free, need >= 3 GiB)", nodeName, rep.DiskAvailKiB)
	}

	if _, err := c.Run("ip route show default | grep -q default"); err == nil {
		rep.HasRoute = true
	}
	if !rep.HasRoute {
		return nil, fmt.Errorf("%s: no default route configured", nodeName)
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
