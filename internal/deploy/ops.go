package deploy

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/jint/searke/internal/cluster"
)

// ops.go holds the shared plumbing for day-2 node commands
// (exec/scp/hosts/hostname/kubeconfig/delete/cert): select target nodes
// by name or address, build the SSH config once, and connect.

// sshConfig builds the executor config from the cluster descriptor.
func sshConfig(c *cluster.Cluster) SSHConfig {
	return SSHConfig{
		User:         c.Spec.SSH.User,
		Port:         c.Spec.SSH.Port,
		IdentityFile: c.Spec.SSH.IdentityFile,
		Sudo:         c.Spec.SSH.Sudo,
	}
}

// selectNodes resolves the target node list. Empty names means every
// server and agent. Each name may be a node name or address; anything
// not in the descriptor is an error rather than silently skipped.
func selectNodes(c *cluster.Cluster, names []string) ([]cluster.Node, error) {
	all := c.AllNodes()
	if len(names) == 0 {
		return all, nil
	}
	byKey := map[string]cluster.Node{}
	for _, n := range all {
		byKey[n.Name] = n
		byKey[n.Address] = n
	}
	var out []cluster.Node
	seen := map[string]bool{}
	for _, name := range names {
		n, ok := byKey[name]
		if !ok {
			return nil, fmt.Errorf("node %q is not in the cluster descriptor", name)
		}
		if !seen[n.Name] {
			seen[n.Name] = true
			out = append(out, n)
		}
	}
	return out, nil
}

// parseNodeSelectors splits comma-separated node names/addresses from
// CLI flag values like "m1,10.0.0.2".
func ParseNodeSelectors(csv string) []string {
	var out []string
	for _, s := range strings.Split(csv, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// forEachNode connects to each selected node and invokes fn. Node
// failures do not stop the walk; they are collected and returned as
// one aggregate error so partially-unreachable clusters still get the
// work done on the healthy nodes.
func forEachNode(c *cluster.Cluster, names []string,
	fn func(cluster.Node, Executor) error) error {

	nodes, err := selectNodes(c, names)
	if err != nil {
		return err
	}
	cfg := sshConfig(c)
	var errs []string
	for _, n := range nodes {
		ex, err := Connect(n.Address, cfg)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: connect: %v", n.Name, err))
			continue
		}
		if err := fn(n, ex); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", n.Name, err))
		}
		ex.Close()
	}
	if len(errs) > 0 {
		return fmt.Errorf("%d of %d node(s) failed:\n  %s",
			len(errs), len(nodes), strings.Join(errs, "\n  "))
	}
	return nil
}

// runAsRoot wraps RunSudo and collapses a nonzero exit into an error
// carrying stderr.
func runAsRoot(ex Executor, cmd string) (*Result, error) {
	res, err := ex.RunSudo(cmd)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		if msg := strings.TrimSpace(res.Stderr); msg != "" {
			return res, fmt.Errorf("%s", msg)
		}
		return res, fmt.Errorf("exit %d", res.ExitCode)
	}
	return res, nil
}

// Exec runs a shell command on every selected node as root, printing
// output under a [node] prefix. Failures on individual nodes do not
// abort the rest.
func Exec(c *cluster.Cluster, names []string, command string, log func(string, ...any)) error {
	return forEachNode(c, names, func(n cluster.Node, ex Executor) error {
		res, err := ex.RunSudo(command)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(strings.TrimRight(res.Stdout, "\n"), "\n") {
			if line != "" {
				log("[%s] %s", n.Name, line)
			}
		}
		if res.ExitCode != 0 {
			if msg := strings.TrimSpace(res.Stderr); msg != "" {
				return fmt.Errorf("exit %d: %s", res.ExitCode, msg)
			}
			return fmt.Errorf("exit %d", res.ExitCode)
		}
		if msg := strings.TrimSpace(res.Stderr); msg != "" {
			for _, line := range strings.Split(msg, "\n") {
				log("[%s] (stderr) %s", n.Name, line)
			}
		}
		return nil
	})
}

// Scp transfers one local file to the same remote path on every
// selected node (upload), or — with download — fetches remote from
// each node into localDir/<node-name>-<basename>.
func Scp(c *cluster.Cluster, names []string, local, remote string,
	download bool, log func(string, ...any)) error {

	// Preserve the local file's permission bits on upload — a script
	// pushed with 0644 would silently lose its exec bit.
	mode := os.FileMode(0o644)
	if st, err := os.Stat(local); err == nil {
		mode = st.Mode().Perm()
	}

	return forEachNode(c, names, func(n cluster.Node, ex Executor) error {
		if !download {
			if err := ex.UploadFile(local, remote, mode); err != nil {
				return err
			}
			log("[%s] uploaded %s -> %s", n.Name, local, remote)
			return nil
		}
		dest := filepath.Join(local, n.Name+"-"+filepath.Base(remote))
		if err := ex.DownloadFile(remote, dest); err != nil {
			return err
		}
		log("[%s] downloaded %s -> %s", n.Name, remote, dest)
		return nil
	})
}

const hostsBlockBegin = "# searke-hosts-begin"
const hostsBlockEnd = "# searke-hosts-end"

// HostsSync writes a managed block of "<address> <name>" entries for
// every cluster node into each target's /etc/hosts. The block is
// delimited by markers so repeated runs replace rather than duplicate.
// If cloud-init may rewrite /etc/hosts on reboot a warning is logged.
func HostsSync(c *cluster.Cluster, names []string, log func(string, ...any)) error {
	var block strings.Builder
	block.WriteString(hostsBlockBegin + "\n")
	entries := 0
	for _, n := range c.AllNodes() {
		// Only routable IPs belong in /etc/hosts — a node addressed by
		// hostname or loopback would poison every peer's resolution.
		ip := net.ParseIP(n.Address)
		if ip == nil || ip.IsLoopback() {
			continue
		}
		block.WriteString(n.Address + " " + n.Name + "\n")
		entries++
	}
	block.WriteString(hostsBlockEnd + "\n")

	return forEachNode(c, names, func(n cluster.Node, ex Executor) error {
		res, err := ex.RunSudo("cat /etc/hosts")
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("read /etc/hosts: %s", strings.TrimSpace(res.Stderr))
		}
		merged := replaceHostsBlock(res.Stdout, block.String())
		if err := ex.WriteFile("/etc/hosts", []byte(merged), 0o644); err != nil {
			return err
		}
		// cloud-init can rewrite /etc/hosts at boot and drop the
		// managed block — but only when actually enabled in
		// cloud.cfg/cloud.cfg.d. (Alibaba Cloud images enable it.)
		if r, _ := ex.RunSudo(`grep -rE 'manage_etc_hosts: *(true|True|localhost)' /etc/cloud/cloud.cfg /etc/cloud/cloud.cfg.d/ 2>/dev/null`); r != nil && strings.TrimSpace(r.Stdout) != "" {
			log("[%s] warning: cloud-init manages /etc/hosts and may revert this on reboot", n.Name)
		}
		log("[%s] /etc/hosts synced (%d entries)", n.Name, entries)
		return nil
	})
}

// replaceHostsBlock swaps an existing managed block for the new one,
// or appends it when absent.
func replaceHostsBlock(current, block string) string {
	lines := strings.Split(current, "\n")
	var kept []string
	inBlock := false
	for _, l := range lines {
		switch strings.TrimSpace(l) {
		case hostsBlockBegin:
			inBlock = true
		case hostsBlockEnd:
			inBlock = false
		default:
			if !inBlock {
				kept = append(kept, l)
			}
		}
	}
	out := strings.TrimRight(strings.Join(kept, "\n"), "\n") + "\n"
	return out + block
}

// SetHostname renames nodes via hostnamectl. A node that already has
// RKE2 installed refuses the rename unless force is set, since the
// node name is embedded in kubelet/etcd state. An explicit hostname
// applies to a single node only — renaming every node to the same
// name would break etcd/k8s identity.
func SetHostname(c *cluster.Cluster, names []string, hostname string,
	force bool, log func(string, ...any)) error {

	if hostname != "" {
		nodes, err := selectNodes(c, names)
		if err != nil {
			return err
		}
		if len(nodes) != 1 {
			return fmt.Errorf("an explicit hostname applies to exactly one node; "+
				"pass --nodes to select it (%d selected)", len(nodes))
		}
	}

	return forEachNode(c, names, func(n cluster.Node, ex Executor) error {
		if r, _ := ex.RunSudo("test -d /var/lib/rancher/rke2"); r != nil && r.ExitCode == 0 && !force {
			return fmt.Errorf("RKE2 already installed; refusing to rename without --force")
		}
		target := hostname
		if target == "" {
			target = n.Name
		}
		if _, err := runAsRoot(ex, "hostnamectl set-hostname "+shellQuote(target)); err != nil {
			return err
		}
		// Update the 127.0.1.1 self-reference or every sudo prints
		// "unable to resolve host".
		if _, err := runAsRoot(ex, "sed -i -E 's/^(127\\.0\\.1\\.1[[:space:]]+).*/\\1'"+
			shellQuote(target)+"/' /etc/hosts"); err != nil {
			return fmt.Errorf("hostname set but /etc/hosts update failed: %w", err)
		}
		log("[%s] hostname set to %s", n.Name, target)
		return nil
	})
}

// FetchKubeconfig pulls /etc/rancher/rke2/rke2.yaml from the first
// server, rewrites the apiserver URL to the server address, and writes
// it to outDir/<cluster>-kubeconfig with mode 0600. It is never
// printed to stdout: the file contains cluster-admin credentials.
func FetchKubeconfig(c *cluster.Cluster, outDir string, log func(string, ...any)) (string, error) {
	first := c.FirstServer()
	ex, err := Connect(first.Address, sshConfig(c))
	if err != nil {
		return "", fmt.Errorf("connect %s: %w", first.Name, err)
	}
	defer ex.Close()

	tmp, err := os.CreateTemp("", "searke-kubeconfig-*")
	if err != nil {
		return "", err
	}
	tmp.Close()
	defer os.Remove(tmp.Name())
	if err := ex.DownloadFile("/etc/rancher/rke2/rke2.yaml", tmp.Name()); err != nil {
		return "", fmt.Errorf("fetch rke2.yaml: %w", err)
	}
	raw, err := os.ReadFile(tmp.Name())
	if err != nil {
		return "", err
	}
	// The on-node kubeconfig points at the supervisor loopback; clients
	// reaching the API from outside need a reachable address. Prefer
	// the declared apiAddress (VIP/LB) — it is the SAN the serving
	// cert carries — and fall back to the first server's own address.
	host := first.Address
	if h := apiHost(c); h != "" {
		if ip := net.ParseIP(h); ip == nil || !ip.IsLoopback() {
			host = h
		}
	}
	content := strings.ReplaceAll(string(raw), "https://127.0.0.1:6443",
		"https://"+net.JoinHostPort(host, "6443"))
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", err
	}
	dest := filepath.Join(outDir, c.Name()+"-kubeconfig")
	if err := os.WriteFile(dest, []byte(content), 0o600); err != nil {
		return "", err
	}
	log("wrote " + dest)
	return dest, nil
}
