package deploy

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jint/searke/internal/cluster"
)

// snapshotDir is where RKE2 stores etcd snapshots on a server node.
const snapshotDir = "/var/lib/rancher/rke2/server/db/snapshots"

// connectFirst opens a connection to the bootstrap server.
func connectFirst(c *cluster.Cluster) (Executor, error) {
	first := c.FirstServer()
	return Connect(first.Address, SSHConfig{
		User:         c.Spec.SSH.User,
		Port:         c.Spec.SSH.Port,
		IdentityFile: c.Spec.SSH.IdentityFile,
		Sudo:         c.Spec.SSH.Sudo,
	})
}

// SnapshotSave creates an etcd snapshot on the bootstrap server.
func SnapshotSave(opts *Options, name string) error {
	c, err := cluster.Load(opts.ClusterFile)
	if err != nil {
		return err
	}
	if name == "" {
		name = "searke-" + time.Now().UTC().Format("20060102-150405")
	}
	cl, err := connectFirst(c)
	if err != nil {
		return err
	}
	defer cl.Close()
	res, err := cl.RunSudo("rke2 etcd-snapshot save --name " + shellQuote(name))
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("etcd-snapshot save failed: %s", res.Stderr)
	}
	logf(opts, "snapshot %s saved on %s (%s)", name, c.FirstServer().Name, snapshotDir)
	return nil
}

// SnapshotList lists snapshots present on the bootstrap server.
func SnapshotList(opts *Options) error {
	c, err := cluster.Load(opts.ClusterFile)
	if err != nil {
		return err
	}
	cl, err := connectFirst(c)
	if err != nil {
		return err
	}
	defer cl.Close()
	res, err := cl.RunSudo("ls -lh " + snapshotDir + " 2>/dev/null | tail -n +2")
	if err != nil {
		return err
	}
	if res.ExitCode != 0 || strings.TrimSpace(res.Stdout) == "" {
		logf(opts, "no snapshots on %s", c.FirstServer().Name)
		return nil
	}
	logf(opts, "snapshots on %s:", c.FirstServer().Name)
	for _, l := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		logf(opts, "  %s", l)
	}
	return nil
}

// SnapshotPull downloads a snapshot file to a local directory.
func SnapshotPull(opts *Options, name, outDir string) error {
	c, err := cluster.Load(opts.ClusterFile)
	if err != nil {
		return err
	}
	cl, err := connectFirst(c)
	if err != nil {
		return err
	}
	defer cl.Close()
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return err
	}
	local := filepath.Join(outDir, name)
	if err := cl.DownloadFile(snapshotDir+"/"+name, local); err != nil {
		return err
	}
	logf(opts, "downloaded %s -> %s", name, local)
	return nil
}

// SnapshotRestore performs an etcd cluster-reset. For multi-server
// clusters it follows the RKE2 HA procedure: stop rke2-server on all
// servers, reset on the bootstrap server, then wipe the etcd db on the
// remaining servers and start them so they rejoin cleanly.
// Requires confirm=true (the --yes flag).
func SnapshotRestore(opts *Options, name string, confirm bool) error {
	if strings.Contains(name, "/") || strings.Contains(name, "..") {
		return fmt.Errorf("invalid snapshot name %q", name)
	}
	if !confirm {
		return fmt.Errorf("restore is destructive (etcd cluster-reset from %s/%s); pass --yes to confirm",
			snapshotDir, name)
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

	clients := map[string]Executor{}
	defer func() {
		for _, cl := range clients {
			cl.Close()
		}
	}()
	conn := func(n cluster.Node) (Executor, error) {
		if cl, ok := clients[n.Name]; ok {
			return cl, nil
		}
		cl, err := Connect(n.Address, sshCfg)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", n.Name, err)
		}
		clients[n.Name] = cl
		return cl, nil
	}

	first := c.FirstServer()
	// 1. Stop rke2-server on every server node.
	for _, n := range c.Spec.Servers {
		cl, err := conn(n)
		if err != nil {
			return err
		}
		logf(opts, "stopping rke2-server on %s", n.Name)
		res, err := cl.RunSudo("systemctl stop rke2-server")
		if err != nil {
			return fmt.Errorf("%s: %w", n.Name, err)
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("%s: stop rke2-server: %s", n.Name, res.Stderr)
		}
	}

	// 2. Cluster-reset on the bootstrap server.
	cl := clients[first.Name]
	logf(opts, "cluster-reset --cluster-reset-restore-path=%s/%s on %s", snapshotDir, name, first.Name)
	res, err := cl.RunSudo(fmt.Sprintf("rke2 server --cluster-reset --cluster-reset-restore-path=%s/%s",
		snapshotDir, shellQuote(name)))
	if err != nil {
		return fmt.Errorf("cluster-reset: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("cluster-reset failed (exit %d): %s", res.ExitCode, tail(res.Stderr+res.Stdout, 800))
	}
	if res, err := cl.RunSudo("systemctl start rke2-server"); err != nil {
		return err
	} else if res.ExitCode != 0 {
		return fmt.Errorf("start rke2-server on %s: %s", first.Name, res.Stderr)
	}

	// 3. Other servers: wipe etcd db and restart to rejoin.
	for _, n := range c.Spec.Servers {
		if n.Name == first.Name {
			continue
		}
		cl := clients[n.Name]
		logf(opts, "wiping etcd db and restarting %s", n.Name)
		res, err := cl.RunSudo("rm -rf /var/lib/rancher/rke2/server/db && systemctl start rke2-server")
		if err != nil {
			return fmt.Errorf("%s: %w", n.Name, err)
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("%s: wipe+start: %s", n.Name, res.Stderr)
		}
	}
	logf(opts, "restore complete; %d server(s) rejoined", len(c.Spec.Servers)-1)
	return nil
}
