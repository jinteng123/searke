package deploy

import (
	"fmt"
	"strings"

	"github.com/jint/searke/internal/cluster"
)

// Status prints recorded state plus a live per-node service/node view.
func Status(opts *Options) error {
	c, err := cluster.Load(opts.ClusterFile)
	if err != nil {
		return err
	}
	st, err := LoadState(StatePath(stateDir(opts), c.Name()), c.Name(), c.Spec.RKE2.Bundle, "", "")
	if err != nil {
		return err
	}
	logf(opts, "cluster %s (bundle: %s, version: %s, cni: %s)", st.Cluster, st.Bundle, st.Version, st.CNI)
	if len(st.Nodes) == 0 {
		logf(opts, "no apply state recorded yet")
	}
	for _, n := range c.AllNodes() {
		ns, ok := st.Node(n.Name)
		if !ok {
			logf(opts, "  %s (%s): no state", n.Name, n.Address)
			continue
		}
		logf(opts, "  %s (%s): %s/%s stage=%s at %s", n.Name, n.Address, ns.Role, ns.Arch, ns.Stage, ns.Updated.Format("2006-01-02 15:04:05Z"))
	}
	if len(st.History) > 0 {
		logf(opts, "history:")
		for _, h := range st.History {
			logf(opts, "  %s  %s  %s", h.TS.Format("2006-01-02 15:04:05Z"), h.Action, h.Detail)
		}
	}

	// Live view: query the bootstrap server if one was recorded.
	first := c.FirstServer()
	sshCfg := sshConfig(c)
	cl, err := Connect(first.Address, sshCfg)
	if err != nil {
		logf(opts, "live status unavailable: %v", err)
		return nil
	}
	defer cl.Close()
	res, err := cl.RunSudo(fmt.Sprintf("%s --kubeconfig %s get nodes -o wide --no-headers 2>/dev/null", kubectlPath, kubeconfigPath))
	if err != nil || res.ExitCode != 0 || strings.TrimSpace(res.Stdout) == "" {
		logf(opts, "live status: kubectl on %s not ready", first.Name)
		return nil
	}
	logf(opts, "live nodes (via %s):", first.Name)
	for _, line := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		logf(opts, "  %s", line)
	}
	return nil
}
