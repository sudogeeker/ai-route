package config

import (
	"strings"
	"testing"
	"time"
)

func TestOpenCodeYAML(t *testing.T) {
	defaults, err := decode(strings.NewReader("opencode: {enabled: true}"))
	if err != nil {
		t.Fatal(err)
	}
	opts := defaults.OpenCode
	if opts == nil || len(opts.Models) != 46 || opts.Models["jev-1.13-free"] || !opts.Models["flint-40b-free"] ||
		!opts.AllowImplicitFree || !opts.FreeCompat || opts.WARP || opts.WARPPoolSize != DefaultOpenCodeWARPPoolSize ||
		opts.WARPStack != DefaultOpenCodeWARPStack || opts.Concurrency != DefaultOpenCodeConcurrency ||
		opts.Retries != DefaultOpenCodeRetries || opts.FirstTokenTimeout != DefaultOpenCodeFirstTokenGrace ||
		opts.StreamIdleTimeout != time.Minute || opts.WaitTimeout != 3*time.Minute || opts.Cooldown != DefaultOpenCodeCooldown ||
		opts.MaxBodyBytes != 64<<20 || opts.LargeContextBytes != DefaultOpenCodeLargeContextBytes ||
		opts.LargeContextRace != DefaultOpenCodeLargeContextRace {
		t.Fatalf("unexpected OpenCode defaults: %+v", opts)
	}

	cfg, err := decode(strings.NewReader(`
gemini:
  enabled: false
opencode:
  enabled: true
  base_url: https://inference.example/v1/
  auth_key: " local-key "
  api_key: upstream-key
  free_compat: false
  models: [big-pickle, mimo-v2.6-flash-free]
  concurrency: 32
  large_context_bytes: 0
  retries: 0
  cooldown: 0s
  stream_idle_timeout: 15s
  warp: true
  warp_pool_size: 3
  warp_stack: v6
  proxy_urls: ["http://user:pass@proxy.example:8080", "socks5h://127.0.0.1:1080"]
  workspace: /workspace/project
  workspace_root: /workspace
`))
	if err != nil {
		t.Fatal(err)
	}
	opts = cfg.OpenCode
	if cfg.Gemini != nil || opts.BaseURL.String() != "https://inference.example/v1" || opts.AuthKey != "local-key" ||
		opts.APIKey != "upstream-key" || opts.FreeCompat || opts.AllowImplicitFree || len(opts.Models) != 2 ||
		opts.Concurrency != 32 || opts.LargeContextBytes != 0 || opts.Retries != 0 || opts.Cooldown != 0 ||
		opts.StreamIdleTimeout != 15*time.Second || !opts.WARP || opts.WARPPoolSize != 3 || opts.WARPStack != "ipv6" ||
		len(opts.ProxyURLs) != 2 || opts.ProxyURLs[0].Host != "proxy.example:8080" || opts.ProxyURLs[1].Scheme != "socks5h" ||
		opts.Workspace != "/workspace/project" || opts.WorkspaceRoot != "/workspace" {
		t.Fatalf("unexpected OpenCode settings: %+v", opts)
	}
}

func TestRejectInvalidOpenCodeSettings(t *testing.T) {
	for _, test := range []struct{ yaml, field string }{
		{"base_url: 'http://example.com'", "base_url"},
		{"base_url: 'https://key@example.com'", "base_url"},
		{"base_url: 'https://example.com?key=secret'", "base_url"},
		{"models: []", "models"},
		{"models: ['']", "models"},
		{"models: [opencode/big-pickle]", "models"},
		{"concurrency: 0", "concurrency"},
		{"large_context_bytes: -1", "large_context_bytes"},
		{"large_context_concurrency: 0", "large_context_concurrency"},
		{"retries: -1", "retries"},
		{"wait_timeout: 0s", "wait_timeout"},
		{"first_token_timeout: 0s", "first_token_timeout"},
		{"stream_idle_timeout: -1s", "stream_idle_timeout"},
		{"cooldown: -1s", "cooldown"},
		{"max_body_bytes: -1", "max_body_bytes"},
		{"warp_pool_size: 0", "warp_pool_size"},
		{"warp_pool_size: 17", "warp_pool_size"},
		{"warp_stack: ipv5", "warp_stack"},
		{"proxy_urls: ['ftp://proxy.example']", "proxy_urls"},
		{"proxy_urls: ['http://proxy.example/path']", "proxy_urls"},
		{"proxy_urls: ['http://proxy.example?']", "proxy_urls"},
	} {
		t.Run(test.yaml, func(t *testing.T) {
			_, err := decode(strings.NewReader("opencode:\n  enabled: true\n  " + test.yaml))
			if err == nil || !strings.Contains(err.Error(), "opencode."+test.field) {
				t.Fatalf("want error for %s, got %v", test.field, err)
			}
		})
	}
}
