package deploy

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/jint/searke/internal/cluster"
)

// rke2BinDirs are the install prefixes used by upstream install.sh —
// /opt/rke2 is the fallback when /usr/local is read-only.
var rke2BinDirs = []string{"/usr/local/bin", "/opt/rke2/bin"}

// Uninstall removes RKE2 from every node in the cluster file using the
// upstream rke2-killall.sh / rke2-uninstall.sh scripts shipped by the
// installer. Agents are torn down first, then servers, so workload
// nodes detach before the control plane disappears. The searke payload
// dir and the local state file are removed as well.
//
// Node failures are collected rather than aborting: remaining nodes
// are still cleaned, the state file is kept, and the command exits
// non-zero. --ignore-unreachable clears the state anyway (e.g. for
// nodes that are permanently gone). Requires confirm.
func Uninstall(ctx context.Context, opts *Options, confirm, ignoreUnreachable bool) error {
	if !confirm {
		return fmt.Errorf("uninstall removes RKE2, etcd data and workloads on every listed node; pass --yes to confirm")
	}
	c, err := cluster.Load(opts.ClusterFile)
	if err != nil {
		return err
	}
	sshCfg := sshConfig(c)

	var ordered []cluster.Node
	ordered = append(ordered, c.Spec.Agents...)
	ordered = append(ordered, c.Spec.Servers...)

	var failed []string
	for _, n := range ordered {
		if err := ctx.Err(); err != nil {
			return err
		}
		role := c.RoleOf(n.Name)
		cl, err := Connect(n.Address, sshCfg)
		if err != nil {
			failed = append(failed, n.Name+": "+err.Error())
			logf(opts, "  %s (%s): connect failed: %v", n.Name, role, err)
			continue
		}
		mode := "ssh"
		if _, ok := cl.(*LocalExecutor); ok {
			mode = "local exec"
		}
		logf(opts, "  %s (%s, %s): uninstall", n.Name, role, mode)
		if err := uninstallNode(cl, n.Name, opts); err != nil {
			failed = append(failed, n.Name+": "+err.Error())
			logf(opts, "  %s: FAILED: %v", n.Name, err)
		}
		cl.Close()
	}

	if len(failed) > 0 && !ignoreUnreachable {
		return fmt.Errorf("uninstall incomplete on %s; state kept — fix and rerun, or pass --ignore-unreachable",
			strings.Join(failed, "; "))
	}

	statePath := StatePath(stateDir(opts), c.Name())
	st, err := LoadStateUnchecked(statePath, c.Name())
	if err == nil && st != nil {
		if _, statErr := os.Stat(statePath); statErr == nil {
			if err := st.Reset(); err != nil {
				return err
			}
			logf(opts, "state cleared for cluster %s", c.Name())
		}
	}
	if len(failed) > 0 {
		logf(opts, "uninstall finished with %d skipped node(s): %s", len(failed), strings.Join(failed, "; "))
	} else {
		logf(opts, "uninstall complete on %d node(s)", len(ordered))
	}
	return nil
}

// uninstallNode runs the upstream removal scripts and then verifies the
// outcome: an exit-code failure is only an error when RKE2 residue is
// still detected afterwards.
func uninstallNode(cl Executor, name string, opts *Options) error {
	var binDir string
	for _, d := range rke2BinDirs {
		if r, _ := cl.RunSudo("test -f " + d + "/rke2-uninstall.sh"); r != nil && r.ExitCode == 0 {
			binDir = d
			break
		}
	}
	if binDir == "" {
		// No upstream uninstaller — either already clean or an install
		// layout we do not recognise.
		r, _ := cl.RunSudo("test -e /usr/local/bin/rke2 -o -e /opt/rke2/bin/rke2 -o -d /var/lib/rancher/rke2")
		if r != nil && r.ExitCode != 0 {
			logf(opts, "  %s: no RKE2 install found, already clean", name)
			return nil
		}
		return fmt.Errorf("RKE2 present but no rke2-uninstall.sh found in %s", strings.Join(rke2BinDirs, " or "))
	}

	if r, _ := cl.RunSudo("test -f " + binDir + "/rke2-killall.sh"); r != nil && r.ExitCode == 0 {
		if res, err := cl.RunSudo("sh " + binDir + "/rke2-killall.sh"); err != nil || res.ExitCode != 0 {
			// killall is best-effort cleanup; the uninstall script also
			// invokes it and the post-verify decides the final verdict.
			logf(opts, "  %s: rke2-killall.sh reported an error, continuing", name)
		}
	}
	var uninstallErr error
	if res, err := cl.RunSudo("sh " + binDir + "/rke2-uninstall.sh"); err != nil {
		uninstallErr = err
	} else if res.ExitCode != 0 {
		uninstallErr = fmt.Errorf("exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}

	// Verify the outcome rather than trusting exit codes: no rke2
	// binary under either install prefix, no systemd units, no data dir.
	res, err := cl.RunSudo(`{ ls /usr/local/bin/rke2 /opt/rke2/bin/rke2 2>/dev/null; ` +
		`systemctl list-unit-files 'rke2-*' --no-legend 2>/dev/null | grep -v '^$'; ` +
		`test -d /var/lib/rancher/rke2 && echo /var/lib/rancher/rke2; } | head -10`)
	if err != nil {
		return fmt.Errorf("post-uninstall verify failed: %w", err)
	}
	if residue := strings.TrimSpace(res.Stdout); residue != "" {
		if uninstallErr != nil {
			return fmt.Errorf("rke2-uninstall.sh failed (%v); residue: %s", uninstallErr, residue)
		}
		return fmt.Errorf("residue after uninstall: %s", residue)
	}
	if uninstallErr != nil {
		logf(opts, "  %s: rke2-uninstall.sh exited nonzero but no residue detected", name)
	}
	cl.RunSudo(fmt.Sprintf("rm -rf %s %s", RemoteRoot, StagingDir)) // best effort
	return nil
}
