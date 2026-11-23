package deploy

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"

	"gopkg.in/yaml.v3"

	"github.com/jint/searke/internal/cluster"
)

// RunOptions configures `searke run`: a single-node install on this
// machine, comparable to `sealos run --single`. Empty fields fall back
// to the defaults noted on each one.
type RunOptions struct {
	Bundle   string // positional arg: OCI dir, .oci.tar, or registry ref
	Insecure bool   // reach a registry bundle over plain HTTP

	Name        string // cluster name (default "searke-<hostname>")
	NodeName    string // Kubernetes node name (default sanitized hostname)
	Address     string // registration/API IP (default primary IPv4)
	CNI         string // default "canal"
	ClusterCIDR string // default "10.42.0.0/16"
	ServiceCIDR string // default "10.43.0.0/16"
	HomeDir     string // generated cluster.yaml + state dir (default ~/.searke/<name>)

	// Sealos-style remote install: when Masters/Nodes are given, `run`
	// generates a multi-node cluster.yaml and applies it remotely.
	Masters []string // control-plane addresses; first entry becomes the bootstrap server
	Nodes   []string // worker addresses
	Single  bool     // explicit local single-node mode (rejects masters/nodes)

	SSHUser string
	SSHKey  string
	SSHPass string // used in memory only; never written to cluster.yaml
	SSHPort int

	Log func(format string, args ...any) // optional warnings sink
}

// SingleNodeCluster builds a one-server cluster whose only node is this
// machine (driven by local execution) and persists it as cluster.yaml
// so later `status`, `snapshot`, or resume runs can reuse it via -f.
// When the generated file already exists it is reloaded rather than
// overwritten — flags passed on a rerun must match what was recorded
// (only the bundle may differ). Returns the cluster and the file path.
func SingleNodeCluster(ro RunOptions) (*cluster.Cluster, string, error) {
	if ro.Bundle == "" {
		return nil, "", fmt.Errorf("bundle is required")
	}
	host, _ := os.Hostname()
	if ro.Name == "" {
		if host != "" {
			ro.Name = "searke-" + cluster.SanitizeNodeName(host)
		} else {
			ro.Name = "searke"
		}
	}
	if ro.HomeDir == "" {
		if h, err := os.UserHomeDir(); err == nil {
			ro.HomeDir = filepath.Join(h, ".searke", ro.Name)
		} else {
			ro.HomeDir = ro.Name
		}
	}
	file := filepath.Join(ro.HomeDir, "cluster.yaml")
	if c, err := reuseCluster(file, ro); err != nil || c != nil {
		return c, file, err
	}

	if ro.NodeName == "" {
		ro.NodeName = cluster.SanitizeNodeName(host)
	}
	if ro.Address == "" {
		ro.Address = cluster.PrimaryIPv4()
	} else if !cluster.IsLocalAddress(ro.Address) && ro.Log != nil {
		ro.Log("warning: --address %s is not a local address; it is only used for the registration endpoint/TLS SAN, the install still runs on this machine", ro.Address)
	}
	if ro.CNI == "" {
		ro.CNI = "canal"
	}
	if ro.ClusterCIDR == "" {
		ro.ClusterCIDR = "10.42.0.0/16"
	}
	if ro.ServiceCIDR == "" {
		ro.ServiceCIDR = "10.43.0.0/16"
	}

	c := &cluster.Cluster{APIVersion: cluster.APIVersion, Kind: cluster.Kind}
	c.Metadata.Name = ro.Name
	c.Spec.RKE2.Bundle = ro.Bundle
	c.Spec.RKE2.CNI = ro.CNI
	c.Spec.RKE2.Insecure = ro.Insecure
	// Local execution ignores SSH auth fields; sudo:true enables the
	// non-root `sudo -n` path (harmless when running as root).
	c.Spec.SSH.Sudo = true
	c.Spec.Endpoints.RegistrationAddress = net.JoinHostPort(ro.Address, "9345")
	c.Spec.Endpoints.APIAddress = net.JoinHostPort(ro.Address, "6443")
	c.Spec.Servers = []cluster.Node{{Name: ro.NodeName, Address: "127.0.0.1"}}
	c.Spec.RKE2Config = map[string]any{
		"cluster-cidr": ro.ClusterCIDR,
		"service-cidr": ro.ServiceCIDR,
	}
	if err := c.Validate(); err != nil {
		return nil, "", fmt.Errorf("generated cluster invalid: %w", err)
	}
	if err := writeCluster(file, c); err != nil {
		return nil, "", err
	}
	return c, file, nil
}

// reuseCluster loads an existing generated cluster.yaml. Rerunning
// `searke run` converges onto the recorded spec: only a changed bundle
// argument updates the file; conflicting flags are rejected so a rerun
// can never silently produce a different cluster.
func reuseCluster(file string, ro RunOptions) (*cluster.Cluster, error) {
	if _, err := os.Stat(file); err != nil {
		return nil, nil // not generated yet
	}
	c, err := cluster.Load(file)
	if err != nil {
		return nil, err
	}
	if ro.Log != nil {
		ro.Log("reusing %s (flags on a rerun must match; edit the file or use `searke apply -f` for other changes)", file)
	}
	conflicts := map[string][2]any{}
	check := func(flag string, given, recorded any) {
		if v, ok := given.(string); ok && v == "" {
			return // flag not passed
		}
		if !reflect.DeepEqual(given, recorded) {
			conflicts[flag] = [2]any{given, recorded}
		}
	}
	check("--name", ro.Name, c.Name())
	check("--node-name", ro.NodeName, c.Spec.Servers[0].Name)
	check("--cni", ro.CNI, c.Spec.RKE2.CNI)
	check("--cluster-cidr", ro.ClusterCIDR, c.Spec.RKE2Config["cluster-cidr"])
	check("--service-cidr", ro.ServiceCIDR, c.Spec.RKE2Config["service-cidr"])
	if ro.Address != "" {
		check("--address", net.JoinHostPort(ro.Address, "9345"), c.Spec.Endpoints.RegistrationAddress)
	}
	if ro.Insecure {
		check("--insecure", true, c.Spec.RKE2.Insecure)
	}
	for flag, pair := range conflicts {
		return nil, fmt.Errorf("%s %v conflicts with %s (recorded %v); edit %s or run `searke apply -f` instead",
			flag, pair[0], file, pair[1], file)
	}
	// A different bundle argument is the one supported change: it is
	// what drives an upgrade (or the version-drift error) on rerun.
	if c.Spec.RKE2.Bundle != ro.Bundle {
		c.Spec.RKE2.Bundle = ro.Bundle
		if ro.Log != nil {
			ro.Log("updating bundle in %s -> %s", file, ro.Bundle)
		}
		if err := writeCluster(file, c); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func writeCluster(file string, c *cluster.Cluster) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(file, data, 0o600)
}

// RemoteCluster builds a multi-node cluster from sealos-style
// --masters/--nodes flags (the first master becomes the bootstrap
// server) and persists it as cluster.yaml under HomeDir, the same
// convention as SingleNodeCluster. A rerun with the same addresses
// reuses the file; a different node set is rejected in favour of
// `searke add` / `searke apply -f`.
func RemoteCluster(ro RunOptions) (*cluster.Cluster, string, error) {
	if ro.Bundle == "" {
		return nil, "", fmt.Errorf("bundle is required")
	}
	if ro.Single {
		return nil, "", fmt.Errorf("--single cannot be combined with --masters/--nodes")
	}
	if len(ro.Masters) == 0 {
		return nil, "", fmt.Errorf("--nodes requires --masters")
	}
	if ro.Name == "" {
		ro.Name = "searke"
	}
	if ro.HomeDir == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return nil, "", fmt.Errorf("resolve home directory: %w", err)
		}
		ro.HomeDir = filepath.Join(h, ".searke", ro.Name)
	}
	file := filepath.Join(ro.HomeDir, "cluster.yaml")

	// Bootstrap is the name-sorted first server, so masters[0] gets the
	// lowest name; zero-padding keeps the order stable beyond 9 nodes.
	var servers, agents []cluster.Node
	for i, a := range ro.Masters {
		servers = append(servers, cluster.Node{Name: fmt.Sprintf("server-%02d", i+1), Address: a})
	}
	for i, a := range ro.Nodes {
		agents = append(agents, cluster.Node{Name: fmt.Sprintf("agent-%02d", i+1), Address: a})
	}
	regHost := ro.Address
	if regHost == "" {
		regHost = ro.Masters[0]
	}
	if ro.CNI == "" {
		ro.CNI = "canal"
	}
	if ro.ClusterCIDR == "" {
		ro.ClusterCIDR = "10.42.0.0/16"
	}
	if ro.ServiceCIDR == "" {
		ro.ServiceCIDR = "10.43.0.0/16"
	}

	want := &cluster.Cluster{APIVersion: cluster.APIVersion, Kind: cluster.Kind}
	want.Metadata.Name = ro.Name
	want.Spec.RKE2.Bundle = ro.Bundle
	want.Spec.RKE2.CNI = ro.CNI
	want.Spec.RKE2.Insecure = ro.Insecure
	want.Spec.SSH = cluster.SSHSpec{
		User:         ro.SSHUser,
		IdentityFile: ro.SSHKey,
		Port:         ro.SSHPort,
		Sudo:         true,
	}
	want.Spec.Endpoints.RegistrationAddress = net.JoinHostPort(regHost, "9345")
	want.Spec.Endpoints.APIAddress = net.JoinHostPort(regHost, "6443")
	want.Spec.Servers = servers
	want.Spec.Agents = agents
	want.Spec.RKE2Config = map[string]any{
		"cluster-cidr": ro.ClusterCIDR,
		"service-cidr": ro.ServiceCIDR,
	}
	// Password auth leaves no credential behind for later commands, so
	// when the operator gave a password but no --ssh-key, generate a
	// per-cluster key up front: it is recorded as identityFile and
	// authorized on each node during apply (see maybeProvisionKey).
	// Generated before the rerun comparison so sameRemoteSpec can treat
	// it as an upgrade path rather than a user flag conflict.
	var managed string
	if ro.SSHPass != "" && ro.SSHKey == "" {
		if priv, _, err := ensureManagedKey(filepath.Dir(file), ro.Name); err != nil {
			if ro.Log != nil {
				ro.Log("warning: cannot generate cluster SSH key (%v); password-installed nodes will not be reachable by later commands", err)
			}
		} else {
			managed = priv
			want.Spec.SSH.IdentityFile = priv
		}
	}
	if err := want.Validate(); err != nil {
		return nil, "", fmt.Errorf("generated cluster invalid: %w", err)
	}

	c := want
	if _, err := os.Stat(file); err == nil {
		existing, err := cluster.Load(file)
		if err != nil {
			return nil, "", err
		}
		if !sameRemoteSpec(existing, want, managedKeyPath(filepath.Dir(file))) {
			return nil, "", fmt.Errorf("%s already exists with a different node set; "+
				"use `searke add` / `searke apply -f`, or pass --name/--home for a separate cluster", file)
		}
		if existing.Spec.RKE2.Bundle != ro.Bundle {
			existing.Spec.RKE2.Bundle = ro.Bundle
			if err := writeCluster(file, existing); err != nil {
				return nil, "", err
			}
		}
		c = existing
	} else if err := writeCluster(file, want); err != nil {
		return nil, "", err
	}
	// Password is applied to the in-memory cluster only — secrets are
	// never persisted to the descriptor. A descriptor from before the
	// managed key existed gets it recorded now so password-installed
	// nodes become key-reachable too.
	c.Spec.SSH.Password = ro.SSHPass
	if managed != "" && c.Spec.SSH.IdentityFile == "" {
		c.Spec.SSH.IdentityFile = managed
		if err := writeCluster(file, c); err != nil && ro.Log != nil {
			ro.Log("warning: cannot record cluster SSH key in %s: %v", file, err)
		}
	}
	return c, file, nil
}

// sameRemoteSpec compares the parts of a generated spec that a rerun
// must keep identical; the bundle is allowed to differ (it drives an
// upgrade on rerun). The managed key path is exempt in either
// direction: it is auto-generated, so recording it (or having it
// recorded already) never counts as a user-flag conflict.
func sameRemoteSpec(got, want *cluster.Cluster, managed string) bool {
	if got.Spec.SSH.IdentityFile != want.Spec.SSH.IdentityFile &&
		want.Spec.SSH.IdentityFile != managed &&
		!(want.Spec.SSH.IdentityFile == "" && got.Spec.SSH.IdentityFile == managed) {
		return false
	}
	if got.Name() != want.Name() ||
		got.Spec.RKE2.CNI != want.Spec.RKE2.CNI ||
		got.Spec.RKE2.Insecure != want.Spec.RKE2.Insecure ||
		got.Spec.Endpoints != want.Spec.Endpoints ||
		got.Spec.SSH.User != want.Spec.SSH.User ||
		got.Spec.SSH.Port != want.Spec.SSH.Port ||
		got.Spec.RKE2Config["cluster-cidr"] != want.Spec.RKE2Config["cluster-cidr"] ||
		got.Spec.RKE2Config["service-cidr"] != want.Spec.RKE2Config["service-cidr"] {
		return false
	}
	return sameAddrs(got.Spec.Servers, want.Spec.Servers) && sameAddrs(got.Spec.Agents, want.Spec.Agents)
}

func sameAddrs(a, b []cluster.Node) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]bool{}
	for _, n := range a {
		seen[n.Address] = true
	}
	for _, n := range b {
		if !seen[n.Address] {
			return false
		}
	}
	return true
}
