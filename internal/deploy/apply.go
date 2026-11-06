package deploy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/jint/searke/internal/bundle"
	"github.com/jint/searke/internal/cluster"
	"github.com/jint/searke/internal/ociimg"
)

// Remote layout on each node:
//
//	/opt/searke/install.sh           upstream installer
//	/opt/searke/artifacts/<files>    INSTALL_RKE2_ARTIFACT_PATH target
//	/etc/rancher/rke2/config.yaml    rendered node config
const (
	RemoteRoot      = "/opt/searke"
	RemoteArtifacts = RemoteRoot + "/artifacts"
	RKE2ConfigPath  = "/etc/rancher/rke2/config.yaml"
	nodeTokenPath   = "/var/lib/rancher/rke2/server/node-token"
	kubectlPath     = "/var/lib/rancher/rke2/bin/kubectl"
	kubeconfigPath  = "/etc/rancher/rke2/rke2.yaml"
)

// Options controls Validate/Plan/Apply runs.
type Options struct {
	ClusterFile string
	StateDir    string // defaults to the cluster file's directory
	Log         func(format string, args ...any)
}

// ResolvedBundle pairs the opened index with its metadata.
type ResolvedBundle struct {
	Source ociimg.Source
	Meta   *ociimg.Meta
	Close  func()
}

// nodeRuntime collects per-node run state.
type nodeRuntime struct {
	node   cluster.Node
	role   string
	client *Client
	report *PreflightReport
}

func logf(o *Options, f string, a ...any) {
	if o.Log != nil {
		o.Log(f, a...)
	}
}

func loadAndResolve(opts *Options) (*cluster.Cluster, *ResolvedBundle, error) {
	c, err := cluster.Load(opts.ClusterFile)
	if err != nil {
		return nil, nil, err
	}
	src, err := ociimg.ParseSource(c.Spec.RKE2.Bundle)
	if err != nil {
		return nil, nil, err
	}
	ii, cleanup, err := src.OpenIndex()
	if err != nil {
		return nil, nil, fmt.Errorf("open bundle %s: %w", c.Spec.RKE2.Bundle, err)
	}
	meta, err := ociimg.IndexMeta(ii)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	if meta.Version == "" {
		cleanup()
		return nil, nil, fmt.Errorf("bundle %s carries no %s annotation; only images built by `searke image build` are supported",
			c.Spec.RKE2.Bundle, ociimg.AnnRKE2Version)
	}
	if v := c.Spec.RKE2.Version; v != "" && v != meta.Version {
		cleanup()
		return nil, nil, fmt.Errorf("cluster requires RKE2 %s but bundle contains %s", v, meta.Version)
	}
	if cni := c.Spec.RKE2.CNI; cni != "" && meta.CNI != "" && cni != meta.CNI {
		cleanup()
		return nil, nil, fmt.Errorf("cluster requires cni %s but bundle was built for %s", cni, meta.CNI)
	}
	return c, &ResolvedBundle{Source: src, Meta: meta, Close: func() { cleanup() }}, nil
}

func stateDir(opts *Options) string {
	if opts.StateDir != "" {
		return opts.StateDir
	}
	abs, err := filepath.Abs(opts.ClusterFile)
	if err != nil {
		return "."
	}
	return filepath.Dir(abs)
}

// connectAll SSHes into every node and runs preflight.
func connectAll(c *cluster.Cluster, opts *Options) ([]*nodeRuntime, error) {
	sshCfg := SSHConfig{
		User:         c.Spec.SSH.User,
		Port:         c.Spec.SSH.Port,
		IdentityFile: c.Spec.SSH.IdentityFile,
		Sudo:         c.Spec.SSH.Sudo,
	}
	hostnames := map[string]string{}
	var runtimes []*nodeRuntime
	for _, n := range c.AllNodes() {
		role := c.RoleOf(n.Name)
		logf(opts, "  connect %s (%s)", n.Name, n.Address)
		cl, err := Connect(n.Address, sshCfg)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", n.Name, err)
		}
		rep, err := Preflight(cl, n.Name)
		if err != nil {
			cl.Close()
			return nil, err
		}
		if prev, dup := hostnames[rep.Hostname]; dup {
			return nil, fmt.Errorf("nodes %s and %s share hostname %q; hostnames must be unique", prev, n.Name, rep.Hostname)
		}
		hostnames[rep.Hostname] = n.Name
		if err := rep.CheckConsistency(n.Name, c.RegistrationURL()); err != nil {
			return nil, err
		}
		runtimes = append(runtimes, &nodeRuntime{node: n, role: role, client: cl, report: rep})
	}
	return runtimes, nil
}

func closeAll(rts []*nodeRuntime) {
	for _, r := range rts {
		r.client.Close()
	}
}

// Validate checks the cluster file, the bundle, SSH connectivity and
// remote preflight. No changes are made.
func Validate(opts *Options) error {
	c, rb, err := loadAndResolve(opts)
	if err != nil {
		return err
	}
	defer rb.Close()
	logf(opts, "cluster %s: %d server(s), %d agent(s), bundle %s (RKE2 %s, cni %s)",
		c.Name(), len(c.Spec.Servers), len(c.Spec.Agents), c.Spec.RKE2.Bundle, rb.Meta.Version, rb.Meta.CNI)
	rts, err := connectAll(c, opts)
	if err != nil {
		return err
	}
	defer closeAll(rts)
	for _, r := range rts {
		status := "clean"
		if r.report.HasRKE2 || r.report.Service != "" {
			status = fmt.Sprintf("existing install (%s %s)", r.report.Service, r.report.ServiceState)
		}
		logf(opts, "  ok %s: %s %s, %s, disk %d MiB free, %s",
			r.node.Name, r.report.OS, r.report.OSVersion, r.report.Arch, r.report.DiskAvailKiB/1024, status)
	}
	return nil
}

// Plan performs Validate then prints the actions apply would take.
func Plan(opts *Options) error {
	c, rb, err := loadAndResolve(opts)
	if err != nil {
		return err
	}
	defer rb.Close()
	st, err := LoadState(StatePath(stateDir(opts), c.Name()), c.Name(), c.Spec.RKE2.Bundle, rb.Meta.Version, rb.Meta.CNI)
	if err != nil {
		return err
	}
	rts, err := connectAll(c, opts)
	if err != nil {
		return err
	}
	defer closeAll(rts)
	logf(opts, "plan for cluster %s (RKE2 %s, cni %s):", c.Name(), rb.Meta.Version, rb.Meta.CNI)
	for _, r := range rts {
		ns, known := st.Node(r.node.Name)
		var acts []string
		switch {
		case r.report.Service != "" && r.report.ServiceState == "active":
			acts = append(acts, "keep existing install (verify config consistency only)")
		default:
			if !known || !st.Done(r.node.Name, StageDistributed) {
				acts = append(acts, "upload "+r.report.Arch+" payload")
			}
			if !known || !st.Done(r.node.Name, StageConfigured) {
				acts = append(acts, "write "+RKE2ConfigPath)
			}
			if !known || !st.Done(r.node.Name, StageInstalled) {
				acts = append(acts, "run install.sh ("+r.role+")")
			}
			acts = append(acts, "enable+start rke2-"+r.role)
		}
		if known {
			acts = append(acts, "recorded stage: "+ns.Stage)
		}
		logf(opts, "  %s (%s, %s): %s", r.node.Name, r.role, r.node.Address, strings.Join(acts, "; "))
	}
	logf(opts, "no remote changes were made")
	return nil
}

// Apply installs or converges the cluster.
func Apply(ctx context.Context, opts *Options) error {
	c, rb, err := loadAndResolve(opts)
	if err != nil {
		return err
	}
	defer rb.Close()
	st, err := LoadState(StatePath(stateDir(opts), c.Name()), c.Name(), c.Spec.RKE2.Bundle, rb.Meta.Version, rb.Meta.CNI)
	if err != nil {
		return err
	}
	st.FirstServer = c.FirstServer().Name

	logf(opts, "== phase 1: preflight ==")
	rts, err := connectAll(c, opts)
	if err != nil {
		return err
	}
	defer closeAll(rts)
	for _, r := range rts {
		if r.report.Service != "" && r.report.ServiceState == "active" &&
			r.report.InstalledVersion != "" && r.report.InstalledVersion != rb.Meta.Version {
			return fmt.Errorf("%s: installed RKE2 %s but bundle carries %s; run `searke upgrade` for version changes",
				r.node.Name, r.report.InstalledVersion, rb.Meta.Version)
		}
		if err := st.Mark(r.node.Name, r.role, r.report.Arch, StagePreflight, "ok"); err != nil {
			return err
		}
	}

	logf(opts, "== phase 2: distribute payloads ==")
	for _, r := range rts {
		if st.Done(r.node.Name, StageDistributed) {
			logf(opts, "  %s: already distributed, skipping", r.node.Name)
			continue
		}
		if err := distribute(ctx, c, rb, r, opts); err != nil {
			return err
		}
		if err := st.Mark(r.node.Name, r.role, r.report.Arch, StageDistributed, "payload uploaded"); err != nil {
			return err
		}
	}

	logf(opts, "== phase 3: bootstrap first server ==")
	first := findRuntime(rts, c.FirstServer().Name)
	if first == nil {
		return fmt.Errorf("first server %s not reachable", c.FirstServer().Name)
	}
	if err := convergeNode(ctx, c, st, first, "", opts); err != nil {
		return err
	}
	token, err := fetchToken(first)
	if err != nil {
		return fmt.Errorf("read node token from %s: %w", first.node.Name, err)
	}

	logf(opts, "== phase 4: remaining servers ==")
	for _, r := range rts {
		if r.node.Name == first.node.Name || r.role != "server" {
			continue
		}
		if err := convergeNode(ctx, c, st, r, token, opts); err != nil {
			return err
		}
	}

	logf(opts, "== phase 5: agents ==")
	for _, r := range rts {
		if r.role != "agent" {
			continue
		}
		if err := convergeNode(ctx, c, st, r, token, opts); err != nil {
			return err
		}
	}

	logf(opts, "== phase 6: readiness ==")
	if err := waitReady(ctx, first, nodeNames(rts), opts); err != nil {
		return err
	}
	for _, r := range rts {
		if err := st.Mark(r.node.Name, r.role, r.report.Arch, StageReady, "node Ready"); err != nil {
			return err
		}
	}
	if err := st.Log("apply", "cluster converged: "+fmt.Sprint(len(rts))+" nodes"); err != nil {
		return err
	}
	logf(opts, "apply complete. kubeconfig: ssh %s@%s 'sudo cat %s'",
		c.Spec.SSH.User, first.node.Address, kubeconfigPath)
	return nil
}

func findRuntime(rts []*nodeRuntime, name string) *nodeRuntime {
	for _, r := range rts {
		if r.node.Name == name {
			return r
		}
	}
	return nil
}

func nodeNames(rts []*nodeRuntime) map[string]string {
	m := map[string]string{}
	for _, r := range rts {
		m[r.report.Hostname] = r.node.Name
	}
	return m
}

// distribute extracts the platform payload locally and uploads it.
func distribute(ctx context.Context, c *cluster.Cluster, rb *ResolvedBundle, r *nodeRuntime, opts *Options) error {
	p, err := r.report.Platform()
	if err != nil {
		return err
	}
	logf(opts, "  %s: extract %s payload from bundle", r.node.Name, p)

	ii, cleanup, err := rb.Source.OpenIndex()
	if err != nil {
		return err
	}
	defer cleanup()

	tmp, err := os.MkdirTemp("", "searke-payload-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := ociimg.ExtractPayload(ii, p, tmp); err != nil {
		return fmt.Errorf("%s: %w", r.node.Name, err)
	}
	// Verify payload before shipping it to the node.
	cni := rb.Meta.CNI
	if cni == "" {
		cni = "canal"
	}
	payloadDir := filepath.Join(tmp, "opt", "searke", "payload")
	if err := bundle.VerifyPlatform(payloadDir, cni, p); err != nil {
		return fmt.Errorf("%s: extracted payload failed verification: %w", r.node.Name, err)
	}

	if err := r.client.PrepareStaging(); err != nil {
		return fmt.Errorf("%s: %w", r.node.Name, err)
	}
	for _, pair := range [][2]string{
		{filepath.Join(tmp, "opt", "searke", "install.sh"), RemoteRoot + "/install.sh"},
	} {
		logf(opts, "  %s: upload %s", r.node.Name, filepath.Base(pair[0]))
		if err := r.client.UploadFile(pair[0], pair[1], 0o755); err != nil {
			return fmt.Errorf("%s: upload: %w", r.node.Name, err)
		}
	}
	files, err := bundle.Summarize(payloadDir)
	if err != nil {
		return err
	}
	for _, name := range files {
		logf(opts, "  %s: upload %s", r.node.Name, name)
		if err := r.client.UploadFile(filepath.Join(payloadDir, name), RemoteArtifacts+"/"+name, 0o644); err != nil {
			return fmt.Errorf("%s: upload %s: %w", r.node.Name, name, err)
		}
	}
	return nil
}

// renderConfig builds /etc/rancher/rke2/config.yaml content.
func renderConfig(c *cluster.Cluster, n cluster.Node, role, regURL, token string, isFirst bool) ([]byte, error) {
	m := map[string]any{}
	for k, v := range c.Spec.RKE2Config {
		m[k] = v
	}
	cni := c.Spec.RKE2.CNI
	if cni == "" {
		cni = "canal"
	}
	m["cni"] = cni
	m["node-name"] = n.Name
	if isFirst {
		m["tls-san"] = []string{c.RegistrationHost()}
		m["write-kubeconfig-mode"] = "0600"
	} else {
		m["server"] = regURL
		m["token"] = token
	}
	if len(n.Labels) > 0 {
		var labels []string
		for k, v := range n.Labels {
			labels = append(labels, k+"="+v)
		}
		sort.Strings(labels)
		m["node-labels"] = labels
	}
	return yaml.Marshal(m)
}

// convergeNode brings one node to running state, resuming after its
// last recorded stage. token is empty for the first server.
func convergeNode(ctx context.Context, c *cluster.Cluster, st *State, r *nodeRuntime, token string, opts *Options) error {
	name := r.node.Name
	isFirst := name == c.FirstServer().Name

	// Existing foreign installs were already rejected in preflight.
	if r.report.Service != "" && r.report.ServiceState == "active" {
		logf(opts, "  %s: %s already active, verifying only", name, r.report.Service)
		return st.Mark(name, r.role, r.report.Arch, StageRunning, "pre-existing active service")
	}

	if !st.Done(name, StageConfigured) {
		cfg, err := renderConfig(c, r.node, r.role, c.RegistrationURL(), token, isFirst)
		if err != nil {
			return err
		}
		if _, err := r.client.RunSudo("mkdir -p /etc/rancher/rke2"); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if err := r.client.WriteFile(RKE2ConfigPath, cfg, 0o600); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if err := st.Mark(name, r.role, r.report.Arch, StageConfigured, "config written"); err != nil {
			return err
		}
	} else if !isFirst && token != "" {
		// Config may have been written before the token existed (a
		// previous interrupted run never reaches this state because
		// StageConfigured is only marked after token is known) — but
		// re-render to stay safe on token rotation.
		cfg, err := renderConfig(c, r.node, r.role, c.RegistrationURL(), token, isFirst)
		if err != nil {
			return err
		}
		if err := r.client.WriteFile(RKE2ConfigPath, cfg, 0o600); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}

	if !st.Done(name, StageInstalled) {
		logf(opts, "  %s: run install.sh (%s)", name, r.role)
		installCmd := fmt.Sprintf(
			"INSTALL_RKE2_ARTIFACT_PATH=%s INSTALL_RKE2_TYPE=%s INSTALL_RKE2_SKIP_START=true INSTALL_RKE2_VERSION=%s sh %s",
			shellQuote(RemoteArtifacts), r.role, shellQuote(st.Version), RemoteRoot+"/install.sh")
		res, err := r.client.RunSudo(installCmd)
		if err != nil {
			return fmt.Errorf("%s: install.sh: %w", name, err)
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("%s: install.sh failed (exit %d): %s", name, res.ExitCode, tail(res.Stderr+res.Stdout, 800))
		}
		if err := st.Mark(name, r.role, r.report.Arch, StageInstalled, "install.sh ok"); err != nil {
			return err
		}
	}

	if !st.Done(name, StageRunning) {
		svc := "rke2-" + r.role
		logf(opts, "  %s: enable+start %s", name, svc)
		res, err := r.client.RunSudo("systemctl enable "+svc+" && systemctl start "+svc)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("%s: start %s: %s", name, svc, res.Stderr)
		}
		if isFirst {
			if err := waitFor(ctx, 180*time.Second, func() (bool, error) {
				r, _ := r.client.RunSudo("curl -sk https://127.0.0.1:9345/ping")
				return r != nil && strings.Contains(r.Stdout, "pong"), nil
			}); err != nil {
				return fmt.Errorf("%s: registration endpoint never came up: %w", name, err)
			}
		}
		if err := st.Mark(name, r.role, r.report.Arch, StageRunning, "service active"); err != nil {
			return err
		}
	}
	return nil
}

// fetchToken reads the node token from the bootstrap server. It is kept
// in memory only and never written to logs or the state file.
func fetchToken(first *nodeRuntime) (string, error) {
	res, err := first.client.RunSudo("cat " + nodeTokenPath)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("exit %d: %s", res.ExitCode, res.Stderr)
	}
	tok := strings.TrimSpace(res.Stdout)
	if tok == "" {
		return "", fmt.Errorf("empty node token")
	}
	return tok, nil
}

// waitReady polls kubectl on the bootstrap server until every node is Ready.
func waitReady(ctx context.Context, first *nodeRuntime, names map[string]string, opts *Options) error {
	kctl := fmt.Sprintf("%s --kubeconfig %s", kubectlPath, kubeconfigPath)
	return waitFor(ctx, 5*time.Minute, func() (bool, error) {
		res, err := first.client.RunSudo(kctl + " get nodes --no-headers")
		if err != nil || res.ExitCode != 0 {
			return false, nil
		}
		ready := map[string]bool{}
		for _, line := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
			f := strings.Fields(line)
			if len(f) >= 2 {
				ready[f[0]] = f[1] == "Ready"
			}
		}
		for _, nodeName := range names {
			ok := ready[nodeName]
			if !ok {
				// fall back to hostname match
				for h, nn := range names {
					if nn == nodeName {
						ok = ready[h]
					}
				}
			}
			if !ok {
				return false, nil
			}
		}
		return true, nil
	})
}

// waitFor polls cond until it returns true or the deadline passes.
func waitFor(ctx context.Context, timeout time.Duration, cond func() (bool, error)) error {
	deadline := time.Now().Add(timeout)
	for {
		ok, err := cond()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
