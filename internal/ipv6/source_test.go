package ipv6

import (
	"net"
	"testing"
	"time"
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

func TestSourcePoolUsesDistinctSubnets(t *testing.T) {
	pool, err := NewSourcePool([]string{"2001:db8:1200::/56"})
	if err != nil {
		t.Fatal(err)
	}
	sources, err := pool.Unique(10)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, source := range sources {
		key := subnet64Key(source)
		if seen[key] {
			t.Fatalf("reused /64 in one race: %s", source)
		}
		seen[key] = true
	}
}

func TestSourcePoolRaceCapacity(t *testing.T) {
	for _, test := range []struct {
		cidr string
		want int
	}{
		{"2001:db8:1234::/64", 1},
		{"2001:db8:1200::/56", 10},
		{"2001:db8::/48", 10},
	} {
		pool, err := NewSourcePool([]string{test.cidr})
		if err != nil {
			t.Fatal(err)
		}
		if got := pool.RaceCapacity(10); got != test.want {
			t.Errorf("RaceCapacity(%s)=%d, want %d", test.cidr, got, test.want)
		}
	}
}

func TestSourcePoolCooldown(t *testing.T) {
	pool, err := NewSourcePool([]string{"2001:db8:1200::/56"})
	if err != nil {
		t.Fatal(err)
	}
	source := pool.Random()
	pool.Cooldown(source, time.Minute)
	sources, err := pool.Unique(64)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range sources {
		if subnet64Key(candidate) == subnet64Key(source) {
			t.Fatalf("cooled /64 was allocated again: %s", candidate)
		}
	}
}

func TestSourcePoolSingleSubnetExhaustion(t *testing.T) {
	pool, err := NewSourcePool([]string{"2001:db8:1234::/64"})
	if err != nil {
		t.Fatal(err)
	}
	sources, err := pool.Unique(10)
	if err != nil || len(sources) != 10 {
		t.Fatalf("single /64 must still provide a race: %v %v", sources, err)
	}
	pool.Cooldown(sources[0], time.Minute)
	if _, err := pool.Unique(1); err == nil {
		t.Fatal("allocated from an exhausted cooling pool")
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
