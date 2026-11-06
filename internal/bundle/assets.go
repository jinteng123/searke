package bundle

import (
	"fmt"
	"strings"
)

// ReleaseBase is the upstream source of all RKE2 artifacts.
// It is a var so tests can point the downloader at a fake server.
var ReleaseBase = "https://github.com/rancher/rke2"

// RawBase serves repo files at a tag (install.sh).
var RawBase = "https://raw.githubusercontent.com/rancher/rke2"

// ScriptMirrors are fallback URLs for install.sh used when the primary
// raw.githubusercontent.com source is unreachable. All mirrors serve
// the identical file from the pinned git tag; the fetched content is
// hashed into bundle.lock.yaml either way.
var ScriptMirrors = []string{
	"https://cdn.jsdelivr.net/gh/rancher/rke2@%s/install.sh",
}

// ReleaseMirrors wrap a full GitHub release asset URL (%s = the
// primary URL). They serve byte-identical release artifacts; the
// upstream sha256sum manifest still gates every download.
var ReleaseMirrors = []string{
	"https://gh-proxy.com/%s",
	"https://ghproxy.net/%s",
}

// mirrorCandidates returns url plus its wrapped mirror URLs.
func mirrorCandidates(url string) []string {
	out := []string{url}
	for _, tmpl := range ReleaseMirrors {
		out = append(out, fmt.Sprintf(tmpl, url))
	}
	return out
}

// EscapedTag returns the release tag with '+' percent-encoded, as required
// by GitHub release asset URLs (v1.36.5+rke2r1 -> v1.36.5%2Brke2r1).
func EscapedTag(version string) string {
	return strings.ReplaceAll(version, "+", "%2B")
}

// InstallScriptURL returns the primary raw URL of install.sh for the
// pinned tag.
func InstallScriptURL(version string) string {
	return RawBase + "/" + EscapedTag(version) + "/install.sh"
}

// InstallScriptURLs returns the primary URL followed by mirror URLs.
func InstallScriptURLs(version string) []string {
	urls := []string{InstallScriptURL(version)}
	for _, tmpl := range ScriptMirrors {
		urls = append(urls, fmt.Sprintf(tmpl, EscapedTag(version)))
	}
	return urls
}

// releaseURL builds the release asset URL for a file name.
func releaseURL(version, name string) string {
	return fmt.Sprintf("%s/releases/download/%s/%s", ReleaseBase, EscapedTag(version), name)
}

// Asset is a single downloadable file for a platform (or shared).
type Asset struct {
	Name     string // file name inside payload dir
	Platform string // "" for shared files like install.sh
	URL      string
	// URLs lists mirror candidates; the first is primary. When empty,
	// URL is used alone.
	URLs []string
	// Required reports whether a missing asset is fatal.
	Required bool
}

// Candidates returns every URL to try, in order.
func (a Asset) Candidates() []string {
	if len(a.URLs) > 0 {
		return a.URLs
	}
	return []string{a.URL}
}

// AssetsFor computes the required artifact list for one platform.
// Canal (the default) uses the combined rke2-images archive; other CNIs
// need rke2-images-core plus the CNI-specific archive.
func AssetsFor(version, cni string, p Platform) []Asset {
	arch := p.Arch
	mk := func(name string) Asset {
		u := releaseURL(version, name)
		return Asset{
			Name:     name,
			Platform: p.String(),
			URL:      u,
			URLs:     mirrorCandidates(u),
			Required: true,
		}
	}
	assets := []Asset{
		mk(fmt.Sprintf("rke2.linux-%s.tar.gz", arch)),
		mk(fmt.Sprintf("sha256sum-%s.txt", arch)),
	}
	if cni == "canal" {
		assets = append(assets, mk(fmt.Sprintf("rke2-images.linux-%s.tar.zst", arch)))
	} else {
		for _, name := range []string{
			fmt.Sprintf("rke2-images-core.linux-%s.tar.zst", arch),
			fmt.Sprintf("rke2-images-%s.linux-%s.tar.zst", cni, arch),
		} {
			assets = append(assets, mk(name))
		}
	}
	return assets
}

// SharedAssets lists non-platform files (the installer).
func SharedAssets(version string) []Asset {
	return []Asset{{
		Name:     "install.sh",
		URL:      InstallScriptURL(version),
		URLs:     InstallScriptURLs(version),
		Required: true,
	}}
}

// RequiredFiles returns the payload file names a verified bundle must
// contain for the platform (excluding the checksum manifest itself).
func RequiredFiles(cni string, p Platform) []string {
	var out []string
	for _, a := range AssetsFor("v0.0.0+rke2r0", cni, p) {
		// Name does not depend on version; reuse the naming logic.
		out = append(out, a.Name)
	}
	return out
}
