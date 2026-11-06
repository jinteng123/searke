package ociimg

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/jint/searke/internal/bundle"
)

// PayloadRoot is the absolute path inside every platform sub-image.
const PayloadRoot = "/opt/searke"

// Annotation keys recorded on the index and sub-image manifests.
const (
	AnnRKE2Version = "io.searke.rke2.version"
	AnnCNI         = "io.searke.cni"
	AnnSource      = "org.opencontainers.image.source"
)

// Meta describes a built multi-arch image.
type Meta struct {
	Version   string            `json:"version" yaml:"version"`
	CNI       string            `json:"cni" yaml:"cni"`
	Platforms []string          `json:"platforms" yaml:"platforms"`
	Digests   map[string]string `json:"digests" yaml:"digests"` // platform -> manifest digest
}

// Build verifies the bundle directory and produces an OCI Image Index
// at outDir (OCI Image Layout, no daemon required). Each platform gets
// its own sub-image containing only that architecture's payload.
func Build(bundleDir, outDir string) (*Meta, error) {
	b, err := bundle.VerifyBundleDir(bundleDir)
	if err != nil {
		return nil, fmt.Errorf("bundle verification failed, refusing to build image: %w", err)
	}
	cni, _ := bundle.NormalizeCNI(b.Spec.CNI)
	platforms, err := b.PlatformList()
	if err != nil {
		return nil, err
	}

	var addenda []mutate.IndexAddendum
	var layerTars []string
	defer func() {
		for _, f := range layerTars {
			os.Remove(f)
		}
	}()
	meta := &Meta{Version: b.Spec.Version, CNI: cni, Digests: map[string]string{}}
	for _, p := range platforms {
		img, digest, tarPath, err := buildPlatformImage(bundleDir, b.Spec.Version, cni, p)
		if err != nil {
			return nil, err
		}
		layerTars = append(layerTars, tarPath)
		meta.Platforms = append(meta.Platforms, p.String())
		meta.Digests[p.String()] = digest.String()
		addenda = append(addenda, mutate.IndexAddendum{
			Add: img,
			Descriptor: v1.Descriptor{
				Platform: &v1.Platform{OS: p.OS, Architecture: p.Arch},
				Annotations: map[string]string{
					AnnRKE2Version: b.Spec.Version,
					AnnCNI:         cni,
				},
			},
		})
	}
	sort.Strings(meta.Platforms)

	ii := mutate.AppendManifests(empty.Index, addenda...)
	ii = mutate.IndexMediaType(ii, types.OCIImageIndex)
	ii = mutate.Annotations(ii, map[string]string{
		AnnRKE2Version: b.Spec.Version,
		AnnCNI:         cni,
		AnnSource:      bundle.ReleaseBase,
	}).(v1.ImageIndex)

	if err := os.RemoveAll(outDir); err != nil {
		return nil, err
	}
	if _, err := layout.Write(outDir, ii); err != nil {
		return nil, fmt.Errorf("write OCI layout: %w", err)
	}
	return meta, nil
}

// buildPlatformImage assembles the single-layer image for one platform.
// The returned tar path backs the layer lazily; the caller must keep it
// until the index has been written, then remove it.
func buildPlatformImage(bundleDir, version, cni string, p bundle.Platform) (v1.Image, v1.Hash, string, error) {
	tarPath, err := writeLayerTar(bundleDir, p)
	if err != nil {
		return nil, v1.Hash{}, "", err
	}
	layer, err := tarball.LayerFromFile(tarPath)
	if err != nil {
		os.Remove(tarPath)
		return nil, v1.Hash{}, "", fmt.Errorf("%s: create layer: %w", p, err)
	}
	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		return nil, v1.Hash{}, "", err
	}
	img = mutate.MediaType(img, types.OCIManifestSchema1)
	img, err = mutate.ConfigFile(img, &v1.ConfigFile{
		OS:           p.OS,
		Architecture: p.Arch,
		Created:      v1.Time{Time: time.Now().UTC()},
		Config: v1.Config{
			Labels: map[string]string{
				AnnRKE2Version: version,
				AnnCNI:         cni,
			},
		},
	})
	if err != nil {
		return nil, v1.Hash{}, "", err
	}
	d, err := img.Digest()
	if err != nil {
		return nil, v1.Hash{}, "", err
	}
	return img, d, tarPath, nil
}

// writeLayerTar streams the platform payload into a tar file. Layout:
//
//	/opt/searke/bundle.yaml
//	/opt/searke/bundle.lock.yaml   (if present)
//	/opt/searke/install.sh
//	/opt/searke/payload/<arch files>
func writeLayerTar(bundleDir string, p bundle.Platform) (string, error) {
	tmp, err := os.CreateTemp("", "searke-layer-*.tar")
	if err != nil {
		return "", err
	}
	defer tmp.Close()
	tw := tar.NewWriter(tmp)

	add := func(src, name string, mode int64) error {
		fi, err := os.Stat(src)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{
			Name:    name,
			Mode:    mode,
			Size:    fi.Size(),
			ModTime: fi.ModTime(),
		}); err != nil {
			return err
		}
		f, err := os.Open(src)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	}

	if err := add(filepath.Join(bundleDir, "bundle.yaml"), "opt/searke/bundle.yaml", 0o644); err != nil {
		return "", err
	}
	lockPath := filepath.Join(bundleDir, "bundle.lock.yaml")
	if _, err := os.Stat(lockPath); err == nil {
		if err := add(lockPath, "opt/searke/bundle.lock.yaml", 0o644); err != nil {
			return "", err
		}
	}
	if err := add(filepath.Join(bundleDir, "install.sh"), "opt/searke/install.sh", 0o755); err != nil {
		return "", err
	}

	payloadDir := filepath.Join(bundleDir, "payload", p.DirName())
	entries, err := os.ReadDir(payloadDir)
	if err != nil {
		return "", fmt.Errorf("%s: %w", p, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if err := add(filepath.Join(payloadDir, e.Name()),
			"opt/searke/payload/"+e.Name(), 0o644); err != nil {
			return "", err
		}
	}
	if err := tw.Close(); err != nil {
		return "", err
	}
	return tmp.Name(), nil
}
