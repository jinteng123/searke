package deploy

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
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
	rke2BinDir      = "/var/lib/rancher/rke2/bin"
	// rke2Exe invokes the rke2 binary wherever install.sh put it:
	// sudo's secure_path on RHEL-family omits /usr/local/bin, and
	// install.sh falls back to /opt/rke2/bin on read-only /usr/local.
	rke2Exe = "PATH=/usr/local/bin:/opt/rke2/bin:$PATH rke2"
)

// Options controls Validate/Plan/Apply runs.
type Options struct {
	ClusterFile string
	Cluster     *cluster.Cluster // when set, used instead of loading ClusterFile
	StateDir    string           // defaults to the cluster file's directory
	Log         func(format string, args ...any)
	// Only, when non-nil, restricts connect/preflight/converge to the
	// named nodes. The bootstrap (first) server is always included:
	// the join token and readiness checks flow through it.
	Only map[string]bool
	// NodePassword authenticates nodes in Only only — lets `searke
	// add` reach brand-new nodes without imposing one password on
	// existing cluster members. Never persisted.
	NodePassword string
	// StrictHostKey disables trust-on-first-use host key acceptance:
	// unknown hosts are refused and must be in known_hosts already.
	StrictHostKey bool
}

// ResolvedBundle pairs the opened index with its metadata. The index
// stays open for the whole run so each node doesn't re-unpack a local
// .oci.tar.
type ResolvedBundle struct {
	Source ociimg.Source
	II     v1.ImageIndex
	Meta   *ociimg.Meta
	Close  func()
}

// nodeRuntime collects per-node run state.
type nodeRuntime struct {
	node   cluster.Node
	role   string
	client Executor
	report *PreflightReport
}

func logf(o *Options, f string, a ...any) {
	if o.Log != nil {
		o.Log(f, a...)
	}
}

func loadAndResolve(opts *Options) (*cluster.Cluster, *ResolvedBundle, error) {
	c := opts.Cluster
	if c == nil {
		var err error
		c, err = cluster.Load(opts.ClusterFile)
		if err != nil {
			return nil, nil, err
		}
	}
	src, err := ociimg.ParseSource(c.Spec.RKE2.Bundle, c.Spec.RKE2.Insecure)
	if err != nil {
		return nil, nil, err
	}
	logf(opts, "resolving RKE2 bundle %s (a local OCI archive may take a while to unpack)", c.Spec.RKE2.Bundle)
	ii, cleanup, err := src.OpenIndexProgress(func(read, total int64) {
		if total <= 0 {
			return
		}
		logf(opts, "  bundle archive: %s/%s (%.1f%%)", formatBytes(read), formatBytes(total), float64(read)*100/float64(total))
	})
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
	return c, &ResolvedBundle{Source: src, II: ii, Meta: meta, Close: cleanup}, nil
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

// connectAll reaches every node (SSH or local exec) and runs preflight.
// When opts.Only is set, nodes outside the scope are skipped entirely —
// except the bootstrap server, which is needed for the join token.
func connectAll(c *cluster.Cluster, opts *Options) ([]*nodeRuntime, error) {
	baseCfg := sshConfig(c)
	first := c.FirstServer().Name
	hostnames := map[string]string{}
	var runtimes []*nodeRuntime
	for _, n := range c.AllNodes() {
		if opts.Only != nil && !opts.Only[n.Name] && n.Name != first {
			continue
		}
		cfg := baseCfg
		cfg.StrictHostKey = opts.StrictHostKey
		if opts.Only != nil && opts.Only[n.Name] && opts.NodePassword != "" {
			cfg.Password = opts.NodePassword
		}
		role := c.RoleOf(n.Name)
		cl, err := Connect(n.Address, cfg)
		if err != nil && n.Name == first && cfg.Password == "" && opts.NodePassword != "" && isAuthErr(err) {
			// The bootstrap may itself have been installed via a
			// password that was never persisted; the run-scoped
			// password is the operator's declared credential for
			// this cluster, so retry with it before giving up.
			cfg.Password = opts.NodePassword
			cl, err = Connect(n.Address, cfg)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", n.Name, err)
		}
		// A node reached by password gets the cluster key authorized
		// so future runs (apply/exec/uninstall) authenticate by key.
		maybeProvisionKey(cl, n.Name, cfg, opts)
		mode := "ssh"
		if _, ok := cl.(*LocalExecutor); ok {
			mode = "local exec"
		}
		logf(opts, "  connect %s (%s, %s)", n.Name, n.Address, mode)
		logf(opts, "  [%s] checking OS, architecture, disk, network, and existing RKE2 state", n.Name)
		rep, err := preflightWithHeartbeat(cl, n.Name, opts)
		if err != nil {
			cl.Close()
			return nil, err
		}
		for _, w := range rep.Warnings {
			logf(opts, "  warning %s: %s", n.Name, w)
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

func preflightWithHeartbeat(ex Executor, node string, opts *Options) (*PreflightReport, error) {
	type result struct {
		report *PreflightReport
		err    error
	}
	done := make(chan result, 1)
	go func() {
		rep, err := Preflight(ex, node)
		done <- result{report: rep, err: err}
	}()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	started := time.Now()
	for {
		select {
		case res := <-done:
			return res.report, res.err
		case <-ticker.C:
			logf(opts, "  [%s] preflight still running (elapsed %s)", node, time.Since(started).Round(time.Second))
		}
	}
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
		mode := "ssh"
		if _, ok := r.client.(*LocalExecutor); ok {
			mode = "local"
		}
		logf(opts, "  %s (%s, %s, %s): %s", r.node.Name, r.role, r.node.Address, mode, strings.Join(acts, "; "))
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
	if prev := st.FirstServer; prev != "" && prev != c.FirstServer().Name && hasServer(c, prev) {
		return fmt.Errorf("bootstrap server changed from %s to %s (it is the name-sorted first server); "+
			"rename %s so it sorts after %s, otherwise it would initialise a separate cluster",
			prev, c.FirstServer().Name, c.FirstServer().Name, prev)
	}
	st.FirstServer = c.FirstServer().Name

	logf(opts, "== phase 1: preflight ==")
	rts, err := connectAll(c, opts)
	if err != nil {
		return err
	}
	defer closeAll(rts)
	if err := checkCgroupV1(c, rts, rb.Meta.Version, opts); err != nil {
		return err
	}
	for _, r := range rts {
		if r.report.Service != "" && r.report.ServiceState == "active" &&
			r.report.InstalledVersion != "" && r.report.InstalledVersion != rb.Meta.Version {
			return fmt.Errorf("%s: installed RKE2 %s but bundle carries %s; run `searke upgrade` for version changes",
				r.node.Name, r.report.InstalledVersion, rb.Meta.Version)
		}
		if ns, known := st.Node(r.node.Name); known && st.Done(r.node.Name, StageRunning) && r.report.Service == "" {
			hint := "remove the node from cluster.yaml and state, or delete the state file to reinstall"
			if _, local := r.client.(*LocalExecutor); local {
				hint = "the descriptor uses a local address that resolves to this machine; run on the node itself or use its real address"
			}
			return fmt.Errorf("%s: state records stage %q but no RKE2 install found here; refusing to reinstall: %s",
				r.node.Name, ns.Stage, hint)
		}
		if err := st.Mark(r.node.Name, r.role, r.report.Arch, StagePreflight, "ok"); err != nil {
			return err
		}
	}

	logf(opts, "== phase 2: distribute payloads ==")
	scratch, err := os.MkdirTemp("", "searke-payload-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	payloadDirs := map[bundle.Platform]string{}
	for i, r := range rts {
		if r.report.Service != "" && r.report.ServiceState == "active" {
			logf(opts, "  [%d/%d] %s: active service found, skip payload", i+1, len(rts), r.node.Name)
			continue // healthy node needs no new payload
		}
		if st.Done(r.node.Name, StageDistributed) && remotePayloadOK(r) {
			logf(opts, "  [%d/%d] %s: payload already present, skipping", i+1, len(rts), r.node.Name)
			continue
		}
		logf(opts, "  [%d/%d] %s (%s): distributing payload", i+1, len(rts), r.node.Name, r.report.Arch)
		if err := distribute(ctx, c, rb, r, scratch, payloadDirs, opts); err != nil {
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
	stopJournals := make([]func(), 0, len(rts))
	for _, r := range rts {
		stopJournals = append(stopJournals, followJournal(ctx, r, "rke2-"+r.role, opts))
	}
	stopAllJournals := func() {
		for i := len(stopJournals) - 1; i >= 0; i-- {
			stopJournals[i]()
		}
	}
	if err := waitReady(ctx, first, nodeNames(rts), opts); err != nil {
		stopAllJournals()
		return err
	}
	stopAllJournals()
	for _, r := range rts {
		if err := st.Mark(r.node.Name, r.role, r.report.Arch, StageReady, "node Ready"); err != nil {
			return err
		}
	}
	// Convenience, not correctness: put kubectl/crictl/ctr on PATH and a
	// kubeconfig in the admin's ~/.kube so the node is usable on login.
	for _, r := range rts {
		if err := setupNodeCLI(ctx, c, r, opts); err != nil {
			logf(opts, "  warning %s: CLI setup failed (cluster is still healthy): %v", r.node.Name, err)
		}
	}
	if err := st.Log("apply", "cluster converged: "+fmt.Sprint(len(c.AllNodes()))+" nodes"); err != nil {
		return err
	}
	if cluster.IsLocalAddress(first.node.Address) {
		logf(opts, "apply complete. kubeconfig: sudo cat %s", kubeconfigPath)
	} else {
		logf(opts, "apply complete. kubeconfig: ssh %s@%s 'sudo cat %s'",
			c.Spec.SSH.User, first.node.Address, kubeconfigPath)
	}
	return nil
}

// setupNodeCLI makes the freshly installed node convenient to operate:
// rke2/kubectl/crictl/ctr are symlinked into /usr/local/bin and, on
// servers, the admin kubeconfig is installed into ~/.kube/config of root
// and the invoking user ($SUDO_USER — set by sudo on both the local and
// SSH paths). An existing ~/.kube/config pointing at a different API
// server is never overwritten; the file lands as config-<cluster>
// instead. A config pointing at the same server (i.e. a stale copy
// previously installed by searke, e.g. after a reinstall) is replaced.
func setupNodeCLI(ctx context.Context, c *cluster.Cluster, r *nodeRuntime, opts *Options) error {
	cmd := `for b in /var/lib/rancher/rke2/bin /opt/rke2/bin; do
  if test -d "$b"; then
    for t in rke2 kubectl crictl ctr; do
      test -x "$b/$t" && ln -sf "$b/$t" /usr/local/bin/$t
    done
  fi
done
if test ! -f ` + kubeconfigPath + `; then exit 0; fi
new_srv=$(grep -m1 'server:' ` + kubeconfigPath + ` | awk '{print $2}')
for u in root $SUDO_USER; do
  test -n "$u" || continue
  d=$(eval "echo ~$u" 2>/dev/null) || continue
  test -d "$d" || continue
  mkdir -p "$d/.kube" || continue
  chown "$(id -u "$u")":"$(id -g "$u")" "$d/.kube" 2>/dev/null
  if test ! -f "$d/.kube/config"; then
    install -m 600 -o "$(id -u "$u")" -g "$(id -g "$u")" ` + kubeconfigPath + ` "$d/.kube/config" && echo "kubeconfig -> $d/.kube/config"
  elif cmp -s ` + kubeconfigPath + ` "$d/.kube/config"; then
    :
  elif test "$new_srv" = "$(grep -m1 'server:' "$d/.kube/config" | awk '{print $2}')"; then
    install -m 600 -o "$(id -u "$u")" -g "$(id -g "$u")" ` + kubeconfigPath + ` "$d/.kube/config" && echo "replaced stale kubeconfig -> $d/.kube/config"
  else
    install -m 600 -o "$(id -u "$u")" -g "$(id -g "$u")" ` + kubeconfigPath + ` "$d/.kube/config-` + c.Name() + `" && echo "existing ~/.kube/config kept; wrote $d/.kube/config-` + c.Name() + `"
  fi
done`
	res, err := runSudoStreaming(ctx, r.client, cmd, opts, r.node.Name)
	if err != nil {
		return err
	}
	if res == nil || res.ExitCode != 0 {
		out := ""
		if res != nil {
			out = res.Stderr + res.Stdout
		}
		return fmt.Errorf("exit %s", tail(out, 400))
	}
	return nil
}

func hasServer(c *cluster.Cluster, name string) bool {
	for _, n := range c.Spec.Servers {
		if n.Name == name {
			return true
		}
	}
	return false
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

// remotePayloadOK checks whether the artifacts actually still exist on
// the node — local state alone is not trusted.
func remotePayloadOK(r *nodeRuntime) bool {
	p, err := r.report.Platform()
	if err != nil {
		return false
	}
	check := fmt.Sprintf("test -f %s/install.sh && test -f %s/rke2.linux-%s.tar.gz",
		RemoteRoot, RemoteArtifacts, p.Arch)
	res, err := r.client.RunSudo(check)
	return err == nil && res.ExitCode == 0
}

// distribute extracts the platform payload locally (once per platform,
// cached under scratch) and uploads it.
func distribute(ctx context.Context, c *cluster.Cluster, rb *ResolvedBundle, r *nodeRuntime,
	scratch string, cache map[bundle.Platform]string, opts *Options) error {
	p, err := r.report.Platform()
	if err != nil {
		return err
	}

	payloadDir, ok := cache[p]
	if !ok {
		tmp := filepath.Join(scratch, p.DirName())
		logf(opts, "  [%s] extracting %s OCI payload", r.node.Name, p)
		if err := ociimg.ExtractPayloadProgress(rb.II, p, tmp, func(layer int, bytesRead int64) {
			logf(opts, "  [%s] extracting %s layer %d: %s streamed", r.node.Name, p, layer, formatBytes(bytesRead))
		}); err != nil {
			return fmt.Errorf("%s: %w", r.node.Name, err)
		}
		payloadDir = filepath.Join(tmp, "opt", "searke")
		// Verify payload once before shipping it to any node.
		cni := rb.Meta.CNI
		if cni == "" {
			cni = "canal"
		}
		if err := bundle.VerifyPlatform(payloadDir+"/payload", cni, p); err != nil {
			return fmt.Errorf("extracted %s payload failed verification: %w", p, err)
		}
		cache[p] = payloadDir
	}

	if err := r.client.PrepareStaging(); err != nil {
		return fmt.Errorf("%s: %w", r.node.Name, err)
	}
	if err := uploadPayloadFile(r, payloadDir+"/install.sh", RemoteRoot+"/install.sh", 0o755, opts); err != nil {
		return fmt.Errorf("%s: upload: %w", r.node.Name, err)
	}
	files, err := bundle.Summarize(payloadDir + "/payload")
	if err != nil {
		return err
	}
	for _, name := range files {
		if err := uploadPayloadFile(r, filepath.Join(payloadDir, "payload", name), RemoteArtifacts+"/"+name, 0o644, opts); err != nil {
			return fmt.Errorf("%s: upload %s: %w", r.node.Name, name, err)
		}
	}
	return nil
}

func uploadPayloadFile(r *nodeRuntime, local, remote string, mode os.FileMode, opts *Options) error {
	info, err := os.Stat(local)
	if err != nil {
		return err
	}
	total := info.Size()
	started := time.Now()
	logf(opts, "  [%s] uploading %s (%s)", r.node.Name, filepath.Base(local), formatBytes(total))
	var transferred atomic.Int64
	stopHeartbeat := make(chan struct{})
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		last := int64(-1)
		for {
			select {
			case <-stopHeartbeat:
				return
			case <-ticker.C:
				now := transferred.Load()
				if now == last && now < total {
					logf(opts, "  [%s] still uploading %s (%s/%s, elapsed %s)", r.node.Name, filepath.Base(local), formatBytes(now), formatBytes(total), time.Since(started).Round(time.Second))
				}
				last = now
			}
		}
	}()
	var progMu sync.Mutex
	lastLog := time.Time{}
	progress := func(written, size int64) {
		transferred.Store(written)
		if size == 0 {
			return
		}
		// Concurrent SFTP writes can invoke this from several goroutines.
		progMu.Lock()
		defer progMu.Unlock()
		// Log at most every 5s mid-flight so a slow link doesn't bury the
		// run log; the final "uploaded" line below reports completion.
		if written != size && time.Since(lastLog) < 5*time.Second {
			return
		}
		lastLog = time.Now()
		pct := float64(written) * 100 / float64(size)
		elapsed := time.Since(started).Seconds()
		if elapsed < 0.5 {
			logf(opts, "  [%s] %s: %s/%s (%.1f%%)", r.node.Name, filepath.Base(local), formatBytes(written), formatBytes(size), pct)
			return
		}
		rate := float64(written) / elapsed / (1024 * 1024)
		logf(opts, "  [%s] %s: %s/%s (%.1f%%, %.1f MiB/s)", r.node.Name, filepath.Base(local), formatBytes(written), formatBytes(size), pct, rate)
	}
	err = uploadWithProgress(r.client, local, remote, mode, progress)
	close(stopHeartbeat)
	<-heartbeatDone
	if err != nil {
		return err
	}
	logf(opts, "  [%s] uploaded %s in %s", r.node.Name, filepath.Base(local), time.Since(started).Round(time.Second))
	return nil
}

func formatBytes(n int64) string {
	if n < 1024*1024 {
		return fmt.Sprintf("%.0f KiB", float64(n)/1024)
	}
	return fmt.Sprintf("%.1f MiB", float64(n)/(1024*1024))
}

// renderConfig builds /etc/rancher/rke2/config.yaml content.
// cgroupV1 adds kubelet-arg fail-cgroupv1=false so kubelet >= 1.35 still
// starts on a cgroup v1 host; an explicit user value is never overridden.
func renderConfig(c *cluster.Cluster, n cluster.Node, role, regURL, token string, isFirst, cgroupV1 bool) ([]byte, error) {
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
	// tls-san applies to every server, not just the bootstrap: the
	// registration/API endpoint may be a VIP or LB fronting all of
	// them, so each cert must carry it.
	if role == "server" {
		sans := map[string]bool{c.RegistrationHost(): true}
		if h := apiHost(c); h != "" {
			sans[h] = true
		}
		var sanList []string
		if v, ok := m["tls-san"]; ok {
			for _, s := range toStringSlice(v) {
				sanList = append(sanList, s)
				delete(sans, s)
			}
		}
		for s := range sans {
			sanList = append(sanList, s)
		}
		sort.Strings(sanList)
		m["tls-san"] = sanList
	}
	if isFirst {
		if _, ok := m["write-kubeconfig-mode"]; !ok {
			m["write-kubeconfig-mode"] = "0600"
		}
	} else {
		m["server"] = regURL
		m["token"] = token
	}
	// node-labels: merge labels from the node entry and rke2Config.
	var labels []string
	if v, ok := m["node-labels"]; ok {
		labels = append(labels, toStringSlice(v)...)
	}
	for k, v := range n.Labels {
		labels = append(labels, k+"="+v)
	}
	if len(labels) > 0 {
		sort.Strings(labels)
		m["node-labels"] = labels
	}
	if _, pinned := userFailCgroupV1(c); cgroupV1 && !pinned {
		m["kubelet-arg"] = append(toStringSlice(m["kubelet-arg"]), failCgroupV1Arg+"=false")
	}
	return yaml.Marshal(m)
}

// apiHost returns the host part of spec.endpoints.apiAddress.
func apiHost(c *cluster.Cluster) string {
	if c.Spec.Endpoints.APIAddress == "" {
		return ""
	}
	host, _, err := net.SplitHostPort(c.Spec.Endpoints.APIAddress)
	if err != nil {
		return ""
	}
	return strings.Trim(host, "[]")
}

func toStringSlice(v any) []string {
	var out []string
	switch t := v.(type) {
	case string:
		out = []string{t}
	case []string:
		out = t
	case []any:
		for _, x := range t {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// convergeNode brings one node to running state, resuming after its
// last recorded stage. token is empty for the first server.
func convergeNode(ctx context.Context, c *cluster.Cluster, st *State, r *nodeRuntime, token string, opts *Options) error {
	name := r.node.Name
	isFirst := name == c.FirstServer().Name

	// Existing foreign installs were already rejected in preflight.
	if r.report.Service != "" && r.report.ServiceState == "active" {
		// An active node must still carry the current spec: re-render
		// config.yaml and, when it drifted, write it back and restart so a
		// re-run apply actually converges instead of skipping the node.
		cfg, err := renderConfig(c, r.node, r.role, c.RegistrationURL(), token, isFirst,
			!r.report.CgroupV2 && cgroupV1NeedsOverride(st.Version))
		if err != nil {
			return err
		}
		res, err := r.client.RunSudo("cat " + shellQuote(RKE2ConfigPath))
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if res.ExitCode != 0 || res.Stdout != string(cfg) {
			if !isFirst && token == "" {
				return fmt.Errorf("%s: config drifted but no node token available to re-render it", name)
			}
			logf(opts, "  %s: config drifted, rewriting %s and restarting %s", name, RKE2ConfigPath, r.report.Service)
			if err := r.client.WriteFile(RKE2ConfigPath, cfg, 0o600); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			svc := "rke2-" + r.role
			res, err := runSudoStreaming(ctx, r.client, "systemctl restart "+shellQuote(svc), opts, name)
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
			if r.role == "server" {
				if err := waitServerReadyContext(ctx, r.client, 10*time.Minute, func(msg string) {
					logf(opts, "  [%s] %s", name, msg)
				}); err != nil {
					return fmt.Errorf("%s: rke2-server did not come back after config rewrite: %w", name, err)
				}
			}
		}
		logf(opts, "  %s: %s already active, verifying only", name, r.report.Service)
		return st.Mark(name, r.role, r.report.Arch, StageRunning, "pre-existing active service")
	}

	if !st.Done(name, StageConfigured) {
		cfg, err := renderConfig(c, r.node, r.role, c.RegistrationURL(), token, isFirst,
			!r.report.CgroupV2 && cgroupV1NeedsOverride(st.Version))
		if err != nil {
			return err
		}
		if res, err := r.client.RunSudo("mkdir -p /etc/rancher/rke2"); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		} else if res.ExitCode != 0 {
			return fmt.Errorf("%s: mkdir /etc/rancher/rke2: %s", name, res.Stderr)
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
		cfg, err := renderConfig(c, r.node, r.role, c.RegistrationURL(), token, isFirst,
			!r.report.CgroupV2 && cgroupV1NeedsOverride(st.Version))
		if err != nil {
			return err
		}
		if err := r.client.WriteFile(RKE2ConfigPath, cfg, 0o600); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}

	if !st.Done(name, StageInstalled) {
		logf(opts, "  [%s] running the RKE2 installer (%s); output streams live", name, r.role)
		// The upstream installer copies artifacts and enables nothing;
		// service start is controlled by searke below.
		installCmd := fmt.Sprintf(
			"INSTALL_RKE2_ARTIFACT_PATH=%s INSTALL_RKE2_TYPE=%s INSTALL_RKE2_VERSION=%s sh %s",
			shellQuote(RemoteArtifacts), r.role, shellQuote(st.Version), RemoteRoot+"/install.sh")
		res, err := runSudoStreamingWithHeartbeat(ctx, r.client, installCmd, opts, name, "running install.sh", 10*time.Second)
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
		logf(opts, "  [%s] enabling %s", name, svc)
		res, err := r.client.RunSudo("systemctl enable " + svc)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("%s: enable %s: %s", name, svc, res.Stderr)
		}
		if err := startService(ctx, r, svc, opts); err != nil {
			return err
		}
		if isFirst {
			// Joiners need a serving bootstrap node. Probe via the
			// bundled kubectl rather than curl, which minimal images
			// lack.
			stopJournal := followJournal(ctx, r, svc, opts)
			if err := waitServerReadyContext(ctx, r.client, 10*time.Minute, func(msg string) {
				logf(opts, "  [%s] %s", name, msg)
			}); err != nil {
				stopJournal()
				return fmt.Errorf("%s: bootstrap server never became ready: %w", name, err)
			}
			stopJournal()
		}
		if err := st.Mark(name, r.role, r.report.Arch, StageRunning, "service active"); err != nil {
			return err
		}
	}
	return nil
}

func startService(ctx context.Context, r *nodeRuntime, service string, opts *Options) error {
	name := r.node.Name
	logf(opts, "  [%s] starting %s; streaming systemd journal", name, service)
	stopJournal := followJournal(ctx, r, service, opts)
	type commandResult struct {
		result *Result
		err    error
	}
	done := make(chan commandResult, 1)
	go func() {
		res, err := runSudoStreaming(ctx, r.client, "systemctl start "+shellQuote(service), opts, name)
		done <- commandResult{result: res, err: err}
	}()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	started := time.Now()
	for {
		select {
		case result := <-done:
			stopJournal()
			if result.err != nil {
				return fmt.Errorf("%s: start %s: %w", name, service, result.err)
			}
			if result.result == nil || result.result.ExitCode != 0 {
				if result.result == nil {
					return fmt.Errorf("%s: start %s returned no result", name, service)
				}
				return fmt.Errorf("%s: start %s failed (exit %d): %s", name, service,
					result.result.ExitCode, tail(result.result.Stderr+result.result.Stdout, 800))
			}
			return nil
		case <-ctx.Done():
			stopJournal()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
			}
			return ctx.Err()
		case <-ticker.C:
			logf(opts, "  [%s] still waiting for %s to start (elapsed %s)", name, service, time.Since(started).Round(time.Second))
		}
	}
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
	const timeout = 10 * time.Minute
	started, deadline := time.Now(), time.Now().Add(timeout)
	kctl := fmt.Sprintf("%s --kubeconfig %s --request-timeout=10s", kubectlPath, kubeconfigPath)
	lastReport := time.Time{}
	lastReady := -1
	for {
		res, err := first.client.RunSudo(kctl + " get nodes --no-headers")
		ready := map[string]bool{}
		if err == nil && res.ExitCode == 0 {
			for _, line := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
				f := strings.Fields(line)
				if len(f) >= 2 {
					// "Ready" or "Ready,SchedulingDisabled" (cordoned
					// nodes stay schedulable=false until uncordoned).
					ready[f[0]] = f[1] == "Ready" || strings.HasPrefix(f[1], "Ready,")
				}
			}
		}
		count := 0
		for _, nodeName := range names {
			if ready[nodeName] {
				count++
				continue
			}
			// Fall back to the host name reported by Kubernetes.
			for hostname, configuredName := range names {
				if configuredName == nodeName && ready[hostname] {
					count++
					break
				}
			}
		}
		if count == len(names) {
			return nil
		}
		if count != lastReady || time.Since(lastReport) >= 5*time.Second {
			logf(opts, "  waiting for nodes Ready: %d/%d (elapsed %s)", count, len(names), time.Since(started).Round(time.Second))
			lastReady, lastReport = count, time.Now()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s; %d/%d nodes Ready", timeout, count, len(names))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// followJournal streams a service journal while a readiness probe is waiting.
// The returned function stops and joins the SSH/local stream.
func followJournal(ctx context.Context, r *nodeRuntime, service string, opts *Options) func() {
	followCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	cmd := "journalctl -fu " + shellQuote(service) + " -n 20 --no-pager"
	go func() {
		defer close(done)
		if _, err := runSudoStreaming(followCtx, r.client, cmd, opts, r.node.Name); err != nil && followCtx.Err() == nil {
			logf(opts, "  [%s] journal stream ended: %v", r.node.Name, err)
		}
	}()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			logf(opts, "  [%s] journal stream is still closing", r.node.Name)
		}
	}
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
