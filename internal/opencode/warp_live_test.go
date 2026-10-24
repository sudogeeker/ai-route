package opencode

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"testing"
	"time"

	"aizasy.pages.dev/ai-route/internal/config"
	"aizasy.pages.dev/ai-route/internal/warp"
)

// This opt-in diagnostic sends one anonymous request through each pre-warmed
// WARP exit and reports the resulting egress and HTTP status.
func TestOpenCodeLiveWARPChat(t *testing.T) {
	if os.Getenv("OPENCODE_WARP_LIVE_TEST") != "1" {
		t.Skip("set OPENCODE_WARP_LIVE_TEST=1 to send requests through real WARP exits")
	}
	model := os.Getenv("OPENCODE_LIVE_MODEL")
	if model == "" {
		model = "deepseek-v4.1-flash-free"
	}
	base, err := url.Parse("https://opencode.ai/inference/openai/v1")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	stack, err := warp.ParseStack(os.Getenv("OPENCODE_WARP_STACK"))
	if err != nil {
		t.Fatal(err)
	}
	pool, err := warp.NewWithStack(ctx, nil, 4, stack)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	t.Log("waiting for WARP exits to prewarm")
	time.Sleep(35 * time.Second)

	p := NewWithWARP(config.OpenCode{BaseURL: base, FreeCompat: true, WARP: true}, nil, nil, pool)
	for attempt := 1; attempt <= 4; attempt++ {
		body, err := json.Marshal(map[string]any{
			"model": model, "messages": []map[string]string{{"role": "user", "content": "Reply exactly OK. Do not use tools."}},
			"max_tokens": 64, "stream": false,
		})
		if err != nil {
			t.Fatal(err)
		}
		request, err := prepareChat(body, false, true)
		if err != nil {
			t.Fatal(err)
		}
		identity, err := newRequestIdentity()
		if err != nil {
			t.Fatal(err)
		}
		result := p.attempt(ctx, nil, request, identity)
		status, retryAfter := 0, ""
		if result.response != nil {
			status = result.response.StatusCode
			retryAfter = result.response.Header.Get("Retry-After")
		}
		t.Logf("attempt %d egress=%s status=%d retry-after=%q error=%v body=%s", attempt, result.egress, status, retryAfter, result.err, result.body)
		result.cleanup()
	}
}
