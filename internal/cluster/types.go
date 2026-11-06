package cluster

import (
	"fmt"
	"net"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	APIVersion = "searke.io/v1alpha1"
	Kind       = "Cluster"
)

// SSHSpec describes how to reach the nodes.
type SSHSpec struct {
	User         string `yaml:"user" json:"user"`
	IdentityFile string `yaml:"identityFile,omitempty" json:"identityFile,omitempty"`
	Port         int    `yaml:"port,omitempty" json:"port,omitempty"`
	Sudo         bool   `yaml:"sudo" json:"sudo"`
}

// Endpoints describe pre-existing cluster addresses. searke does not
// create VIPs or load balancers; they must already work.
type Endpoints struct {
	RegistrationAddress string `yaml:"registrationAddress" json:"registrationAddress"`
	APIAddress          string `yaml:"apiAddress,omitempty" json:"apiAddress,omitempty"`
}

// Node is one cluster member.
type Node struct {
	Name    string `yaml:"name" json:"name"`
	Address string `yaml:"address" json:"address"`
	// Labels are applied to the Kubernetes node via rke2 config.
	Labels map[string]string `yaml:"labels,omitempty" json:"labels,omitempty"`
}

// RKE2Spec pins the bundle source and cluster-wide settings.
type RKE2Spec struct {
	// Bundle is a local OCI layout dir, a local .oci/.tar archive, or a
	// registry reference.
	Bundle string `yaml:"bundle" json:"bundle"`
	// CNI must match the bundle's CNI.
	CNI string `yaml:"cni,omitempty" json:"cni,omitempty"`
	// Version, when set, must match the bundle's recorded version.
	Version string `yaml:"version,omitempty" json:"version,omitempty"`
}

// Spec is the cluster.yaml spec section.
type Spec struct {
	RKE2       RKE2Spec            `yaml:"rke2" json:"rke2"`
	SSH        SSHSpec             `yaml:"ssh" json:"ssh"`
	Endpoints  Endpoints           `yaml:"endpoints" json:"endpoints"`
	Servers    []Node              `yaml:"servers" json:"servers"`
	Agents     []Node              `yaml:"agents,omitempty" json:"agents,omitempty"`
	RKE2Config map[string]any      `yaml:"rke2Config,omitempty" json:"rke2Config,omitempty"`
}

// Cluster is the parsed cluster.yaml document.
type Cluster struct {
	APIVersion string `yaml:"apiVersion" json:"apiVersion"`
	Kind       string `yaml:"kind" json:"kind"`
	Metadata   struct {
		Name string `yaml:"name" json:"name"`
	} `yaml:"metadata" json:"metadata"`
	Spec Spec `yaml:"spec" json:"spec"`
}

// Name returns the cluster name.
func (c *Cluster) Name() string { return c.Metadata.Name }

// Load parses and validates a cluster.yaml.
func Load(path string) (*Cluster, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Cluster
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// Validate enforces the cluster schema rules.
func (c *Cluster) Validate() error {
	if c.APIVersion != APIVersion {
		return fmt.Errorf("unsupported apiVersion %q (want %q)", c.APIVersion, APIVersion)
	}
	if c.Kind != Kind {
		return fmt.Errorf("unsupported kind %q (want %q)", c.Kind, Kind)
	}
	if c.Metadata.Name == "" {
		return fmt.Errorf("metadata.name is required")
	}
	if c.Spec.RKE2.Bundle == "" {
		return fmt.Errorf("spec.rke2.bundle is required")
	}
	if c.Spec.SSH.User == "" {
		return fmt.Errorf("spec.ssh.user is required")
	}
	if c.Spec.SSH.Port == 0 {
		c.Spec.SSH.Port = 22
	}
	if c.Spec.SSH.Port < 1 || c.Spec.SSH.Port > 65535 {
		return fmt.Errorf("spec.ssh.port %d out of range", c.Spec.SSH.Port)
	}
	if _, err := splitHostPort(c.Spec.Endpoints.RegistrationAddress); err != nil {
		return fmt.Errorf("spec.endpoints.registrationAddress: %w", err)
	}
	if c.Spec.Endpoints.APIAddress != "" {
		if _, err := splitHostPort(c.Spec.Endpoints.APIAddress); err != nil {
			return fmt.Errorf("spec.endpoints.apiAddress: %w", err)
		}
	}
	if len(c.Spec.Servers) == 0 {
		return fmt.Errorf("spec.servers must list at least one server node")
	}
	if len(c.Spec.Servers)%2 == 0 {
		return fmt.Errorf("spec.servers has %d nodes: etcd requires an odd server count", len(c.Spec.Servers))
	}
	names := map[string]string{}
	for _, list := range []struct {
		role  string
		nodes []Node
	}{{"server", c.Spec.Servers}, {"agent", c.Spec.Agents}} {
		for _, n := range list.nodes {
			if n.Name == "" || n.Address == "" {
				return fmt.Errorf("%s node requires name and address", list.role)
			}
			if prev, dup := names[n.Name]; dup {
				return fmt.Errorf("node name %q used by both %s and %s", n.Name, prev, list.role)
			}
			names[n.Name] = list.role
			// DNS resolution is intentionally not checked here: validate
			// must stay fast and work fully offline; unreachable or
			// unresolvable addresses surface during the SSH phase.
		}
	}
	if c.Spec.RKE2Config != nil {
		for _, k := range []string{"server", "token", "node-name", "cni"} {
			if _, bad := c.Spec.RKE2Config[k]; bad {
				return fmt.Errorf("spec.rke2Config.%s is managed by searke and must not be set (use spec.rke2.cni)", k)
			}
		}
	}
	return nil
}

// splitHostPort validates a host:port pair.
func splitHostPort(s string) (string, error) {
	if s == "" {
		return "", fmt.Errorf("value is required")
	}
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return "", fmt.Errorf("%q is not host:port: %w", s, err)
	}
	if host == "" || port == "" {
		return "", fmt.Errorf("%q missing host or port", s)
	}
	return s, nil
}

// AllNodes returns server nodes followed by agent nodes, sorted by name
// within each role. The first server is deterministic and is used as
// the bootstrap node.
func (c *Cluster) AllNodes() []Node {
	var out []Node
	servers := append([]Node(nil), c.Spec.Servers...)
	agents := append([]Node(nil), c.Spec.Agents...)
	sort.Slice(servers, func(i, j int) bool { return servers[i].Name < servers[j].Name })
	sort.Slice(agents, func(i, j int) bool { return agents[i].Name < agents[j].Name })
	out = append(out, servers...)
	return append(out, agents...)
}

// FirstServer returns the deterministic bootstrap server.
func (c *Cluster) FirstServer() Node {
	return c.AllNodes()[0]
}

// RoleOf reports whether a node name is a server or agent.
func (c *Cluster) RoleOf(name string) string {
	for _, n := range c.Spec.Servers {
		if n.Name == name {
			return "server"
		}
	}
	return "agent"
}

// RegistrationURL is the https URL joining nodes use.
func (c *Cluster) RegistrationURL() string {
	return "https://" + c.Spec.Endpoints.RegistrationAddress
}

// RegistrationHost returns only the host part of the registration address.
func (c *Cluster) RegistrationHost() string {
	host, _, _ := net.SplitHostPort(c.Spec.Endpoints.RegistrationAddress)
	return host
}

// ExpandPath expands a leading ~/ in paths like identityFile.
func ExpandPath(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return home + p[1:]
		}
	}
	return p
}
