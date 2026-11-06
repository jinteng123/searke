package ociimg

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/jint/searke/internal/bundle"
)

// Source identifies where an OCI image comes from: a local layout dir,
// a local tar archive, or a registry reference.
type Source struct {
	Path string // local directory or tar file
	Ref  string // registry reference
}

// ParseSource classifies a user-supplied bundle reference. Anything that
// exists on disk is treated as a local path; otherwise it must parse as
// an OCI reference.
func ParseSource(s string) (Source, error) {
	if s == "" {
		return Source{}, fmt.Errorf("empty bundle reference")
	}
	if _, err := os.Stat(s); err == nil {
		return Source{Path: s}, nil
	}
	if _, err := name.ParseReference(s); err != nil {
		return Source{}, fmt.Errorf("%q is neither a local path nor a valid registry reference: %w", s, err)
	}
	return Source{Ref: s}, nil
}

// OpenIndex resolves the source to an image index. When a tar file is
// used it is unpacked to a temp dir; the returned cleanup removes it.
func (s Source) OpenIndex() (v1.ImageIndex, func(), error) {
	cleanup := func() {}
	if s.Ref != "" {
		ref, err := name.ParseReference(s.Ref)
		if err != nil {
			return nil, cleanup, err
		}
		ii, err := remote.Index(ref, remote.WithAuthFromKeychain(authn.DefaultKeychain))
		return ii, cleanup, err
	}
	fi, err := os.Stat(s.Path)
	if err != nil {
		return nil, cleanup, err
	}
	path := s.Path
	if !fi.IsDir() {
		tmp, err := os.MkdirTemp("", "searke-oci-*")
		if err != nil {
			return nil, cleanup, err
		}
		if err := Untar(path, tmp); err != nil {
			os.RemoveAll(tmp)
			return nil, cleanup, fmt.Errorf("unpack %s: %w", path, err)
		}
		cleanup = func() { os.RemoveAll(tmp) }
		path = tmp
	}
	ii, err := layout.ImageIndexFromPath(path)
	if err != nil {
		cleanup()
		return nil, cleanup, fmt.Errorf("read OCI layout %s: %w", path, err)
	}
	return ii, cleanup, nil
}

// IndexMeta returns version/CNI/platform info recorded on the index.
func IndexMeta(ii v1.ImageIndex) (*Meta, error) {
	im, err := ii.IndexManifest()
	if err != nil {
		return nil, err
	}
	m := &Meta{Digests: map[string]string{}}
	if im.Annotations != nil {
		m.Version = im.Annotations[AnnRKE2Version]
		m.CNI = im.Annotations[AnnCNI]
	}
	for _, d := range im.Manifests {
		if d.Platform == nil {
			continue
		}
		ps := d.Platform.OS + "/" + d.Platform.Architecture
		m.Platforms = append(m.Platforms, ps)
		m.Digests[ps] = d.Digest.String()
		if m.Version == "" && d.Annotations != nil {
			m.Version = d.Annotations[AnnRKE2Version]
			m.CNI = d.Annotations[AnnCNI]
		}
	}
	return m, nil
}

// ExtractPayload unpacks the files of the sub-image matching platform
// into destDir, preserving the /opt/searke/... layout. The payload is
// verified against the embedded sha256sum manifest before returning.
func ExtractPayload(ii v1.ImageIndex, p bundle.Platform, destDir string) error {
	im, err := ii.IndexManifest()
	if err != nil {
		return err
	}
	var desc *v1.Descriptor
	for i, d := range im.Manifests {
		if d.Platform != nil && d.Platform.OS == p.OS && d.Platform.Architecture == p.Arch {
			desc = &im.Manifests[i]
			break
		}
	}
	if desc == nil {
		var have []string
		for _, d := range im.Manifests {
			if d.Platform != nil {
				have = append(have, d.Platform.OS+"/"+d.Platform.Architecture)
			}
		}
		return fmt.Errorf("image index has no sub-image for %s (available: %s)", p, strings.Join(have, ", "))
	}
	img, err := ii.Image(desc.Digest)
	if err != nil {
		return err
	}
	layers, err := img.Layers()
	if err != nil {
		return err
	}
	for _, l := range layers {
		rc, err := l.Uncompressed()
		if err != nil {
			return err
		}
		if err := untarInto(rc, destDir); err != nil {
			rc.Close()
			return err
		}
		rc.Close()
	}
	return nil
}

// untarInto extracts a tar stream under destDir, stripping the leading
// "/" so /opt/searke/x becomes <destDir>/opt/searke/x. Path traversal
// outside destDir is rejected.
func untarInto(r io.Reader, destDir string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(filepath.Clean(hdr.Name), string(filepath.Separator))
		target := filepath.Join(destDir, name)
		if !strings.HasPrefix(target, filepath.Clean(destDir)+string(filepath.Separator)) {
			return fmt.Errorf("tar entry %q escapes destination", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		}
	}
}

// Push publishes a local OCI layout directory to a registry reference.
func Push(layoutDir, refStr string) error {
	ii, err := layout.ImageIndexFromPath(layoutDir)
	if err != nil {
		return fmt.Errorf("read OCI layout %s: %w", layoutDir, err)
	}
	ref, err := name.ParseReference(refStr)
	if err != nil {
		return err
	}
	return remote.WriteIndex(ref, ii, remote.WithAuthFromKeychain(authn.DefaultKeychain))
}

// PullIndex fetches an index from a registry (for save/inspect).
func PullIndex(refStr string) (v1.ImageIndex, error) {
	ref, err := name.ParseReference(refStr)
	if err != nil {
		return nil, err
	}
	return remote.Index(ref, remote.WithAuthFromKeychain(authn.DefaultKeychain))
}

// SaveTar writes an image index as a .tar archive containing an OCI
// Image Layout (oci-layout, index.json, blobs/).
func SaveTar(ii v1.ImageIndex, outPath string) error {
	tmp, err := os.MkdirTemp("", "searke-layout-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if _, err := layout.Write(tmp, ii); err != nil {
		return err
	}
	return TarDir(tmp, outPath)
}

// LoadTar unpacks a saved .tar into an OCI layout directory.
func LoadTar(tarPath, outDir string) error {
	tmp, err := os.MkdirTemp("", "searke-load-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := Untar(tarPath, tmp); err != nil {
		return err
	}
	// sanity check the layout
	if _, err := layout.ImageIndexFromPath(tmp); err != nil {
		return fmt.Errorf("%s does not contain a valid OCI image layout: %w", tarPath, err)
	}
	if err := prepareLayoutDir(outDir); err != nil {
		return err
	}
	return copyDir(tmp, outDir)
}

// TarDir streams a directory tree into a tar file.
func TarDir(srcDir, outPath string) error {
	out, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer out.Close()
	tw := tar.NewWriter(out)
	defer tw.Close()
	return filepath.Walk(srcDir, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil || rel == "." {
			return err
		}
		hdr, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if fi.IsDir() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
}

// Untar extracts a (optionally gzipped) tar file into destDir.
func Untar(tarPath, destDir string) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()
	var r io.Reader = f
	br, err := peekGzip(f)
	if err != nil {
		return err
	}
	if br {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer gz.Close()
		r = gz
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	return untarInto(r, destDir)
}

func peekGzip(f *os.File) (bool, error) {
	buf := make([]byte, 2)
	n, err := f.ReadAt(buf, 0)
	if err != nil && n < 2 {
		return false, nil
	}
	return buf[0] == 0x1f && buf[1] == 0x8b, nil
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if fi.IsDir() {
			return os.MkdirAll(target, fi.Mode())
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fi.Mode())
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, in)
		return err
	})
}

// PullToLayout downloads a remote index into a local layout directory.
func PullToLayout(refStr, outDir string) error {
	ii, err := PullIndex(refStr)
	if err != nil {
		return err
	}
	if err := prepareLayoutDir(outDir); err != nil {
		return err
	}
	_, err = layout.Write(outDir, ii)
	return err
}
