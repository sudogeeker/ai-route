package ipv6

import (
	"net"
	"testing"
)

func TestSourcePoolUniqueAndIsolation(t *testing.T) {
	for _, cidr := range []string{"2001:db8:1234::/48", "2001:db8:5678::/64"} {
		pool, err := NewSourcePool([]string{cidr})
		if err != nil {
			t.Fatal(err)
		}
		_, subnet, _ := net.ParseCIDR(cidr)
		sources, err := pool.Unique(10)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, ip := range sources {
			if !subnet.Contains(ip) || seen[ip.String()] {
				t.Fatalf("invalid or duplicate source %s", ip)
			}
			seen[ip.String()] = true
		}
		if !subnet.Contains(pool.Random()) {
			t.Fatal("source pool used another instance's ranges")
		}
	}
}

func TestInvalidSourceRanges(t *testing.T) {
	for _, cidr := range []string{"invalid", "192.0.2.0/24", "2001:db8::/65", "2001:db8::/128"} {
		if _, err := NewSourcePool([]string{cidr}); err == nil {
			t.Errorf("accepted invalid source range %q", cidr)
		}
	}
	var empty *SourcePool
	if _, err := empty.Unique(10); err == nil {
		t.Fatal("missing source ranges accepted")
	}
	if _, err := empty.Unique(-1); err == nil {
		t.Fatal("negative source count accepted")
	}
}
