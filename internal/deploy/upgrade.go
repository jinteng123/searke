package deploy

import (
	"context"
	"fmt"
	"time"

	"github.com/jint/searke/internal/cluster"
)

// Upgrade performs a rolling RKE2 upgrade: all servers first (one at a
// time), then agents. The bundle in cluster.yaml carries the target
// version; the recorded state keeps the previous one until finished.
func Upgrade(ctx context.Context, opts *Options) error {
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
	logf(opts, "upgrading cluster %s: %s -> %s", c.Name(), st.Version, target)

	rts, err := connectAll(c, opts)
	if err != nil {
		return err
	}
	defer closeAll(rts)

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

	first := findRuntime(rts, c.FirstServer().Name)
	token, err := fetchToken(first)
	if err != nil {
		return fmt.Errorf("cannot proceed without node token: %w", err)
	}

	for _, r := range order {
		if r.report.InstalledVersion == target {
			logf(opts, "  %s: already %s, skipping", r.node.Name, target)
			continue
		}
		if r.report.Service == "" {
			return fmt.Errorf("%s: no existing rke2 install found; run `searke apply` instead", r.node.Name)
		}
		if err := upgradeNode(ctx, c, rb, r, token, target, opts); err != nil {
			return err
		}
		// Wait for the node to come back Ready before moving on (except
		// when the node under upgrade IS the kubectl host).
		if r.node.Name != first.node.Name {
			if err := waitReady(ctx, first, map[string]string{r.report.Hostname: r.node.Name}, opts); err != nil {
				return fmt.Errorf("%s: node did not return Ready: %w", r.node.Name, err)
			}
		} else {
			if err := waitFor(ctx, 120*time.Second, func() (bool, error) {
				res, _ := r.client.RunSudo("systemctl is-active rke2-server")
				return res != nil && res.ExitCode == 0, nil
			}); err != nil {
				return fmt.Errorf("%s: rke2-server did not come back: %w", r.node.Name, err)
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

// upgradeNode ships the new payload, re-renders config, reruns
// install.sh (which replaces the binaries) and restarts the service.
func upgradeNode(ctx context.Context, c *cluster.Cluster, rb *ResolvedBundle, r *nodeRuntime, token, target string, opts *Options) error {
	name := r.node.Name
	isFirst := name == c.FirstServer().Name

	if err := distribute(ctx, c, rb, r, opts); err != nil {
		return err
	}
	cfg, err := renderConfig(c, r.node, r.role, c.RegistrationURL(), token, isFirst)
	if err != nil {
		return err
	}
	if err := r.client.WriteFile(RKE2ConfigPath, cfg, 0o600); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	logf(opts, "  %s: rerun install.sh for %s", name, target)
	res, err := r.client.RunSudo(fmt.Sprintf(
		"INSTALL_RKE2_ARTIFACT_PATH=%s INSTALL_RKE2_TYPE=%s INSTALL_RKE2_SKIP_START=true INSTALL_RKE2_VERSION=%s sh %s",
		shellQuote(RemoteArtifacts), r.role, shellQuote(target), RemoteRoot+"/install.sh"))
	if err != nil {
		return fmt.Errorf("%s: install.sh: %w", name, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%s: install.sh failed (exit %d): %s", name, res.ExitCode, tail(res.Stderr+res.Stdout, 800))
	}
	svc := "rke2-" + r.role
	if res, err := r.client.RunSudo("systemctl restart " + svc); err != nil {
		return fmt.Errorf("%s: restart %s: %w", name, svc, err)
	} else if res.ExitCode != 0 {
		return fmt.Errorf("%s: restart %s: %s", name, svc, res.Stderr)
	}
	return nil
}
