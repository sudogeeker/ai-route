package ipv6

import (
	"fmt"
	"net"
	"strings"
)

func parseIPv6CIDRs(cidrs []string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, raw := range cidrs {
		for _, token := range splitCIDRInput(raw) {
			_, n, err := net.ParseCIDR(token)
			if err != nil {
				return nil, fmt.Errorf("invalid CIDR %q: %v", token, err)
			}
			if n.IP.To4() != nil {
				return nil, fmt.Errorf("CIDR must be IPv6: %q", token)
			}
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty CIDR list")
	}
	return out, nil
}

func splitCIDRInput(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	}) {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// detectLocalIPv6Subnet tries to infer a usable IPv6 subnet from local interfaces,
// without calling external commands. It prefers global unicast addresses with a
// prefix length < 128 and skips link-local (fe80::/10) and other non-routable ranges.
func detectLocalIPv6Subnet() (*net.IPNet, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}

	var best *net.IPNet
	bestPrefix := 128

	for _, ifc := range ifaces {
		// Skip down or loopback interfaces
		if (ifc.Flags&net.FlagUp) == 0 || (ifc.Flags&net.FlagLoopback) != 0 {
			continue
		}

		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet == nil {
				continue
			}
			ip := ipnet.IP.To16()
			if ip.To4() != nil || !ip.IsGlobalUnicast() {
				continue
			}
			// Skip unique local (fc00::/7) to avoid selecting private-only ranges by default
			if (ip[0] & 0xfe) == 0xfc {
				continue
			}

			ones, bits := ipnet.Mask.Size()
			if bits == 128 && ones < bestPrefix {
				bestPrefix = ones
				best = &net.IPNet{IP: ip.Mask(ipnet.Mask), Mask: ipnet.Mask}
			}
		}
	}

	if best == nil {
		return nil, fmt.Errorf("no suitable IPv6 subnet found")
	}
	return best, nil
}
