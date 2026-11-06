package cluster

import "testing"

func valid() *Cluster {
	c := &Cluster{APIVersion: APIVersion, Kind: Kind}
	c.Metadata.Name = "test"
	c.Spec.RKE2.Bundle = "./bundle.oci.tar"
	c.Spec.SSH.User = "ubuntu"
	c.Spec.Endpoints.RegistrationAddress = "10.0.0.100:9345"
	c.Spec.Servers = []Node{
		{Name: "m1", Address: "10.0.0.11"},
		{Name: "m2", Address: "10.0.0.12"},
		{Name: "m3", Address: "10.0.0.13"},
	}
	c.Spec.Agents = []Node{{Name: "w1", Address: "10.0.0.21"}}
	return c
}

func TestValidateOK(t *testing.T) {
	c := valid()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Spec.SSH.Port != 22 {
		t.Fatalf("default ssh port should be 22, got %d", c.Spec.SSH.Port)
	}
	if c.FirstServer().Name != "m1" {
		t.Fatalf("first server should be deterministic m1, got %s", c.FirstServer().Name)
	}
	if c.RoleOf("w1") != "agent" {
		t.Fatal("w1 should be agent")
	}
}

func TestValidateRejects(t *testing.T) {
	cases := map[string]func(*Cluster){
		"bad apiVersion":   func(c *Cluster) { c.APIVersion = "v2" },
		"even servers":     func(c *Cluster) { c.Spec.Servers = c.Spec.Servers[:2] },
		"no servers":       func(c *Cluster) { c.Spec.Servers = nil },
		"no reg address":   func(c *Cluster) { c.Spec.Endpoints.RegistrationAddress = "" },
		"bad reg address":  func(c *Cluster) { c.Spec.Endpoints.RegistrationAddress = "10.0.0.100" },
		"dup node name":    func(c *Cluster) { c.Spec.Agents = append(c.Spec.Agents, Node{Name: "m1", Address: "10.0.0.99"}) },
		"managed server":   func(c *Cluster) { c.Spec.RKE2Config = map[string]any{"server": "x"} },
		"managed token":    func(c *Cluster) { c.Spec.RKE2Config = map[string]any{"token": "x"} },
		"no ssh user":      func(c *Cluster) { c.Spec.SSH.User = "" },
		"no bundle":        func(c *Cluster) { c.Spec.RKE2.Bundle = "" },
		"empty node name":  func(c *Cluster) { c.Spec.Servers[0].Name = "" },
	}
	for name, mutate := range cases {
		c := valid()
		mutate(c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: should fail", name)
		}
	}
}
