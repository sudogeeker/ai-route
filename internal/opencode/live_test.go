package opencode

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"aizasy.pages.dev/ai-route/internal/config"
)

// This opt-in check sends two tiny anonymous requests to the real provider.
// It uses production Go TLS settings with the host's default egress; random
// source-bound IPv6 routing still requires validation on the deployment host.
func TestOpenCodeLiveFreeChat(t *testing.T) {
	if os.Getenv("OPENCODE_LIVE_TEST") != "1" {
		t.Skip("set OPENCODE_LIVE_TEST=1 to test the live anonymous free model")
	}
	model := os.Getenv("OPENCODE_LIVE_MODEL")
	if model == "" {
		model = "big-pickle"
	}
	baseURL := os.Getenv("OPENCODE_LIVE_BASE_URL")
	if baseURL == "" {
		baseURL = "https://opencode.ai/inference/openai/v1"
	}
	base, err := url.Parse(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	p := &Proxy{cfg: config.OpenCode{BaseURL: base, FreeCompat: true}}
	p.transport = func(source net.IP) *http.Transport {
		transport := p.newTransport(source)
		dial := (&net.Dialer{Timeout: 10 * time.Second}).DialContext
		transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			if os.Getenv("OPENCODE_LIVE_FORCE_IPV4") == "1" {
				network = "tcp4"
			}
			return dial(ctx, network, address)
		}
		return transport
	}
	for _, streaming := range []bool{false, true} {
		name := "json"
		if streaming {
			name = "sse"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			body, err := json.Marshal(map[string]any{
				"model": model, "messages": []map[string]string{{"role": "user", "content": "Reply exactly OK. Do not use tools."}},
				"max_tokens": 256,
			})
			if err != nil {
				t.Fatal(err)
			}
			started := time.Now()
			request, err := prepareChat(body, streaming, true)
			if err != nil {
				t.Fatal(err)
			}
			identity, err := newRequestIdentity()
			if err != nil {
				t.Fatal(err)
			}
			result := p.attempt(ctx, nil, request, identity)
			defer result.cleanup()
			if result.err != nil {
				retryAfter, placement := "", ""
				if result.response != nil {
					retryAfter = result.response.Header.Get("Retry-After")
					placement = result.response.Header.Get("CF-Placement")
				}
				t.Fatalf("live anonymous request failed: %v body=%s retry-after=%q cf-placement=%q", result.err, result.body, retryAfter, placement)
			}
			data := result.body
			if request.streaming {
				data, err = collectChat(result.stream)
				if err != nil {
					t.Fatal(err)
				}
			}
			var response struct {
				Choices []struct{ Message struct{ Content string } } `json:"choices"`
			}
			if err := json.Unmarshal(data, &response); err != nil || len(response.Choices) == 0 || response.Choices[0].Message.Content == "" {
				t.Fatalf("no complete chat answer: %s %v", data, err)
			}
			t.Logf("native Go model=%s HTTP %d, complete %s answer in %s: %q", model, result.response.StatusCode, name, time.Since(started).Round(time.Millisecond), response.Choices[0].Message.Content)
		})
	}
}
