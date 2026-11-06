package deploy

import (
	"fmt"

	"github.com/jint/searke/internal/cluster"
)

// Uninstall removes RKE2 from every node in the cluster file using the
// upstream rke2-killall.sh / rke2-uninstall.sh scripts shipped by the
// installer. Agents are torn down first, then servers, so workload
// nodes detach before the control plane disappears. The searke payload
// dir and the local state file are removed as well. Requires confirm.
func Uninstall(opts *Options, confirm bool) error {
	if !confirm {
		return fmt.Errorf("uninstall removes RKE2, etcd data and workloads on every listed node; pass --yes to confirm")
	}
	c, err := cluster.Load(opts.ClusterFile)
	if err != nil {
		return err
	}
	sshCfg := SSHConfig{
		User:         c.Spec.SSH.User,
		Port:         c.Spec.SSH.Port,
		IdentityFile: c.Spec.SSH.IdentityFile,
		Sudo:         c.Spec.SSH.Sudo,
	}

	// Agents first, then servers — mirrors the install ordering.
	var ordered []cluster.Node
	ordered = append(ordered, c.Spec.Agents...)
	ordered = append(ordered, c.Spec.Servers...)

	for _, n := range ordered {
		role := c.RoleOf(n.Name)
		cl, err := Connect(n.Address, sshCfg)
		if err != nil {
			return fmt.Errorf("%s: %w", n.Name, err)
		}
		if _, ok := cl.(*LocalExecutor); ok {
			logf(opts, "  %s (%s, local exec): killall + uninstall", n.Name, role)
		} else {
			logf(opts, "  %s (%s, ssh): killall + uninstall", n.Name, role)
		}
		// killall stops units and processes; uninstall removes files,
		// units and data. Both are idempotent upstream scripts.
		for _, script := range []string{"rke2-killall.sh", "rke2-uninstall.sh"} {
			// rke2-uninstall.sh exits non-zero when already uninstalled;
			// tolerate that — idempotent is the goal.
			if _, err := cl.RunSudo("sh /usr/local/bin/" + script + " 2>/dev/null || true"); err != nil {
				cl.Close()
				return fmt.Errorf("%s: %s: %w", n.Name, script, err)
			}
		}
		res, err := cl.RunSudo(fmt.Sprintf("rm -rf %s %s", RemoteRoot, StagingDir))
		if err == nil && res.ExitCode == 0 {
			logf(opts, "  %s: cleaned %s", n.Name, RemoteRoot)
		}
		cl.Close()
	}

	st, err := LoadStateUnchecked(StatePath(stateDir(opts), c.Name()), c.Name())
	if err == nil && st != nil {
		if err := st.Reset(); err != nil {
			return err
		}
		logf(opts, "state cleared for cluster %s", c.Name())
	}
	logf(opts, "uninstall complete on %d node(s)", len(ordered))
	return nil
}
