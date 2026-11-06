package deploy

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/jint/searke/internal/cluster"
)

// RunOptions configures `searke run`: a single-node install on this
// machine, comparable to `sealos run --single`. Empty fields fall back
// to the defaults noted on each one.
type RunOptions struct {
	Bundle string // positional arg: OCI dir, .oci.tar, or registry ref

	Name        string // cluster name (default "searke-<hostname>")
	NodeName    string // Kubernetes node name (default sanitized hostname)
	Address     string // registration/API IP (default primary IPv4)
	CNI         string // default "canal"
	ClusterCIDR string // default "10.42.0.0/16"
	ServiceCIDR string // default "10.43.0.0/16"
	HomeDir     string // generated cluster.yaml + state dir (default ~/.searke/<name>)
}

// SingleNodeCluster builds an in-memory one-server cluster whose only
// node is this machine (driven by local execution), validates it, and
// writes the generated cluster.yaml so later `status`, `snapshot`, or
// resume runs can reuse it via -f. Returns the cluster and the file path.
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
	if ro.NodeName == "" {
		ro.NodeName = cluster.SanitizeNodeName(host)
	}
	if ro.Address == "" {
		ro.Address = cluster.PrimaryIPv4()
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
	if ro.HomeDir == "" {
		if h, err := os.UserHomeDir(); err == nil {
			ro.HomeDir = filepath.Join(h, ".searke", ro.Name)
		} else {
			ro.HomeDir = ro.Name
		}
	}

	c := &cluster.Cluster{APIVersion: cluster.APIVersion, Kind: cluster.Kind}
	c.Metadata.Name = ro.Name
	c.Spec.RKE2.Bundle = ro.Bundle
	c.Spec.RKE2.CNI = ro.CNI
	// Local execution ignores SSH auth fields; sudo:true enables the
	// non-root `sudo -n` path (harmless when running as root).
	c.Spec.SSH.Sudo = true
	c.Spec.Endpoints.RegistrationAddress = ro.Address + ":9345"
	c.Spec.Endpoints.APIAddress = ro.Address + ":6443"
	c.Spec.Servers = []cluster.Node{{Name: ro.NodeName, Address: "127.0.0.1"}}
	c.Spec.RKE2Config = map[string]any{
		"cluster-cidr": ro.ClusterCIDR,
		"service-cidr": ro.ServiceCIDR,
	}
	if err := c.Validate(); err != nil {
		return nil, "", fmt.Errorf("generated cluster invalid: %w", err)
	}

	if err := os.MkdirAll(ro.HomeDir, 0o700); err != nil {
		return nil, "", err
	}
	file := filepath.Join(ro.HomeDir, "cluster.yaml")
	data, err := yaml.Marshal(c)
	if err != nil {
		return nil, "", err
	}
	if err := os.WriteFile(file, data, 0o600); err != nil {
		return nil, "", err
	}
	return c, file, nil
}
