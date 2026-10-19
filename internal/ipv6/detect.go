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
			if n == nil || n.IP == nil {
				return nil, fmt.Errorf("invalid CIDR %q", token)
			}
			ip := n.IP.To16()
			if ip == nil || ip.To4() != nil {
				return nil, fmt.Errorf("CIDR must be IPv6: %q", token)
			}
			if len(n.Mask) != 16 {
				return nil, fmt.Errorf("CIDR mask must be IPv6: %q", token)
			}
			ones, bits := n.Mask.Size()
			if bits != 128 {
				return nil, fmt.Errorf("CIDR mask bits must be 128: %q", token)
			}
			if ones > 64 {
				return nil, fmt.Errorf("CIDR prefix must be <= 64 for /64 splitting: %q", token)
			}
			// Normalize IP to masked network address
			n.IP = ip.Mask(n.Mask)
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

	type cand struct {
		net   *net.IPNet
		score int
	}
	var best *cand

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
			if !ok || ipnet == nil || ipnet.IP == nil {
				continue
			}
			ip := ipnet.IP.To16()
			if ip == nil || ip.To4() != nil {
				continue
			}
			// Skip link-local and multicast
			if ip.IsLinkLocalUnicast() || ip.IsMulticast() {
				continue
			}
			// Skip unique local (fc00::/7) to avoid selecting private-only ranges by default
			if len(ip) == 16 && (ip[0]&0xfe) == 0xfc {
				continue
			}
			if !ip.IsGlobalUnicast() {
				continue
			}

			ones, bits := ipnet.Mask.Size()
			if bits != 128 || ones >= 128 {
				continue
			}

			networkIP := ip.Mask(ipnet.Mask)
			n := &net.IPNet{IP: networkIP, Mask: ipnet.Mask}

			// Heuristic scoring:
			// Prefer prefix <= 64 (common routed subnets) and bigger subnets (smaller ones value)
			score := 0
			if ones <= 64 {
				score += 1000
			}
			// Slightly prefer typical /64 over very small ranges like /120
			score += (128 - ones)

			if best == nil || score > best.score {
				best = &cand{net: n, score: score}
			}
		}
	}

	if best == nil || best.net == nil {
		return nil, fmt.Errorf("no suitable IPv6 subnet found")
	}
	return best.net, nil
}
