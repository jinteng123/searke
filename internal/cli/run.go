package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jint/searke/internal/bundle"
	"github.com/jint/searke/internal/cluster"
	"github.com/jint/searke/internal/deploy"
	"github.com/jint/searke/internal/ociimg"
	"github.com/spf13/cobra"
)

func runBundleFetch(in, out string) error {
	b, err := bundle.Load(in)
	if err != nil {
		return err
	}
	logf("fetching RKE2 %s (cni %s) for %v -> %s", b.Spec.Version, b.Spec.CNI, b.Spec.Platforms, out)
	if err := bundle.Fetch(context.Background(), b, bundle.FetchOptions{OutputDir: out, Log: logf}); err != nil {
		return err
	}
	logf("bundle complete and verified at %s", out)
	return nil
}

func runBundleVerify(dir string) error {
	b, err := bundle.VerifyBundleDir(dir)
	if err != nil {
		return err
	}
	logf("bundle %s verified: RKE2 %s, cni %s, platforms %v", dir, b.Spec.Version, b.Spec.CNI, b.Spec.Platforms)
	return nil
}

func runImageBuild(bundleDir, tag, outDir, comp string) error {
	if outDir == "" {
		outDir = strings.TrimSuffix(bundleDir, "/") + "-oci"
	}
	meta, err := ociimg.Build(bundleDir, outDir, tag, comp)
	if err != nil {
		return err
	}
	logf("built OCI index at %s", outDir)
	for _, p := range meta.Platforms {
		logf("  %s -> %s", p, meta.Digests[p])
	}
	if tag != "" {
		logf("  (annotation-only tag %s; use `searke image push --from %s %s` to publish)", tag, outDir, tag)
	}
	return nil
}

func runImagePush(ctx context.Context, from, ref string, insecure bool) error {
	if err := ociimg.Push(ctx, from, ref, insecure, os.Stderr); err != nil {
		return err
	}
	logf("pushed %s -> %s", from, ref)
	return nil
}

func runImageSave(src, out string, insecure bool) error {
	s, err := ociimg.ParseSource(src, insecure)
	if err != nil {
		return err
	}
	ii, cleanup, err := s.OpenIndex()
	if err != nil {
		return err
	}
	defer cleanup()
	if err := ociimg.SaveTar(ii, out); err != nil {
		return err
	}
	logf("saved %s -> %s", src, out)
	return nil
}

func runImagePull(ref, outDir string, insecure bool) error {
	fmt.Printf("pulling %s -> %s\n", ref, outDir)
	if err := ociimg.PullToLayout(ref, outDir, insecure); err != nil {
		return err
	}
	fmt.Println("done")
	return nil
}

func runImageInspect(cmd *cobra.Command, src string, insecure bool) error {
	s, err := ociimg.ParseSource(src, insecure)
	if err != nil {
		return err
	}
	ii, cleanup, err := s.OpenIndex()
	if err != nil {
		return err
	}
	defer cleanup()
	m, err := ociimg.IndexMeta(ii)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "version:   %s\ncni:       %s\nplatforms:\n", m.Version, m.CNI)
	for _, p := range m.Platforms {
		fmt.Fprintf(out, "  - %s  %s\n", p, m.Digests[p])
	}
	return nil
}

func runImageLoad(tarPath, outDir string) error {
	if outDir == "" {
		outDir = "oci-layout"
	}
	if err := ociimg.LoadTar(tarPath, outDir); err != nil {
		return err
	}
	logf("loaded %s -> %s", tarPath, outDir)
	return nil
}

func deployOpts(file string) *deploy.Options {
	return &deploy.Options{ClusterFile: file, Log: logf, StrictHostKey: strictHostKeyFlag}
}

// loggedDeployOpts mirrors output into ~/.searke/logs/<cluster>/ for
// mutating operations, so a failed or interrupted run leaves a trail.
// The returned function closes the log file.
func loggedDeployOpts(file string) (*deploy.Options, func(), error) {
	c, err := cluster.Load(file)
	if err != nil {
		return nil, nil, err
	}
	logger, err := newRunLogger(c.Name())
	if err != nil {
		return nil, nil, err
	}
	logger.Log("run log: %s", logger.Path())
	return &deploy.Options{ClusterFile: file, Cluster: c, Log: logger.Log, StrictHostKey: strictHostKeyFlag},
		func() { _ = logger.Close() }, nil
}

func runValidate(file string) error {
	if err := deploy.Validate(deployOpts(file)); err != nil {
		return err
	}
	logf("validate ok: %s", filepath.Clean(file))
	return nil
}

func runPlan(file string) error {
	return deploy.Plan(deployOpts(file))
}

func runApply(ctx context.Context, file string) error {
	c, err := cluster.Load(file)
	if err != nil {
		return err
	}
	logger, err := newRunLogger(c.Name())
	if err != nil {
		return err
	}
	defer logger.Close()
	logger.Log("run log: %s", logger.Path())
	return deploy.Apply(ctx, &deploy.Options{ClusterFile: file, Cluster: c, Log: logger.Log, StrictHostKey: strictHostKeyFlag})
}

// runSingle implements `searke run`: synthesize a one-node local
// cluster, persist its cluster.yaml, then Apply (or only Plan on
// --dry-run).
func runSingle(ctx context.Context, ro deploy.RunOptions, dryRun bool) error {
	ro.Log = logf
	c, file, err := deploy.SingleNodeCluster(ro)
	if err != nil {
		return err
	}
	if dryRun {
		logf("single-node cluster %q: node %s @ %s", c.Name(), c.Spec.Servers[0].Name, c.RegistrationHost())
		logf("cluster.yaml: %s (reuse: searke apply|status|snapshot -f %s)", file, file)
		opts := &deploy.Options{ClusterFile: file, Cluster: c, Log: logf, StrictHostKey: strictHostKeyFlag}
		return deploy.Plan(opts)
	}
	logger, err := newRunLogger(c.Name())
	if err != nil {
		return err
	}
	defer logger.Close()
	logger.Log("run log: %s", logger.Path())
	logger.Log("single-node cluster %q: node %s @ %s", c.Name(), c.Spec.Servers[0].Name, c.RegistrationHost())
	logger.Log("cluster.yaml: %s (reuse: searke apply|status|snapshot -f %s)", file, file)
	opts := &deploy.Options{ClusterFile: file, Cluster: c, Log: logger.Log, StrictHostKey: strictHostKeyFlag}
	return deploy.Apply(ctx, opts)
}

// runRemoteCluster implements `searke run <bundle> --masters/--nodes`:
// generates a multi-node cluster.yaml (sealos-style) then applies it.
func runRemoteCluster(ctx context.Context, ro deploy.RunOptions, dryRun bool) error {
	ro.Log = logf
	c, file, err := deploy.RemoteCluster(ro)
	if err != nil {
		return err
	}
	msg := fmt.Sprintf("cluster %q: %d servers / %d agents, registration %s",
		c.Name(), len(c.Spec.Servers), len(c.Spec.Agents), c.Spec.Endpoints.RegistrationAddress)
	if dryRun {
		logf("%s", msg)
		logf("cluster.yaml: %s", file)
		return deploy.Plan(&deploy.Options{ClusterFile: file, Cluster: c, Log: logf, StrictHostKey: strictHostKeyFlag})
	}
	logger, err := newRunLogger(c.Name())
	if err != nil {
		return err
	}
	defer logger.Close()
	logger.Log("run log: %s", logger.Path())
	logger.Log("%s", msg)
	logger.Log("cluster.yaml: %s", file)
	return deploy.Apply(ctx, &deploy.Options{ClusterFile: file, Cluster: c, Log: logger.Log, StrictHostKey: strictHostKeyFlag})
}

func runStatus(file string) error {
	return deploy.Status(deployOpts(file))
}

// loadCluster parses the descriptor for day-2 ops. Unlike deployOpts
// flows it does not run Validate: bundle/endpoints are install-time
// requirements, not needed to run commands on already-installed nodes.
func loadCluster(file string) (*cluster.Cluster, error) {
	return cluster.Load(file)
}

func runExec(file, nodesCSV, command string) error {
	c, err := loadCluster(file)
	if err != nil {
		return err
	}
	return deploy.Exec(c, deploy.ParseNodeSelectors(nodesCSV), command, logf)
}

func runScp(file, nodesCSV, local, remote string, download bool) error {
	c, err := loadCluster(file)
	if err != nil {
		return err
	}
	return deploy.Scp(c, deploy.ParseNodeSelectors(nodesCSV), local, remote, download, logf)
}

func runHostsSync(file, nodesCSV string) error {
	c, err := loadCluster(file)
	if err != nil {
		return err
	}
	return deploy.HostsSync(c, deploy.ParseNodeSelectors(nodesCSV), logf)
}

func runHostname(file, nodesCSV, name string, force bool) error {
	c, err := loadCluster(file)
	if err != nil {
		return err
	}
	return deploy.SetHostname(c, deploy.ParseNodeSelectors(nodesCSV), name, force, logf)
}

func runKubeconfig(file, outDir, server string) error {
	c, err := loadCluster(file)
	if err != nil {
		return err
	}
	_, err = deploy.FetchKubeconfig(c, outDir, server, logf)
	return err
}

func runUpgrade(ctx context.Context, file string, skipDrain, skipSnapshot bool) error {
	opts, done, err := loggedDeployOpts(file)
	if err != nil {
		return err
	}
	defer done()
	return deploy.Upgrade(ctx, opts, deploy.UpgradeOptions{
		SkipDrain:    skipDrain,
		SkipSnapshot: skipSnapshot,
	})
}

func runSnapshotSave(file, name string) error {
	opts, done, err := loggedDeployOpts(file)
	if err != nil {
		return err
	}
	defer done()
	return deploy.SnapshotSave(opts, name)
}

func runSnapshotList(file string) error {
	return deploy.SnapshotList(deployOpts(file))
}

func runSnapshotPull(file, name, outDir string) error {
	return deploy.SnapshotPull(deployOpts(file), name, outDir)
}

func runSnapshotRestore(file, name string, yes bool) error {
	opts, done, err := loggedDeployOpts(file)
	if err != nil {
		return err
	}
	defer done()
	return deploy.SnapshotRestore(opts, name, yes)
}

func runUninstall(ctx context.Context, file string, yes, ignoreUnreachable bool) error {
	opts, done, err := loggedDeployOpts(file)
	if err != nil {
		return err
	}
	defer done()
	return deploy.Uninstall(ctx, opts, yes, ignoreUnreachable)
}

func runAdd(ctx context.Context, file string, add *deploy.AddOptions) error {
	opts, done, err := loggedDeployOpts(file)
	if err != nil {
		return err
	}
	defer done()
	return deploy.Add(ctx, opts, add)
}

func runLogin(ctx context.Context, registry, user, pass string, insecure bool) error {
	warns, err := ociimg.Login(ctx, registry, user, pass, insecure)
	if err != nil {
		return err
	}
	for _, w := range warns {
		logf("warning: %s", w)
	}
	scheme := "https"
	if insecure {
		scheme = "http"
	}
	logf("logged in to %s (%s); credentials stored in ~/.docker/config.json", registry, scheme)
	return nil
}

func runLogout(registry string) error {
	if err := ociimg.Logout(registry); err != nil {
		return err
	}
	logf("removed credential for %s", registry)
	return nil
}

func runDelete(ctx context.Context, file string, del deploy.DeleteOptions) error {
	opts, done, err := loggedDeployOpts(file)
	if err != nil {
		return err
	}
	defer done()
	return deploy.Delete(ctx, opts, del)
}

func runCertCheck(file, nodesCSV string) error {
	c, err := loadCluster(file)
	if err != nil {
		return err
	}
	return deploy.CertCheck(c, deploy.ParseNodeSelectors(nodesCSV), logf)
}

func runCertSan(file, nodesCSV string, sans []string, yes bool) error {
	opts, done, err := loggedDeployOpts(file)
	if err != nil {
		return err
	}
	defer done()
	return deploy.CertSan(opts.Cluster, deploy.ParseNodeSelectors(nodesCSV), sans, opts, yes)
}

func runCertRotate(file, nodesCSV string, yes bool) error {
	opts, done, err := loggedDeployOpts(file)
	if err != nil {
		return err
	}
	defer done()
	return deploy.CertRotate(opts.Cluster, deploy.ParseNodeSelectors(nodesCSV), opts, yes)
}
