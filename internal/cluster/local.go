package cluster

import (
	"net"
	"os"
	"strings"
)

// IsLocalAddress reports whether addr refers to this machine: a loopback
// address, "localhost", the machine's hostname, or an address bound to
// one of its interfaces. Nodes that answer true are driven by direct
// local execution instead of SSH. It performs no DNS lookups, so
// validation stays fast and offline-friendly.
func IsLocalAddress(addr string) bool {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "" || host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() {
			return true
		}
		for _, lip := range localIPs() {
			if lip.Equal(ip) {
				return true
			}
		}
		return false
	}
	if hn, err := os.Hostname(); err == nil && host == hn {
		return true
	}
	return false
}

func localIPs() []net.IP {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []net.IP
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok {
			out = append(out, ipn.IP)
		}
	}
	return out
}
