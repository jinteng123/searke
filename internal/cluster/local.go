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

// PrimaryIPv4 returns the IPv4 address other nodes would use to reach
// this machine: the source address of the default route (matching how
// RKE2 picks its node-ip), falling back to the first global IPv4 on any
// interface, then "127.0.0.1".
func PrimaryIPv4() string {
	// UDP dial sends no packets; it only resolves the route, giving the
	// address on the default-route interface even with several NICs.
	if conn, err := net.Dial("udp", "198.51.100.1:80"); err == nil {
		defer conn.Close()
		if ua, ok := conn.LocalAddr().(*net.UDPAddr); ok {
			if v4 := ua.IP.To4(); v4 != nil {
				return v4.String()
			}
		}
	}
	for _, ip := range localIPs() {
		if v4 := ip.To4(); v4 != nil && v4.IsGlobalUnicast() && !v4.IsLinkLocalUnicast() {
			return v4.String()
		}
	}
	return "127.0.0.1"
}

// SanitizeNodeName converts an arbitrary string (typically a hostname)
// into a DNS-1123 subdomain suitable for a Kubernetes node name:
// lowercase, invalid characters become '-', ends trimmed to alphanumerics.
func SanitizeNodeName(s string) string {
	s = strings.ToLower(s)
	var b []rune
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.':
			b = append(b, r)
		default:
			b = append(b, '-')
		}
	}
	out := strings.Trim(string(b), "-.")
	// Each dot-separated label must also start and end with an
	// alphanumeric: "a_.b" -> "a-.b" is still invalid.
	var labels []string
	for _, lbl := range strings.Split(out, ".") {
		if lbl = strings.Trim(lbl, "-"); lbl != "" {
			labels = append(labels, lbl)
		}
	}
	out = strings.Join(labels, ".")
	if out == "" {
		out = "node"
	}
	if len(out) > 63 {
		out = strings.TrimRight(out[:63], "-.")
	}
	return out
}
