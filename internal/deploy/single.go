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
