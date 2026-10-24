package warp

import (
	"context"
	"net/http"
	"net/netip"
	"strings"
	"testing"
)

func TestExitRotatesOnNetworkForbidden(t *testing.T) {
	e := &exit{}
	rotate, reason := e.observe(http.StatusForbidden, []byte(`{"error":"network anomaly detected"}`))
	if !rotate || !strings.Contains(reason, "network anomaly") {
		t.Fatalf("network 403 did not rotate: %t %q", rotate, reason)
	}
}

func TestExitRotatesAfterTenConsecutive429(t *testing.T) {
	e := &exit{}
	for i := 0; i < rateLimitWindow-1; i++ {
		if rotate, _ := e.observe(http.StatusTooManyRequests, nil); rotate {
			t.Fatalf("rotated after %d responses", i+1)
		}
	}
	if rotate, reason := e.observe(http.StatusTooManyRequests, nil); !rotate || !strings.Contains(reason, "10 consecutive") {
		t.Fatalf("tenth 429 did not rotate: %t %q", rotate, reason)
	}
	if rotate, _ := e.observe(http.StatusTooManyRequests, nil); rotate {
		t.Fatal("rate-limit history did not reset after rotation")
	}
}

func TestExitSuccessResets429Window(t *testing.T) {
	e := &exit{}
	for i := 0; i < rateLimitWindow-1; i++ {
		e.observe(http.StatusTooManyRequests, nil)
	}
	e.observe(http.StatusOK, nil)
	for i := 0; i < rateLimitWindow-1; i++ {
		if rotate, _ := e.observe(http.StatusTooManyRequests, nil); rotate {
			t.Fatalf("stale 429 history survived success at response %d", i+1)
		}
	}
}

func TestEgressSubnet64(t *testing.T) {
	e := &exit{egress: netip.MustParseAddr("2a09:bac1:1234:5678::42")}
	if got := e.egressKey(); got != "2a09:bac1:1234:5678::/64" {
		t.Fatalf("egress subnet=%q", got)
	}
}

func TestEgressKeyIPv4(t *testing.T) {
	e := &exit{egress: netip.MustParseAddr("104.16.132.229")}
	if got := e.egressKey(); got != "104.16.132.229/32" {
		t.Fatalf("IPv4 egress key=%q", got)
	}
}

func TestParseStack(t *testing.T) {
	for input, want := range map[string]Stack{
		"":       StackIPv4,
		"ipv6":   StackIPv6,
		" V6 ":   StackIPv6,
		"ipv4":   StackIPv4,
		"v4":     StackIPv4,
		"dual":   StackDual,
		" both ": StackDual,
	} {
		got, err := ParseStack(input)
		if err != nil || got != want {
			t.Fatalf("ParseStack(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	if _, err := ParseStack("ipv5"); err == nil {
		t.Fatal("invalid stack was accepted")
	}
}

func TestExitDialNetwork(t *testing.T) {
	for _, test := range []struct {
		stack  Stack
		egress string
		want   string
	}{
		{StackIPv6, "2a09:bac1:1234:5678::42", "tcp6"},
		{StackIPv4, "104.16.132.229", "tcp4"},
		{StackDual, "2a09:bac1:1234:5678::42", "tcp6"},
		{StackDual, "104.16.132.229", "tcp4"},
	} {
		e := &exit{stack: test.stack, egress: netip.MustParseAddr(test.egress)}
		if got := e.dialNetwork(); got != test.want {
			t.Fatalf("stack=%s egress=%s network=%q, want %q", test.stack, test.egress, got, test.want)
		}
	}
}

func TestParseTrace(t *testing.T) {
	addr, err := parseTrace(strings.NewReader("ip=2a09:bac1:1234:5678::42\nwarp=on\n"))
	if err != nil || addr.String() != "2a09:bac1:1234:5678::42" {
		t.Fatalf("trace parse: %s %v", addr, err)
	}
	addr, err = parseTrace(strings.NewReader("ip=104.16.132.229\nwarp=on\n"))
	if err != nil || addr.String() != "104.16.132.229" || !addr.Is4() {
		t.Fatalf("IPv4 trace parse: %s %v", addr, err)
	}
	if _, err := parseTrace(strings.NewReader("ip=2a09:bac1:1234:5678::42\nwarp=off\n")); err == nil {
		t.Fatal("warp=off trace was accepted")
	}
}

func TestNormalizeEndpoint(t *testing.T) {
	for input, want := range map[string]string{
		"162.159.192.4":                  "162.159.192.4:2408",
		"162.159.192.4:0":                "162.159.192.4:2408",
		"2606:4700:d0::a29f:c004":        "[2606:4700:d0::a29f:c004]:2408",
		"[2606:4700:d0::a29f:c004]:0500": "[2606:4700:d0::a29f:c004]:0500",
	} {
		got, err := normalizeEndpoint(input)
		if err != nil || got != want {
			t.Fatalf("normalize %q = %q, %v; want %q", input, got, err, want)
		}
	}
}

func TestResolveEndpointsPreservesPriority(t *testing.T) {
	got, err := resolveEndpoints(context.Background(), []string{
		"162.159.192.4:0",
		"[2606:4700:d0::a29f:c004]:500",
		"162.159.192.4:2408",
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"162.159.192.4:2408", "[2606:4700:d0::a29f:c004]:500"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("resolved endpoints = %v, want %v", got, want)
	}
}
