package opencode

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
)

func TestDNSRefreshCoalesces(t *testing.T) {
	var lookups atomic.Int32
	dns := &dnsCache{host: "opencode.example", lookup: func(ctx context.Context, network, host string) ([]net.IP, error) {
		lookups.Add(1)
		if network != "ip6" || host != "opencode.example" {
			t.Error("wrong DNS lookup")
		}
		return []net.IP{net.ParseIP("192.0.2.1"), net.ParseIP("2001:db8::10")}, nil
	}}
	var group sync.WaitGroup
	for i := 0; i < 10; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			ip, err := dns.pick(context.Background())
			if err != nil || !ip.Equal(net.ParseIP("2001:db8::10")) {
				t.Error("wrong OpenCode destination")
			}
		}()
	}
	group.Wait()
	if lookups.Load() != 1 {
		t.Fatalf("DNS refresh was not coalesced: %d", lookups.Load())
	}

}
