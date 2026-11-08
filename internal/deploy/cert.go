package deploy

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jint/searke/internal/cluster"
	"gopkg.in/yaml.v3"
)

// cert.go: day-2 certificate operations on RKE2 servers. Everything is
// inspected locally after pulling the PEM over SSH — no openssl, no
// python, no etcdctl needed on the nodes.

const servingCertPath = "/var/lib/rancher/rke2/server/tls/serving-kube-apiserver.crt"
const remoteConfigPath = "/etc/rancher/rke2/config.yaml"

// selectServers intersects the selector with spec.servers — cert
// operations only apply to control-plane nodes.
func selectServers(c *cluster.Cluster, names []string) ([]cluster.Node, error) {
	nodes, err := selectNodes(c, names)
	if err != nil {
		return nil, err
	}
	var servers []cluster.Node
	for _, n := range nodes {
		if c.RoleOf(n.Name) != "server" {
			return nil, fmt.Errorf("%s is an agent node; cert operations apply to servers only", n.Name)
		}
		servers = append(servers, n)
	}
	return servers, nil
}

// servingCert fetches the apiserver serving cert from a node.
func servingCert(ex Executor) (*x509.Certificate, error) {
	tmp, err := os.CreateTemp("", "searke-cert-*.pem")
	if err != nil {
		return nil, err
	}
	tmp.Close()
	defer os.Remove(tmp.Name())
	if err := ex.DownloadFile(servingCertPath, tmp.Name()); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(tmp.Name())
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("no PEM block in %s", servingCertPath)
	}
	return x509.ParseCertificate(block.Bytes)
}

// CertCheck reports expiry and SANs of the apiserver serving cert on
// each server. Certs expiring within 30 days are flagged.
func CertCheck(c *cluster.Cluster, names []string, log func(string, ...any)) error {
	servers, err := selectServers(c, names)
	if err != nil {
		return err
	}
	cfg := sshConfig(c)
	var errs []string
	for _, n := range servers {
		ex, err := Connect(n.Address, cfg)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: connect: %v", n.Name, err))
			continue
		}
		cert, err := servingCert(ex)
		ex.Close()
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", n.Name, err))
			continue
		}
		days := int(time.Until(cert.NotAfter).Hours() / 24)
		flag := ""
		if days < 30 {
			flag = "  ** expires soon **"
		}
		log("[%s] apiserver cert: notAfter=%s (%dd)%s", n.Name,
			cert.NotAfter.Format("2006-01-02"), days, flag)
		var sans []string
		sans = append(sans, cert.DNSNames...)
		for _, ip := range cert.IPAddresses {
			sans = append(sans, ip.String())
		}
		sort.Strings(sans)
		log("[%s]   SANs: %s", n.Name, strings.Join(sans, " "))
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// CertSan adds entries to tls-san on each server: merges into the
// remote config.yaml, restarts rke2-server one node at a time, waits
// for it to be active, and verifies the served cert picked up the new
// SANs. Only after every node verifies is spec.rke2Config.tls-san
// persisted into cluster.yaml so future config rewrites keep them.
func CertSan(c *cluster.Cluster, names, sans []string, opts *Options, yes bool) error {
	if len(sans) == 0 {
		return fmt.Errorf("no SANs given")
	}
	servers, err := selectServers(c, names)
	if err != nil {
		return err
	}
	if !yes {
		return fmt.Errorf("cert san restarts rke2-server on %s sequentially; pass --yes to confirm",
			nodeNameList(servers))
	}
	cfg := sshConfig(c)
	for _, n := range servers {
		logf(opts, "[%s] merging tls-san %s", n.Name, strings.Join(sans, ","))
		ex, err := Connect(n.Address, cfg)
		if err != nil {
			return fmt.Errorf("%s: connect: %w — cluster.yaml unchanged", n.Name, err)
		}
		err = mergeRemoteSans(ex, sans)
		if err == nil {
			err = restartServerAndVerify(ex, n, sans, opts)
		}
		ex.Close()
		if err != nil {
			return fmt.Errorf("%s: %w — cluster.yaml unchanged; fix and rerun (already-updated SANs are idempotent)", n.Name, err)
		}
	}
	// Persist into the descriptor so renderConfig keeps the SANs.
	merged := mergeSANLists(toStringSlice(c.Spec.RKE2Config["tls-san"]), sans)
	if err := cluster.SetRKE2ConfigList(opts.ClusterFile, "tls-san", merged); err != nil {
		return fmt.Errorf("nodes updated but cluster.yaml write failed: %w", err)
	}
	logf(opts, "tls-san persisted to cluster.yaml: %s", strings.Join(merged, " "))
	return nil
}

// mergeRemoteSans reads the remote config.yaml, unions tls-san with
// the new values, and writes it back.
func mergeRemoteSans(ex Executor, sans []string) error {
	res, err := ex.RunSudo("cat " + remoteConfigPath)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("read %s: %s", remoteConfigPath, strings.TrimSpace(res.Stderr))
	}
	m := map[string]any{}
	if err := yaml.Unmarshal([]byte(res.Stdout), &m); err != nil {
		return fmt.Errorf("parse %s: %w", remoteConfigPath, err)
	}
	cur := toStringSlice(m["tls-san"])
	m["tls-san"] = mergeSANLists(cur, sans)
	out, err := yaml.Marshal(m)
	if err != nil {
		return err
	}
	return ex.WriteFile(remoteConfigPath, out, 0o600)
}

// waitServerReady blocks until rke2-server is active AND the local
// apiserver answers /readyz — the latter proves etcd connectivity was
// re-established, which is what protects quorum before the next
// server's restart in a rolling operation.
func waitServerReady(ex Executor, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	active := false
	for time.Now().Before(deadline) {
		if !active {
			if r, _ := ex.RunSudo("systemctl is-active rke2-server"); r != nil && strings.TrimSpace(r.Stdout) == "active" {
				active = true
			}
		} else {
			res, err := ex.RunSudo(fmt.Sprintf(
				"%s --kubeconfig %s --request-timeout=10s get --raw=/readyz",
				kubectlPath, kubeconfigPath))
			if err == nil && res.ExitCode == 0 && strings.Contains(res.Stdout, "ok") {
				return nil
			}
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("rke2-server did not come back ready within %s", timeout)
}

// restartServerAndVerify restarts rke2-server, waits until the unit is
// serving again, then confirms the serving cert carries all requested
// SANs.
func restartServerAndVerify(ex Executor, n cluster.Node, sans []string, opts *Options) error {
	if _, err := runAsRoot(ex, "systemctl restart rke2-server"); err != nil {
		return fmt.Errorf("restart rke2-server: %w", err)
	}
	if err := waitServerReady(ex, 10*time.Minute); err != nil {
		return err
	}
	cert, err := servingCert(ex)
	if err != nil {
		return fmt.Errorf("serving cert re-read: %w", err)
	}
	have := map[string]bool{}
	for _, s := range cert.DNSNames {
		have[s] = true
	}
	for _, ip := range cert.IPAddresses {
		have[ip.String()] = true
	}
	var missing []string
	for _, s := range sans {
		if !have[s] {
			missing = append(missing, s)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("serving cert still lacks SANs %s after restart", strings.Join(missing, ","))
	}
	logf(opts, "[%s] rke2-server restarted; serving cert verified", n.Name)
	return nil
}

// CertRotate rotates RKE2 certificates on each server, one at a time,
// following the upstream order: stop the service, rotate, start. The
// serving cert's serial is compared before/after so the log reports
// whether rotation actually happened — expiry alone cannot tell on a
// freshly installed cluster.
func CertRotate(c *cluster.Cluster, names []string, opts *Options, yes bool) error {
	servers, err := selectServers(c, names)
	if err != nil {
		return err
	}
	if !yes {
		return fmt.Errorf("cert rotate restarts rke2-server on %s sequentially; pass --yes to confirm",
			nodeNameList(servers))
	}
	cfg := sshConfig(c)
	for _, n := range servers {
		ex, err := Connect(n.Address, cfg)
		if err != nil {
			return fmt.Errorf("%s: connect: %w", n.Name, err)
		}
		logf(opts, "[%s] rke2 certificate rotate", n.Name)
		before, _ := servingCert(ex)
		_, err = runAsRoot(ex, "systemctl stop rke2-server && "+
			rke2Exe+" certificate rotate && systemctl start rke2-server")
		if err == nil {
			err = waitServerReady(ex, 10*time.Minute)
		}
		if err == nil {
			var cert *x509.Certificate
			cert, err = servingCert(ex)
			if err == nil {
				rotated := before == nil || before.SerialNumber.Cmp(cert.SerialNumber) != 0
				logf(opts, "[%s] rotated=%v; apiserver cert expires %s (serial …%s)",
					n.Name, rotated, cert.NotAfter.Format("2006-01-02"),
					cert.SerialNumber.Text(16)[max(0, len(cert.SerialNumber.Text(16))-8):])
			}
		}
		ex.Close()
		if err != nil {
			return fmt.Errorf("%s: %w — remaining servers untouched", n.Name, err)
		}
	}
	return nil
}

// mergeSANLists unions two SAN lists, sorted and deduplicated.
func mergeSANLists(a, b []string) []string {
	set := map[string]bool{}
	for _, s := range a {
		if s = strings.TrimSpace(s); s != "" {
			set[s] = true
		}
	}
	for _, s := range b {
		if s = strings.TrimSpace(s); s != "" {
			set[s] = true
		}
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
