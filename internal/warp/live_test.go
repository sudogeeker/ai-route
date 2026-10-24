package warp

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestWARPLiveRegistration(t *testing.T) {
	if os.Getenv("WARP_LIVE_TEST") != "1" {
		t.Skip("set WARP_LIVE_TEST=1 to register a real Cloudflare WARP device")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	reg, err := newRegistrar().register(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stack, err := ParseStack(os.Getenv("WARP_STACK"))
	if err != nil {
		t.Fatal(err)
	}
	egress, err := newExitWithStack(ctx, reg, stack)
	if err != nil {
		t.Fatal(err)
	}
	defer egress.close()
	t.Logf("WARP/%s device %s egress %s (%s) dial=%s", egress.stack, egress.id, egress.egress, egress.egressKey(), egress.dialNetwork())
}
