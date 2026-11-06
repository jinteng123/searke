package cluster

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const descDoc = `# user comment
apiVersion: searke.io/v1alpha1
kind: Cluster
metadata:
  name: t
spec:
  rke2:
    bundle: /tmp/nope
  ssh: {} # empty ssh block
  endpoints:
    registrationAddress: 10.0.0.1:9345
  servers:
    - { name: m1, address: 10.0.0.1 }
`

func TestUpdateDescriptor(t *testing.T) {
	f := filepath.Join(t.TempDir(), "cluster.yaml")
	if err := os.WriteFile(f, []byte(descDoc), 0o600); err != nil {
		t.Fatal(err)
	}
	err := UpdateDescriptor(f,
		[]Node{{Name: "server-2", Address: "10.0.0.2"}, {Name: "server-3", Address: "10.0.0.3"}},
		[]Node{{Name: "agent-1", Address: "10.0.0.9"}},
		&SSHSpec{User: "ubuntu", IdentityFile: "~/.ssh/id_ed25519"})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(f)
	s := string(data)
	if !strings.Contains(s, "# user comment") || !strings.Contains(s, "# empty ssh block") {
		t.Fatalf("comments lost:\n%s", s)
	}
	c, err := Load(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Spec.Servers) != 3 || len(c.Spec.Agents) != 1 {
		t.Fatalf("servers=%v agents=%v", c.Spec.Servers, c.Spec.Agents)
	}
	if c.Spec.Servers[1].Name != "server-2" || c.Spec.Agents[0].Address != "10.0.0.9" {
		t.Fatalf("bad append: %v / %v", c.Spec.Servers[1], c.Spec.Agents[0])
	}
	if c.Spec.SSH.User != "ubuntu" || c.Spec.SSH.IdentityFile != "~/.ssh/id_ed25519" {
		t.Fatalf("ssh not updated: %+v", c.Spec.SSH)
	}
}

func TestUpdateDescriptorMissingAgents(t *testing.T) {
	f := filepath.Join(t.TempDir(), "cluster.yaml")
	if err := os.WriteFile(f, []byte(descDoc), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := UpdateDescriptor(f, nil, []Node{{Name: "agent-1", Address: "10.0.0.9"}}, &SSHSpec{User: "ubuntu"}); err != nil {
		t.Fatal(err)
	}
	c, err := Load(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Spec.Agents) != 1 || c.Spec.Agents[0].Name != "agent-1" {
		t.Fatalf("agents not created: %v", c.Spec.Agents)
	}
}
