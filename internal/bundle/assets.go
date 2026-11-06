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

// APIBase is the GitHub REST API root for the repo. API endpoints are
// the ONLY allowed fallback for trust-anchor files (checksum manifests
// and install.sh): they are served over TLS by GitHub itself, so a
// compromised mirror cannot forge matching checksums.
var APIBase = "https://api.github.com/repos/rancher/rke2"

// ReleaseMirrors wrap a full GitHub release asset URL (%s = the
// primary URL). Mirrors only ever serve the large binary/image
// payloads, never the checksum manifests or install.sh: those must
// come from GitHub-controlled endpoints, and every mirrored byte is
// verified against the trusted manifest before use.
var ReleaseMirrors = []string{
	"https://gh-proxy.com/%s",
	"https://ghproxy.net/%s",
}


// apiContentsURL fetches a repo file through the GitHub contents API.
// The JSON response carries base64 content — downloadOnce decodes it.
func apiContentsURL(version, path string) string {
	return fmt.Sprintf("%s/contents/%s?ref=%s", APIBase, path, EscapedTag(version))
}

// apiReleaseURL returns the release metadata endpoint for the tag.
func apiReleaseURL(version string) string {
	return fmt.Sprintf("%s/releases/tags/%s", APIBase, EscapedTag(version))
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

// InstallScriptURLs returns the raw URL followed by the trusted
// contents-API fallback. No third-party mirrors: install.sh runs as
// root on every node.
func InstallScriptURLs(version string) []string {
	return []string{
		InstallScriptURL(version),
		apiContentsURL(version, "install.sh"),
	}
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
	// TrustAnchor marks files that establish integrity for everything
	// else (checksum manifests, install.sh). They may only be fetched
	// from GitHub-controlled endpoints — never third-party mirrors.
	TrustAnchor bool
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
	mk := func(name string, anchor bool) Asset {
		return Asset{
			Name:        name,
			Platform:    p.String(),
			URL:         releaseURL(version, name),
			TrustAnchor: anchor,
			Required:    true,
		}
	}
	assets := []Asset{
		mk(fmt.Sprintf("sha256sum-%s.txt", arch), true),
		mk(fmt.Sprintf("rke2.linux-%s.tar.gz", arch), false),
	}
	if cni == "canal" {
		assets = append(assets, mk(fmt.Sprintf("rke2-images.linux-%s.tar.zst", arch), false))
	} else {
		for _, name := range []string{
			fmt.Sprintf("rke2-images-core.linux-%s.tar.zst", arch),
			fmt.Sprintf("rke2-images-%s.linux-%s.tar.zst", cni, arch),
		} {
			assets = append(assets, mk(name, false))
		}
	}
	return assets
}

// SharedAssets lists non-platform files (the installer).
func SharedAssets(version string) []Asset {
	return []Asset{{
		Name:        "install.sh",
		URL:         InstallScriptURL(version),
		URLs:        InstallScriptURLs(version),
		TrustAnchor: true,
		Required:    true,
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
