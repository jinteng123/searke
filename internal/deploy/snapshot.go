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

// connectFirst opens SSH to the bootstrap server.
func connectFirst(c *cluster.Cluster) (*Client, error) {
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

// SnapshotRestore performs a cluster-reset restore on the bootstrap
// server ONLY. Other server nodes must be rejoined afterwards (rerun
// `searke apply`). Requires confirm=true (the --yes flag).
func SnapshotRestore(opts *Options, name string, confirm bool) error {
	if !confirm {
		return fmt.Errorf("restore is destructive (etcd cluster-reset on %s); pass --yes to confirm",
			snapshotDir+"/"+name)
	}
	c, err := cluster.Load(opts.ClusterFile)
	if err != nil {
		return err
	}
	cl, err := connectFirst(c)
	if err != nil {
		return err
	}
	defer cl.Close()

	logf(opts, "stopping rke2-server on %s", c.FirstServer().Name)
	if res, err := cl.RunSudo("systemctl stop rke2-server"); err != nil {
		return err
	} else if res.ExitCode != 0 {
		return fmt.Errorf("stop rke2-server: %s", res.Stderr)
	}
	logf(opts, "cluster-reset --cluster-reset-restore-path=%s/%s", snapshotDir, name)
	res, err := cl.RunSudo(fmt.Sprintf("rke2 server --cluster-reset --cluster-reset-restore-path=%s/%s",
		snapshotDir, shellQuote(name)))
	if err != nil {
		return fmt.Errorf("cluster-reset: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("cluster-reset failed (exit %d): %s", res.ExitCode, tail(res.Stderr+res.Stdout, 800))
	}
	logf(opts, "starting rke2-server")
	res, err = cl.RunSudo("systemctl start rke2-server")
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("start rke2-server: %s", res.Stderr)
	}
	logf(opts, "restore complete on %s. If the cluster had other servers, rejoin them with `searke apply`.", c.FirstServer().Name)
	return nil
}
