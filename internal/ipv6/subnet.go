package ipv6

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"math/big"
	"net"
)

type subnet64Cand struct {
	// prefix is the original subnet prefix length (<= 64)
	prefix int
	// baseWord is the first 64 bits of the subnet network address masked to `prefix`
	baseWord uint64
	// weight is the number of /64 blocks in this subnet: 2^(64-prefix)
	weight *big.Int
}

type subnet64Picker struct {
	cands []subnet64Cand
	total *big.Int
}

func buildSubnet64Picker(subnets []*net.IPNet) (*subnet64Picker, error) {
	p := &subnet64Picker{total: new(big.Int)}
	for _, sn := range subnets {
		if sn == nil || sn.IP == nil || len(sn.Mask) != 16 {
			continue
		}
		ones, bits := sn.Mask.Size()
		if bits != 128 {
			continue
		}
		if ones > 64 {
			return nil, fmt.Errorf("CIDR prefix must be <= 64 for /64 splitting, got %s", sn.String())
		}
		// Normalize to network address
		ip := sn.IP.To16()
		if ip == nil || ip.To4() != nil {
			continue
		}
		netIP := ip.Mask(sn.Mask)
		baseWord := binary.BigEndian.Uint64(netIP[0:8])

		shift := uint(64 - ones)
		weight := new(big.Int).Lsh(big.NewInt(1), shift) // 2^(64-ones)
		p.total.Add(p.total, weight)
		p.cands = append(p.cands, subnet64Cand{
			prefix:   ones,
			baseWord: baseWord,
			weight:   weight,
		})
	}
	if len(p.cands) == 0 {
		return nil, fmt.Errorf("no usable IPv6 subnet(s) for /64 splitting")
	}
	if p.total.Sign() <= 0 {
		return nil, fmt.Errorf("invalid /64 picker total weight")
	}
	return p, nil
}

func (p *subnet64Picker) pick64() *net.IPNet {
	if p == nil || len(p.cands) == 0 || p.total == nil || p.total.Sign() <= 0 {
		return nil
	}

	// Choose a /64 block uniformly across all implied /64 blocks in all CIDRs.
	r, err := rand.Int(rand.Reader, p.total)
	if err != nil {
		// fallback: deterministically pick first
		c := p.cands[0]
		ip := make(net.IP, 16)
		binary.BigEndian.PutUint64(ip[0:8], c.baseWord)
		// lower 64 bits remain 0 for the /64 network address
		return &net.IPNet{IP: ip, Mask: net.CIDRMask(64, 128)}
	}

	tmp := new(big.Int).Set(r)
	for _, c := range p.cands {
		if tmp.Cmp(c.weight) < 0 {
			// tmp is the offset in [0, 2^(64-prefix))
			var off uint64
			if c.prefix == 0 {
				// Need full 64-bit offset. tmp < 2^64, so 8 bytes are enough.
				b := tmp.FillBytes(make([]byte, 8))
				off = binary.BigEndian.Uint64(b)
			} else {
				off = tmp.Uint64()
			}

			ip := make(net.IP, 16)
			binary.BigEndian.PutUint64(ip[0:8], c.baseWord|off)
			// lower 64 bits stay 0 for a /64 network address
			return &net.IPNet{IP: ip, Mask: net.CIDRMask(64, 128)}
		}
		tmp.Sub(tmp, c.weight)
	}

	// Shouldn't happen due to bounds, but keep a safe fallback.
	c := p.cands[len(p.cands)-1]
	ip := make(net.IP, 16)
	binary.BigEndian.PutUint64(ip[0:8], c.baseWord)
	return &net.IPNet{IP: ip, Mask: net.CIDRMask(64, 128)}
}
