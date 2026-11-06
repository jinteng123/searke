package cli

import (
	"context"
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

func runImageBuild(bundleDir, tag, outDir string) error {
	if outDir == "" {
		outDir = strings.TrimSuffix(bundleDir, "/") + "-oci"
	}
	meta, err := ociimg.Build(bundleDir, outDir)
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

func runImagePush(from, ref string) error {
	if err := ociimg.Push(from, ref); err != nil {
		return err
	}
	logf("pushed %s -> %s", from, ref)
	return nil
}

func runImageSave(src, out string) error {
	s, err := ociimg.ParseSource(src)
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

func runStatus(file string) error {
	return deploy.Status(deployOpts(file))
}

