package ipv6

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"math/big"
	"net"
)

type subnet64Cand struct {
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
			baseWord: baseWord,
			weight:   weight,
		})
	}
	if len(p.cands) == 0 {
		return nil, fmt.Errorf("no usable IPv6 subnet(s) for /64 splitting")
	}
	return p, nil
}

func (p *subnet64Picker) pick64() uint64 {
	// Choose a /64 block uniformly across all implied /64 blocks in all CIDRs.
	offset, err := rand.Int(rand.Reader, p.total)
	if err == nil {
		for _, candidate := range p.cands {
			if offset.Cmp(candidate.weight) < 0 {
				return candidate.baseWord | offset.Uint64()
			}
			offset.Sub(offset, candidate.weight)
		}
	}
	return p.cands[0].baseWord
}
