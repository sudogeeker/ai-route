package ipv6

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"sync"
	"time"
)

// SourcePool owns immutable source ranges and can be shared by both providers.
type SourcePool struct {
	subnets []*net.IPNet
	picker  *subnet64Picker
	mu      sync.Mutex
	cooling map[string]time.Time
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
	return &SourcePool{subnets: subnets, picker: picker, cooling: make(map[string]time.Time)}, nil
}
func (p *SourcePool) Random() net.IP {
	if p == nil {
		return nil
	}
	return genRandomIPv6(p.pickSubnet())
}

// RaceCapacity caps a source-bound race to the number of distinct /64 blocks
// available. The OpenCode limiter groups IPv6 by /64, so multiple addresses in
// one block do not create independent attempts.
func (p *SourcePool) RaceCapacity(requested int) int {
	if p == nil || p.picker == nil || p.picker.total == nil || requested <= 0 {
		return 0
	}
	if p.picker.total.Cmp(big.NewInt(int64(requested))) < 0 {
		return int(p.picker.total.Int64())
	}
	return requested
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

// Cooldown removes a /64 from future Unique allocations after an upstream rate
// limit or refusal. Random allocation remains available to the Gemini provider,
// whose quotas are independent from OpenCode.
func (p *SourcePool) Cooldown(ip net.IP, duration time.Duration) {
	key := subnet64Key(ip)
	if p == nil || key == "" || duration <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cooling == nil {
		p.cooling = make(map[string]time.Time)
	}
	until := time.Now().Add(duration)
	if until.After(p.cooling[key]) {
		p.cooling[key] = until
	}
}

func subnet64Key(ip net.IP) string {
	value := ip.To16()
	if value == nil || ip.To4() != nil {
		return ""
	}
	return string(value[:8])
}

func (p *SourcePool) coolingSnapshot() map[string]bool {
	if p == nil {
		return nil
	}
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, until := range p.cooling {
		if !until.After(now) {
			delete(p.cooling, key)
		}
	}
	blocked := make(map[string]bool, len(p.cooling))
	for key := range p.cooling {
		blocked[key] = true
	}
	return blocked
}

// Allocate before launching so concurrent attempts cannot pick the same source.
// Attempts use distinct /64 blocks where the configured ranges provide enough,
// and never reuse a block that recently returned an upstream refusal.
func (p *SourcePool) Unique(count int) ([]net.IP, error) {
	if count <= 0 {
		return nil, errors.New("source IPv6 count must be positive")
	}
	if p == nil || len(p.subnets) == 0 || p.picker == nil {
		return nil, errors.New("no source IPv6 ranges configured")
	}
	blocked := p.coolingSnapshot()
	sources := make([]net.IP, 0, count)
	seenIP := make(map[string]bool, count)
	seenSubnet := make(map[string]bool, count)
	// Prefer one request per /64 so one rate-limited subnet cannot consume the
	// whole race. Fall back to additional addresses only when the ranges have
	// fewer /64 blocks than requested attempts.
	for tries := 0; len(sources) < count && tries < count*128; tries++ {
		ip := p.Random()
		key := subnet64Key(ip)
		if ip == nil || ip.To4() != nil || seenIP[ip.String()] || seenSubnet[key] || blocked[key] {
			continue
		}
		seenIP[ip.String()] = true
		seenSubnet[key] = true
		sources = append(sources, ip)
	}
	for tries := 0; len(sources) < count && tries < count*128; tries++ {
		ip := p.Random()
		key := subnet64Key(ip)
		if ip == nil || ip.To4() != nil || seenIP[ip.String()] || blocked[key] {
			continue
		}
		seenIP[ip.String()] = true
		sources = append(sources, ip)
	}
	if len(sources) != count {
		return nil, errors.New("cannot allocate non-cooling source IPv6 addresses")
	}
	return sources, nil
}
