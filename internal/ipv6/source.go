package ipv6

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
)

// SourcePool owns immutable source ranges and can be shared by both providers.
type SourcePool struct {
	subnets []*net.IPNet
	picker  *subnet64Picker
}

func NewSourcePool(cidrs []string) (*SourcePool, error) {
	var subnets []*net.IPNet
	if len(cidrs) > 0 {
		var err error
		subnets, err = parseIPv6CIDRs(cidrs)
		if err != nil {
			return nil, err
		}
	} else {
		subnet, err := detectLocalIPv6Subnet()
		if err != nil {
			return nil, fmt.Errorf("no subnet detected, use -cidr or IPV6_CIDR: %w", err)
		}
		subnets = []*net.IPNet{subnet}
	}
	picker, err := buildSubnet64Picker(subnets)
	if err != nil {
		return nil, err
	}
	return &SourcePool{subnets: subnets, picker: picker}, nil
}
func (p *SourcePool) Random() net.IP {
	if p == nil {
		return nil
	}
	return genRandomIPv6(p.pickSubnet())
}

// genRandomIPv6 generates a random IP within the subnet: (Prefix & Mask) | (Random & ^Mask)
func genRandomIPv6(network *net.IPNet) net.IP {
	if network == nil {
		return nil
	}
	netIP := network.IP.To16()
	mask := network.Mask
	if netIP == nil || len(mask) != 16 {
		return nil
	}

	randBytes := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, randBytes); err != nil {
		return nil
	}

	finalIP := make(net.IP, 16)
	for i := 0; i < 16; i++ {
		finalIP[i] = (netIP[i] & mask[i]) | (randBytes[i] & ^mask[i])
	}
	return finalIP
}
func (p *SourcePool) pickSubnet() *net.IPNet {
	// Prefer /64 splitting picker when available: uniformly pick a /64 block across all CIDRs.
	if p.picker != nil {
		if sn := p.picker.pick64(); sn != nil {
			return sn
		}
	}

	// Fallback to previous behavior: uniformly pick a CIDR as-is.
	if len(p.subnets) == 0 {
		return nil
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(p.subnets))))
	if err != nil {
		return p.subnets[0]
	}
	return p.subnets[n.Int64()]
}

// Allocate before launching so concurrent attempts cannot pick the same source.
func (p *SourcePool) Unique(count int) ([]net.IP, error) {
	if count <= 0 {
		return nil, errors.New("source IPv6 count must be positive")
	}
	sources := make([]net.IP, 0, count)
	seen := make(map[string]bool, count)
	for tries := 0; len(sources) < count && tries < count*128; tries++ {
		ip := p.Random()
		if ip == nil || ip.To4() != nil || seen[ip.String()] {
			continue
		}
		seen[ip.String()] = true
		sources = append(sources, ip)
	}
	if len(sources) != count {
		return nil, errors.New("cannot allocate distinct source IPv6 addresses")
	}
	return sources, nil
}
