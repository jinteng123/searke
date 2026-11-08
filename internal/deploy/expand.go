package deploy

import (
	"context"
	"fmt"
	"strings"

	"github.com/jint/searke/internal/cluster"
)

// AddOptions configures a sealos-style node expansion: bare addresses
// are appended to the cluster descriptor and then converged by the
// normal apply pipeline — existing nodes skip automatically.
type AddOptions struct {
	Servers []string // additional control-plane addresses
	Agents  []string // additional worker addresses
	SSHUser string   // fills spec.ssh.user when the descriptor lacks one
	SSHKey  string   // fills spec.ssh.identityFile
	SSHPort int      // fills spec.ssh.port
	DryRun  bool     // show the plan without touching nodes or the file
}

// serverPrefix picks the name prefix for added servers. The bootstrap
// server is the name-sorted first one, so a new name sorting before it
// (e.g. "server-1" vs "srv-a") would silently make a fresh node the
// bootstrap and split the cluster. Fall back to "<first>-N", which
// always sorts after <first>.
func serverPrefix(first string) string {
	if "server-1" < first {
		return first
	}
	return "server"
}

// Add validates the expanded spec — including connectivity and remote
// preflight on every listed node — before persisting the new nodes into
// cluster.yaml, then runs Apply so only the new members converge. The
// descriptor stays the single source of truth for
// status/snapshot/uninstall afterwards.
func Add(ctx context.Context, opts *Options, add *AddOptions) error {
	if len(add.Servers)+len(add.Agents) == 0 {
		return fmt.Errorf("nothing to add: pass --servers/--agents (aliases --masters/--nodes)")
	}
	c, err := cluster.Load(opts.ClusterFile)
	if err != nil {
		return err
	}

	existing := map[string]string{}
	taken := map[string]bool{}
	for _, n := range c.Spec.Servers {
		existing[n.Address] = n.Name
		taken[n.Name] = true
	}
	for _, n := range c.Spec.Agents {
		existing[n.Address] = n.Name
		taken[n.Name] = true
	}
	nextName := func(prefix string) string {
		for i := 1; ; i++ {
			cand := fmt.Sprintf("%s-%d", prefix, i)
			if !taken[cand] {
				taken[cand] = true
				return cand
			}
		}
	}
	collect := func(addrs []string, prefix string) []cluster.Node {
		var out []cluster.Node
		for _, a := range addrs {
			a = strings.TrimSpace(a)
			if a == "" {
				continue
			}
			if prev, ok := existing[a]; ok {
				logf(opts, "  %s already in cluster as %s, skipping", a, prev)
				continue
			}
			n := cluster.Node{Name: nextName(prefix), Address: a}
			existing[a] = n.Name
			out = append(out, n)
		}
		return out
	}
	addServers := collect(add.Servers, serverPrefix(c.FirstServer().Name))
	addAgents := collect(add.Agents, "agent")
	if len(addServers)+len(addAgents) == 0 {
		return fmt.Errorf("all requested addresses are already cluster members")
	}

	var sshUpd *cluster.SSHSpec
	if add.SSHUser != "" || add.SSHKey != "" || add.SSHPort != 0 {
		sshUpd = &cluster.SSHSpec{User: add.SSHUser, IdentityFile: add.SSHKey, Port: add.SSHPort}
		conflict := func(flag, old, val string) {
			if old != "" && old != val {
				logf(opts, "warning: %s overrides existing %q for ALL nodes", flag, old)
			}
		}
		if add.SSHUser != "" {
			conflict("--ssh-user", c.Spec.SSH.User, add.SSHUser)
			c.Spec.SSH.User = add.SSHUser
		}
		if add.SSHKey != "" {
			conflict("--ssh-key", c.Spec.SSH.IdentityFile, add.SSHKey)
			c.Spec.SSH.IdentityFile = add.SSHKey
		}
		if add.SSHPort != 0 && c.Spec.SSH.Port != 0 && c.Spec.SSH.Port != add.SSHPort {
			logf(opts, "warning: --ssh-port overrides existing %d for ALL nodes", c.Spec.SSH.Port)
		}
		if add.SSHPort != 0 {
			c.Spec.SSH.Port = add.SSHPort
		}
	}
	c.Spec.Servers = append(c.Spec.Servers, addServers...)
	c.Spec.Agents = append(c.Spec.Agents, addAgents...)
	if err := c.Validate(); err != nil {
		return err
	}
	for _, n := range append(append([]cluster.Node{}, c.Spec.Servers...), c.Spec.Agents...) {
		if cluster.IsLocalAddress(n.Address) {
			logf(opts, "warning: descriptor contains local node %s (%s); run this on that machine or it executes locally", n.Name, n.Address)
			break
		}
	}

	// Preflight before persistence: the expanded spec must connect and
	// pass node checks, otherwise the file is left untouched so a typo
	// never poisons the descriptor that every other command reads.
	opts.Cluster = c
	if add.DryRun {
		return Plan(opts)
	}
	rts, err := connectAll(c, opts)
	if err != nil {
		return fmt.Errorf("preflight failed, cluster.yaml left unchanged: %w", err)
	}
	closeAll(rts)

	if err := cluster.UpdateDescriptor(opts.ClusterFile, addServers, addAgents, sshUpd); err != nil {
		return fmt.Errorf("update %s: %w", opts.ClusterFile, err)
	}
	for _, n := range addServers {
		logf(opts, "added server %s (%s) to %s", n.Name, n.Address, opts.ClusterFile)
	}
	for _, n := range addAgents {
		logf(opts, "added agent %s (%s) to %s", n.Name, n.Address, opts.ClusterFile)
	}
	return Apply(ctx, opts)
}
