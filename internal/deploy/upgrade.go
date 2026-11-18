package deploy

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jint/searke/internal/bundle"
	"github.com/jint/searke/internal/cluster"
)

var versionNumRe = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)`)

// cgroupV1NeedsOverride reports whether the version's kubelet refuses
// cgroup v1 hosts by default: Kubernetes 1.35 flipped failCgroupV1 to
// true (KEP-5573). Earlier releases run on v1 with only a warning.
func cgroupV1NeedsOverride(version string) bool {
	m := versionNumRe.FindStringSubmatch(version)
	if m == nil {
		return false
	}
	maj, _ := strconv.Atoi(m[1])
	min, _ := strconv.Atoi(m[2])
	return maj > 1 || min >= 35
}

// failCgroupV1Arg is the kubelet flag that keeps cgroup v1 hosts
// working on Kubernetes >= 1.35. It is not deprecated, unlike most
// KubeletConfiguration flags, and exists since 1.31.
const failCgroupV1Arg = "fail-cgroupv1"

// userFailCgroupV1 returns the user's explicit fail-cgroupv1 value from
// rke2Config kubelet-arg, if any.
func userFailCgroupV1(c *cluster.Cluster) (string, bool) {
	for _, a := range toStringSlice(c.Spec.RKE2Config["kubelet-arg"]) {
		k, v, _ := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if k == failCgroupV1Arg {
			if v == "" {
				v = "true"
			}
			return v, true
		}
	}
	return "", false
}

// checkCgroupV1 keeps cgroup v1 nodes installable. On targets whose
// kubelet refuses v1 by default, renderConfig adds fail-cgroupv1=false
// for those nodes; this only warns, or refuses when the user pinned
// fail-cgroupv1=true, since kubelet would then crash-loop after install.
func checkCgroupV1(c *cluster.Cluster, rts []*nodeRuntime, version string, opts *Options) error {
	var v1 []string
	for _, r := range rts {
		if !r.report.CgroupV2 {
			v1 = append(v1, r.node.Name)
		}
	}
	if len(v1) == 0 || !cgroupV1NeedsOverride(version) {
		return nil
	}
	nodes := strings.Join(v1, ", ")
	if v, ok := userFailCgroupV1(c); ok && v != "false" {
		return fmt.Errorf("nodes %s run cgroup v1 but rke2Config kubelet-arg sets %s=%s, so kubelet %s would refuse to start: "+
			"remove that arg, or boot with systemd.unified_cgroup_hierarchy=1", nodes, failCgroupV1Arg, v, version)
	}
	logf(opts, "  warning: nodes %s run cgroup v1; adding kubelet-arg %s=false for %s "+
		"(cgroup v1 is deprecated upstream; migrate to cgroup v2 before Kubernetes removes it)", nodes, failCgroupV1Arg, version)
	return nil
}

// checkUpgradePath validates an RKE2 version transition: same major,
// at most one minor ahead, never a downgrade. Patch and +rke2rN
// revision bumps within a minor are upgrades too.
func checkUpgradePath(from, to string) error {
	fm := versionNumRe.FindStringSubmatch(from)
	tm := versionNumRe.FindStringSubmatch(to)
	if fm == nil || tm == nil {
		return fmt.Errorf("cannot compare versions %q -> %q", from, to)
	}
	if fm[1] != tm[1] {
		return fmt.Errorf("major version change %s -> %s is not supported", from, to)
	}
	fMin, _ := strconv.Atoi(fm[2])
	fPatch, _ := strconv.Atoi(fm[3])
	tMin, _ := strconv.Atoi(tm[2])
	tPatch, _ := strconv.Atoi(tm[3])
	switch {
	case tMin < fMin, tMin == fMin && tPatch < fPatch:
		return fmt.Errorf("downgrades are not supported (%s -> %s)", from, to)
	case tMin == fMin && tPatch == fPatch:
		// Same k8s version — only an +rke2rN revision bump reaches
		// here (equal strings were rejected earlier).
		return nil
	case tMin-fMin > 1:
		return fmt.Errorf("upgrade %s -> %s skips %d minor versions; RKE2 supports one at a time",
			from, to, tMin-fMin)
	}
	return nil
}

// UpgradeOptions controls an upgrade run.
type UpgradeOptions struct {
	SkipDrain    bool // do not cordon/drain nodes around the restart
	SkipSnapshot bool // do not take an etcd snapshot first
}

// Upgrade performs a rolling RKE2 upgrade: all servers first (one at a
// time), then agents. The bundle in cluster.yaml carries the target
// version; the recorded state keeps the previous one until finished.
func Upgrade(ctx context.Context, opts *Options, uo UpgradeOptions) error {
	c, rb, err := loadAndResolve(opts)
	if err != nil {
		return err
	}
	defer rb.Close()
	target := rb.Meta.Version

	st, err := LoadStateUnchecked(StatePath(stateDir(opts), c.Name()), c.Name())
	if err != nil {
		return err
	}
	if st.Version == "" {
		return fmt.Errorf("no recorded cluster state; run `searke apply` first")
	}
	if st.Version == target {
		return fmt.Errorf("cluster is already at %s; nothing to do (use apply to converge)", target)
	}
	if err := checkUpgradePath(st.Version, target); err != nil {
		return err
	}
	logf(opts, "upgrading cluster %s: %s -> %s", c.Name(), st.Version, target)

	rts, err := connectAll(c, opts)
	if err != nil {
		return err
	}
	defer closeAll(rts)
	for _, r := range rts {
		for _, w := range r.report.Warnings {
			logf(opts, "  warning %s: %s", r.node.Name, w)
		}
	}
	if err := checkCgroupV1(c, rts, target, opts); err != nil {
		return err
	}

	first := findRuntime(rts, c.FirstServer().Name)
	// The token read races a still-starting apiserver; wait for the
	// bootstrap to actually serve before touching the cluster.
	if err := waitServerReadyContext(ctx, first.client, 10*time.Minute, func(msg string) {
		logf(opts, "  [%s] %s", first.node.Name, msg)
	}); err != nil {
		return fmt.Errorf("%s: bootstrap server not ready: %w", first.node.Name, err)
	}
	token, err := fetchToken(first)
	if err != nil {
		return fmt.Errorf("cannot proceed without node token: %w", err)
	}

	// Safety snapshot before touching anything.
	if !uo.SkipSnapshot && len(c.Spec.Servers) > 0 {
		logf(opts, "== pre-upgrade etcd snapshot ==")
		if err := SnapshotSave(opts, ""); err != nil {
			return fmt.Errorf("pre-upgrade snapshot failed (use --skip-snapshot to bypass): %w", err)
		}
	}

	scratch, err := os.MkdirTemp("", "searke-payload-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	payloadDirs := map[bundle.Platform]string{}

	// Order: servers by name, then agents by name.
	var order []*nodeRuntime
	for _, r := range rts {
		if r.role == "server" {
			order = append(order, r)
		}
	}
	for _, r := range rts {
		if r.role == "agent" {
			order = append(order, r)
		}
	}

	// A single-node cluster has nowhere to evacuate pods; draining it
	// only stalls the upgrade. Skip it unless the user asked anyway.
	singleNode := len(order) == 1
	if singleNode && !uo.SkipDrain {
		logf(opts, "single-node cluster: skipping drain (pods have nowhere to evacuate)")
	}

	for _, r := range order {
		if r.report.InstalledVersion == target {
			logf(opts, "  %s: already %s, skipping", r.node.Name, target)
			// Heal cordon state left by an interrupted earlier run;
			// uncordon on a schedulable node is a no-op.
			if !uo.SkipDrain && !singleNode {
				if err := drainNode(first, r, false, opts); err != nil {
					logf(opts, "  warning %s: uncordon failed: %v", r.node.Name, err)
				}
			}
			continue
		}
		if r.report.Service == "" {
			return fmt.Errorf("%s: no existing rke2 install found; run `searke apply` instead", r.node.Name)
		}
		if !uo.SkipDrain && !singleNode {
			if err := drainNode(first, r, true, opts); err != nil {
				return fmt.Errorf("%s: drain failed (use --skip-drain to bypass): %w", r.node.Name, err)
			}
		}
		if err := upgradeNode(ctx, c, rb, r, token, target, scratch, payloadDirs, opts); err != nil {
			return err
		}
		// Wait for the node to come back before moving on.
		if r.node.Name != first.node.Name {
			if err := waitReady(ctx, first, map[string]string{r.report.Hostname: r.node.Name}, opts); err != nil {
				return fmt.Errorf("%s: node did not return Ready: %w", r.node.Name, err)
			}
		} else {
			// The bootstrap server must actually serve before the
			// next node's drain/kubectl calls — service active alone
			// races ahead of the apiserver.
			if err := waitServerReadyContext(ctx, r.client, 10*time.Minute, func(msg string) {
				logf(opts, "  [%s] %s", r.node.Name, msg)
			}); err != nil {
				return fmt.Errorf("%s: rke2-server did not come back: %w", r.node.Name, err)
			}
		}
		if !uo.SkipDrain && !singleNode {
			if err := drainNode(first, r, false, opts); err != nil {
				logf(opts, "  warning %s: uncordon failed: %v", r.node.Name, err)
			}
		}
		logf(opts, "  %s: now %s", r.node.Name, target)
	}

	prev := st.Version
	st.Version = target
	st.Bundle = c.Spec.RKE2.Bundle
	if err := st.Log("upgrade", prev+" -> "+target); err != nil {
		return err
	}
	if err := st.Save(); err != nil {
		return err
	}
	logf(opts, "upgrade complete: %s -> %s", prev, target)
	return nil
}

// drainNode cordons+drains (drain=true) or uncordons a node via kubectl
// on the bootstrap server.
func drainNode(first *nodeRuntime, r *nodeRuntime, drain bool, opts *Options) error {
	kctl := fmt.Sprintf("%s --kubeconfig %s --request-timeout=90s", kubectlPath, kubeconfigPath)
	var cmd string
	if drain {
		logf(opts, "  %s: drain", r.node.Name)
		cmd = fmt.Sprintf("%s drain %s --ignore-daemonsets --delete-emptydir-data --timeout=120s",
			kctl, shellQuote(r.node.Name))
	} else {
		cmd = fmt.Sprintf("%s uncordon %s", kctl, shellQuote(r.node.Name))
	}
	res, err := first.client.RunSudo(cmd)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("exit %d: %s", res.ExitCode, tail(res.Stderr+res.Stdout, 400))
	}
	return nil
}

// upgradeNode ships the new payload, re-renders config, reruns
// install.sh (which replaces the binaries) and restarts the service.
func upgradeNode(ctx context.Context, c *cluster.Cluster, rb *ResolvedBundle, r *nodeRuntime,
	token, target, scratch string, cache map[bundle.Platform]string, opts *Options) error {
	name := r.node.Name
	isFirst := name == c.FirstServer().Name

	if err := distribute(ctx, c, rb, r, scratch, cache, opts); err != nil {
		return err
	}
	cfg, err := renderConfig(c, r.node, r.role, c.RegistrationURL(), token, isFirst,
		!r.report.CgroupV2 && cgroupV1NeedsOverride(target))
	if err != nil {
		return err
	}
	if err := r.client.WriteFile(RKE2ConfigPath, cfg, 0o600); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	logf(opts, "  %s: rerun install.sh for %s", name, target)
	res, err := runSudoStreamingWithHeartbeat(ctx, r.client, fmt.Sprintf(
		"INSTALL_RKE2_ARTIFACT_PATH=%s INSTALL_RKE2_TYPE=%s INSTALL_RKE2_VERSION=%s sh %s",
		shellQuote(RemoteArtifacts), r.role, shellQuote(target), RemoteRoot+"/install.sh"),
		opts, name, "running install.sh", 10*time.Second)
	if err != nil {
		return fmt.Errorf("%s: install.sh: %w", name, err)
	}
	if res == nil || res.ExitCode != 0 {
		out := ""
		if res != nil {
			out = res.Stderr + res.Stdout
		}
		return fmt.Errorf("%s: install.sh failed: %s", name, tail(out, 800))
	}
	svc := "rke2-" + r.role
	res, err = runSudoStreaming(ctx, r.client, "systemctl restart "+shellQuote(svc), opts, name)
	if err != nil {
		return fmt.Errorf("%s: restart %s: %w", name, svc, err)
	}
	if res == nil || res.ExitCode != 0 {
		out := ""
		if res != nil {
			out = res.Stderr + res.Stdout
		}
		return fmt.Errorf("%s: restart %s failed: %s", name, svc, tail(out, 400))
	}
	return nil
}
