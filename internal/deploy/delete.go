package deploy

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/jint/searke/internal/cluster"
)

// DeleteOptions controls `searke delete`.
type DeleteOptions struct {
	Servers      []string // node names or addresses to remove
	Agents       []string
	DryRun       bool          // print the plan, touch nothing
	Yes          bool          // required for the real run
	SkipDrain    bool          // do not kubectl drain first
	DrainTimeout time.Duration // kubectl drain --timeout (default 120s)
}

// Delete removes nodes: drain -> kubectl delete node -> (servers) etcd
// member verification -> upstream uninstall on the removed node ->
// only then is cluster.yaml and apply state rewritten. Any failure
// before the descriptor write leaves cluster.yaml untouched.
//
// Scheme-A guardrails: the resulting server count must stay odd (a
// 3->1 or 5->3 removal is allowed; 3->2 is refused), the bootstrap
// (first) server and the registration-address server are never
// deleted, and removing every server is refused — that's `uninstall`.
func Delete(ctx context.Context, opts *Options, del DeleteOptions) error {
	c, err := cluster.Load(opts.ClusterFile)
	if err != nil {
		return err
	}
	delServers, err := resolveRole(c, del.Servers, "server")
	if err != nil {
		return err
	}
	delAgents, err := resolveRole(c, del.Agents, "agent")
	if err != nil {
		return err
	}
	if len(delServers)+len(delAgents) == 0 {
		return fmt.Errorf("nothing to delete: pass --servers and/or --agents")
	}

	first := c.FirstServer()
	for _, n := range delServers {
		if n.Name == first.Name {
			return fmt.Errorf("%s is the bootstrap server: status/snapshot/upgrade depend on it; refusing", n.Name)
		}
		if host, _, err := net.SplitHostPort(c.Spec.Endpoints.RegistrationAddress); err == nil && host == n.Address {
			return fmt.Errorf("%s is the registration address: new nodes join through it; refusing", n.Name)
		}
	}
	remaining := len(c.Spec.Servers) - len(delServers)
	switch {
	case remaining == 0:
		return fmt.Errorf("deleting all servers destroys the cluster; use `searke uninstall`")
	case remaining%2 == 0:
		return fmt.Errorf("deleting %d server(s) leaves %d: etcd requires an odd server count "+
			"(delete one fewer or one more)", len(delServers), remaining)
	}

	// Ordered work: agents first (leaf nodes), then servers.
	var doomed []cluster.Node
	doomed = append(doomed, delAgents...)
	doomed = append(doomed, delServers...)
	isServer := map[string]bool{}
	for _, n := range delServers {
		isServer[n.Name] = true
	}

	timeout := del.DrainTimeout
	if timeout == 0 {
		timeout = 120 * time.Second
	}

	if del.DryRun {
		logf(opts, "dry-run: would delete %d agent(s) %d server(s): %s",
			len(delAgents), len(delServers), nodeNameList(doomed))
		logf(opts, "dry-run: remaining servers: %d (odd OK)", remaining)
		if !del.SkipDrain {
			logf(opts, "dry-run: each node is drained first (timeout %s), etcd members verified for servers", timeout)
		}
		return nil
	}
	if !del.Yes {
		return fmt.Errorf("delete drains and removes %s permanently; pass --yes to confirm (--dry-run shows the plan)",
			nodeNameList(doomed))
	}

	kctl, firstExec, err := kubectlOnFirst(c, opts)
	if err != nil {
		return err
	}
	defer firstExec.Close()

	var names []string
	var nodeCleanupFailed []string
	for _, n := range doomed {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !del.SkipDrain {
			logf(opts, "  %s: drain (timeout %s)", n.Name, timeout)
			res, err := firstExec.RunSudo(fmt.Sprintf(
				"%s drain %s --ignore-daemonsets --delete-emptydir-data --timeout=%ds",
				kctl, shellQuote(n.Name), int(timeout.Seconds())))
			if err != nil {
				return fmt.Errorf("%s: drain: %w", n.Name, err)
			}
			if res.ExitCode != 0 {
				return fmt.Errorf("%s: drain failed: %s — cluster.yaml unchanged; "+
					"fix the pods blocking eviction or use --skip-drain", n.Name,
					tail(res.Stderr+res.Stdout, 400))
			}
		}
		logf(opts, "  %s: kubectl delete node", n.Name)
		res, err := firstExec.RunSudo(kctl + " delete node " + shellQuote(n.Name))
		if err != nil {
			return fmt.Errorf("%s: delete node: %w", n.Name, err)
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("%s: delete node: %s", n.Name, tail(res.Stderr+res.Stdout, 400))
		}

		if isServer[n.Name] {
			if err := verifyEtcdMemberRemoved(ctx, firstExec, kctl, n, opts); err != nil {
				return fmt.Errorf("%s: %w — node object removed; resolve the etcd member manually, "+
					"then rerun to finish cleanup", n.Name, err)
			}
		}

		// Node is out of the cluster. Now wipe RKE2 on it; failure here
		// does not roll back the cluster-side removal.
		cl, err := Connect(n.Address, sshConfig(c))
		if err != nil {
			nodeCleanupFailed = append(nodeCleanupFailed, fmt.Sprintf("%s: connect: %v", n.Name, err))
		} else {
			if err := uninstallNode(cl, n.Name, opts); err != nil {
				nodeCleanupFailed = append(nodeCleanupFailed, fmt.Sprintf("%s: %v", n.Name, err))
			} else {
				logf(opts, "  %s: RKE2 uninstalled", n.Name)
			}
			cl.Close()
		}
		names = append(names, n.Name)
	}

	// Remote cluster state is settled; update the local records.
	if err := cluster.RemoveFromDescriptor(opts.ClusterFile, names); err != nil {
		return fmt.Errorf("cluster.yaml update failed after nodes were removed: %w", err)
	}
	st, err := LoadStateUnchecked(StatePath(stateDir(opts), c.Name()), c.Name())
	if err == nil && st != nil {
		for _, n := range names {
			st.RemoveNode(n)
		}
		st.Log("delete", strings.Join(names, ","))
		st.Save()
	}
	logf(opts, "deleted %s; cluster now has %d server(s) %d agent(s)",
		nodeNameList(doomed), remaining, len(c.Spec.Agents)-len(delAgents))
	if len(nodeCleanupFailed) > 0 {
		return fmt.Errorf("nodes removed from cluster but RKE2 cleanup failed on: %s",
			strings.Join(nodeCleanupFailed, "; "))
	}
	return nil
}

// resolveRole maps names/addresses to descriptor entries of the given
// role; a name that exists under the other role is called out clearly.
func resolveRole(c *cluster.Cluster, selectors []string, role string) ([]cluster.Node, error) {
	pool := c.Spec.Agents
	if role == "server" {
		pool = c.Spec.Servers
	}
	byKey := map[string]cluster.Node{}
	for _, n := range pool {
		byKey[n.Name] = n
		byKey[n.Address] = n
	}
	var out []cluster.Node
	for _, s := range selectors {
		n, ok := byKey[s]
		if !ok {
			if other := c.RoleOf(s); other != "" && other != role {
				return nil, fmt.Errorf("%q is a %s node; use --%ss for %ss", s, other, other, other)
			}
			return nil, fmt.Errorf("%q is not a %s in the cluster descriptor", s, role)
		}
		out = append(out, n)
	}
	return out, nil
}

// kubectlOnFirst connects to the bootstrap server and returns the
// kubectl invocation prefix.
func kubectlOnFirst(c *cluster.Cluster, opts *Options) (string, Executor, error) {
	first := c.FirstServer()
	ex, err := Connect(first.Address, sshConfig(c))
	if err != nil {
		return "", nil, fmt.Errorf("connect bootstrap server %s: %w", first.Name, err)
	}
	return fmt.Sprintf("%s --kubeconfig %s", kubectlPath, kubeconfigPath), ex, nil
}

// verifyEtcdMemberRemoved polls `etcdctl member list` inside a
// surviving etcd pod until the deleted server disappears. RKE2's
// controller removes the member when the Node object goes away, but
// that is asynchronous — treat it as authoritative before we wipe the
// node's data dir.
func verifyEtcdMemberRemoved(ctx context.Context, ex Executor, kctl string,
	deleted cluster.Node, opts *Options) error {

	// Any surviving etcd pod works; pick the first listed.
	return waitFor(ctx, 60*time.Second, func() (bool, error) {
		res, err := ex.RunSudo(kctl +
			" -n kube-system get pods -l component=etcd --field-selector=status.phase=Running" +
			" -o jsonpath={.items[0].metadata.name}")
		if err != nil || res.ExitCode != 0 || strings.TrimSpace(res.Stdout) == "" {
			return false, nil
		}
		pod := strings.TrimSpace(res.Stdout)
		res, err = ex.RunSudo(fmt.Sprintf(
			"%s -n kube-system exec %s -- etcdctl"+
				" --endpoints=https://127.0.0.1:2379"+
				" --cacert=/var/lib/rancher/rke2/server/tls/etcd/server-ca.crt"+
				" --cert=/var/lib/rancher/rke2/server/tls/etcd/client.crt"+
				" --key=/var/lib/rancher/rke2/server/tls/etcd/client.key member list",
			kctl, shellQuote(pod)))
		if err != nil || res.ExitCode != 0 {
			return false, nil // etcd still settling; retry
		}
		out := res.Stdout + res.Stderr
		if strings.Contains(out, deleted.Name) || strings.Contains(out, deleted.Address) {
			return false, nil // still a member; keep waiting
		}
		logf(opts, "  %s: etcd member removed (verified)", deleted.Name)
		return true, nil
	})
}

func nodeNameList(nodes []cluster.Node) string {
	var s []string
	for _, n := range nodes {
		s = append(s, n.Name)
	}
	return strings.Join(s, ",")
}
