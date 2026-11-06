package cluster

import (
	"net"
	"os"
	"testing"
)

func TestIsLocalAddress(t *testing.T) {
	for _, addr := range []string{
		"127.0.0.1", "127.0.0.1:22", "127.1.2.3", "::1", "[::1]:22",
		"localhost", "localhost:9345",
	} {
		if !IsLocalAddress(addr) {
			t.Errorf("%s should be local", addr)
		}
	}
	if hn, err := os.Hostname(); err == nil && !IsLocalAddress(hn) {
		t.Errorf("own hostname %s should be local", hn)
	}
	addrs, _ := net.InterfaceAddrs()
	foundIface := false
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() {
			if !IsLocalAddress(ipn.IP.String()) {
				t.Errorf("interface address %s should be local", ipn.IP)
			}
			foundIface = true
		}
	}
	if !foundIface {
		t.Log("no non-loopback interface found; skipped iface check")
	}
	for _, addr := range []string{"10.255.255.255", "192.0.2.1", "8.8.8.8"} {
		if IsLocalAddress(addr) {
			t.Errorf("%s must not be detected as local", addr)
		}
	}
}

func TestValidateAllLocalNeedsNoSSHUser(t *testing.T) {
	c := &Cluster{APIVersion: APIVersion, Kind: Kind}
	c.Metadata.Name = "local"
	c.Spec.RKE2.Bundle = "./b.oci"
	c.Spec.Endpoints.RegistrationAddress = "127.0.0.1:9345"
	c.Spec.Servers = []Node{{Name: "m1", Address: "127.0.0.1"}}
	// no spec.ssh at all
	if err := c.Validate(); err != nil {
		t.Fatalf("all-local cluster should validate without ssh.user: %v", err)
	}
	c.Spec.Servers = append(c.Spec.Servers, Node{Name: "m2", Address: "127.0.0.1"},
		Node{Name: "m3", Address: "10.0.0.13"})
	if err := c.Validate(); err == nil {
		t.Fatal("a remote node must still require spec.ssh.user")
	}
}
