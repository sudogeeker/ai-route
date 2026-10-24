package warp

import (
	"context"
	"fmt"
	"net/netip"
	"sync"
	"testing"
	"time"
)

type exitFactory struct {
	mu    sync.Mutex
	count int
}

func (f *exitFactory) create(context.Context) (*exit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.count++
	return &exit{
		id:     fmt.Sprintf("warp-%d", f.count),
		egress: netip.MustParseAddr(fmt.Sprintf("2a09:bac1:%x::1", f.count)),
	}, nil
}

func TestPoolReplacesPenalizedExit(t *testing.T) {
	factory := &exitFactory{}
	pool, err := newPool(context.Background(), nil, 1, factory.create)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	lease, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	first := lease.String()
	pool.Report(lease, 403, []byte("network anomaly"))
	lease.Release()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	replacement, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.String() == first {
		t.Fatalf("penalized exit was reused: %s", first)
	}
	replacement.Release()
}

func TestPoolUsesPrewarmedStandby(t *testing.T) {
	factory := &exitFactory{}
	pool, err := newPool(context.Background(), nil, 2, factory.create)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	deadline := time.Now().Add(time.Second)
	for {
		pool.mu.Lock()
		ready := len(pool.standby) == 1
		pool.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("standby was not pre-registered")
		}
		time.Sleep(time.Millisecond)
	}

	lease, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	first := lease.String()
	pool.Report(lease, 403, nil)

	next, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if next.String() == first {
		t.Fatalf("standby swap reused %s", first)
	}
	next.Release()
	lease.Release()
}

func TestPoolDistributesAcrossActiveExits(t *testing.T) {
	factory := &exitFactory{}
	pool, err := newPool(context.Background(), nil, 3, factory.create)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	deadline := time.Now().Add(2 * time.Second)
	for {
		pool.mu.Lock()
		ready := len(pool.active) == 2 && len(pool.standby) == 1
		pool.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("multi-active pool did not converge")
		}
		time.Sleep(time.Millisecond)
	}

	first, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.String() == second.String() {
		t.Fatalf("round robin reused active exit: %s", first.String())
	}
	pool.Report(first, 403, nil)
	first.Release()
	second.Release()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	replacement, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	replacement.Release()
}

func TestPoolRejectsDuplicateAndCoolingEgress64(t *testing.T) {
	var calls int
	var mu sync.Mutex
	create := func(context.Context) (*exit, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		subnet := 0x1
		if calls >= 3 {
			subnet = 0x2
		}
		return &exit{
			id:     fmt.Sprintf("warp-%d", calls),
			egress: netip.MustParseAddr(fmt.Sprintf("2a09:bac1:%x::1", subnet)),
		}, nil
	}
	pool, err := newPool(context.Background(), nil, 1, create)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	lease, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	pool.Report(lease, 403, nil)
	lease.Release()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	replacement, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := replacement.EgressKey(); got != "2a09:bac1:2::/64" {
		t.Fatalf("duplicate/cooling /64 was reused: %s", got)
	}
	replacement.Release()
}

func TestPoolRejectsDuplicateAndCoolingEgress32(t *testing.T) {
	var calls int
	var mu sync.Mutex
	create := func(context.Context) (*exit, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		address := 1
		if calls >= 3 {
			address = 2
		}
		return &exit{
			id:     fmt.Sprintf("warp-%d", calls),
			stack:  StackIPv4,
			egress: netip.MustParseAddr(fmt.Sprintf("104.16.132.%d", address)),
		}, nil
	}
	pool, err := newPool(context.Background(), nil, 1, create)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	lease, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	pool.Report(lease, 403, nil)
	lease.Release()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	replacement, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := replacement.EgressKey(); got != "104.16.132.2/32" {
		t.Fatalf("duplicate/cooling /32 was reused: %s", got)
	}
	replacement.Release()
}
