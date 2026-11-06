package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/jint/searke/internal/bundle"
	"github.com/jint/searke/internal/deploy"
	"github.com/jint/searke/internal/ociimg"
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
	return &deploy.Options{ClusterFile: file, Log: logf}
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
	return deploy.Apply(ctx, deployOpts(file))
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
	logf("single-node cluster %q: node %s @ %s", c.Name(), c.Spec.Servers[0].Name, c.RegistrationHost())
	logf("cluster.yaml: %s (reuse: searke apply|status|snapshot -f %s)", file, file)
	opts := &deploy.Options{ClusterFile: file, Cluster: c, Log: logf}
	if dryRun {
		return deploy.Plan(opts)
	}
	return deploy.Apply(ctx, opts)
}

func runStatus(file string) error {
	return deploy.Status(deployOpts(file))
}

func runUpgrade(ctx context.Context, file string, skipDrain, skipSnapshot bool) error {
	return deploy.Upgrade(ctx, deployOpts(file), deploy.UpgradeOptions{
		SkipDrain:    skipDrain,
		SkipSnapshot: skipSnapshot,
	})
}

func runSnapshotSave(file, name string) error {
	return deploy.SnapshotSave(deployOpts(file), name)
}

func runSnapshotList(file string) error {
	return deploy.SnapshotList(deployOpts(file))
}

func runSnapshotPull(file, name, outDir string) error {
	return deploy.SnapshotPull(deployOpts(file), name, outDir)
}

func runSnapshotRestore(file, name string, yes bool) error {
	return deploy.SnapshotRestore(deployOpts(file), name, yes)
}

func runUninstall(ctx context.Context, file string, yes, ignoreUnreachable bool) error {
	return deploy.Uninstall(ctx, deployOpts(file), yes, ignoreUnreachable)
}

func runAdd(ctx context.Context, file string, add *deploy.AddOptions) error {
	return deploy.Add(ctx, deployOpts(file), add)
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
