package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// OpenCodeUserAgent is the exact CLI fingerprint captured from OpenCode
// 1.19.4 with Bun 1.4.4 and the AI SDK provider utilities.
const OpenCodeUserAgent = "opencode/1.19.4 ai-sdk/provider-utils/4.0.23 runtime/bun/1.4.4"

const (
	DefaultOpenCodeConcurrency       = 10
	DefaultOpenCodeLargeContextBytes = 256 << 10
	DefaultOpenCodeLargeContextRace  = 2
	DefaultOpenCodeRetries           = 10
	DefaultOpenCodeFirstTokenGrace   = 60 * time.Second
	DefaultOpenCodeCooldown          = 10 * time.Minute
	DefaultOpenCodeMaxTokens         = 32000
	DefaultOpenCodeWARPPoolSize      = 2
	MaxOpenCodeWARPPoolSize          = 16
	DefaultOpenCodeWARPStack         = "ipv4"
)

var defaultOpenCodeModels = []string{
	"beam-free", "big-pickle", "cat-jam-free", "deepseek-v4-flash-free",
	"deepseek-v4.1-flash-free", "exo-free", "fledge-alpha-free", "flint-40b-free",
	"glm-4.7-free", "glm-5-free", "glm-5.3-flash-free", "grok-code", "hy3-free",
	"hy3-preview-free", "kimi-k2.5-free", "laguna-s-2.1-free", "ling-2.6-flash-free",
	"ling-3.0-flash-fin-free", "ling-3.0-flash-free", "ling-3.0-tiny-free",
	"ling-3.1-flash-free", "ll-alpha-free", "longcat-2.0-free",
	"longcat-2.5-preview-free", "mimo-v2-flash-free", "mimo-v2-omni-free",
	"mimo-v2-pro-free", "mimo-v2.5-free", "mimo-v2.6-flash-free",
	"minimax-m2.1-free", "minimax-m2.5-free", "minimax-m3-free",
	"minimax-m3.1-flash-free", "muse-spark-1.2-contributor-free",
	"muse-spark-1.3-contributor-free", "nemotron-3-super-free",
	"nemotron-3-ultra-free", "nemotron-3.5-lightning-free",
	"nemotron-3.5-super-vl-free", "north-mini-code-free", "qwen3.6-plus-free",
	"ring-2.6-1t-free", "space-bunny-free", "step-5-preview-free",
	"trinity-large-preview-free", "x-preview-f-free",
}

type OpenCode struct {
	BaseURL           *url.URL        `yaml:"-"`
	APIKey            string          `yaml:"api_key"`
	AuthKey           string          `yaml:"auth_key"`
	WARP              bool            `yaml:"warp"`
	WARPPoolSize      int             `yaml:"warp_pool_size"`
	WARPStack         string          `yaml:"warp_stack"`
	FreeCompat        bool            `yaml:"free_compat"`
	Models            map[string]bool `yaml:"-"`
	AllowImplicitFree bool            `yaml:"-"`
	Concurrency       int             `yaml:"concurrency"`
	LargeContextBytes int64           `yaml:"large_context_bytes"`
	LargeContextRace  int             `yaml:"large_context_concurrency"`
	Retries           int             `yaml:"retries"`
	WaitTimeout       time.Duration   `yaml:"wait_timeout"`
	FirstTokenTimeout time.Duration   `yaml:"first_token_timeout"`
	StreamIdleTimeout time.Duration   `yaml:"stream_idle_timeout"`
	Cooldown          time.Duration   `yaml:"cooldown"`
	MaxBodyBytes      int64           `yaml:"max_body_bytes"`
	ProxyURLs         []*url.URL      `yaml:"-"`
	Workspace         string          `yaml:"workspace"`
	WorkspaceRoot     string          `yaml:"workspace_root"`
}

type openCodeFile struct {
	Enabled   bool     `yaml:"enabled"`
	BaseURL   string   `yaml:"base_url"`
	Models    []string `yaml:"models"`
	ProxyURLs []string `yaml:"proxy_urls"`
	OpenCode  `yaml:",inline"`
}

func defaultOpenCode() openCodeFile {
	return openCodeFile{
		BaseURL: "https://opencode.ai/inference/openai/v1",
		OpenCode: OpenCode{
			FreeCompat:        true,
			Concurrency:       DefaultOpenCodeConcurrency,
			LargeContextBytes: DefaultOpenCodeLargeContextBytes,
			LargeContextRace:  DefaultOpenCodeLargeContextRace,
			Retries:           DefaultOpenCodeRetries,
			WaitTimeout:       180 * time.Second,
			FirstTokenTimeout: DefaultOpenCodeFirstTokenGrace,
			StreamIdleTimeout: 60 * time.Second,
			Cooldown:          DefaultOpenCodeCooldown,
			MaxBodyBytes:      64 << 20,
			WARPPoolSize:      DefaultOpenCodeWARPPoolSize,
			WARPStack:         DefaultOpenCodeWARPStack,
		},
	}
}

func (settings openCodeFile) build() (*OpenCode, error) {
	if !settings.Enabled {
		return nil, nil
	}
	cfg := settings.OpenCode
	base, err := url.Parse(settings.BaseURL)
	if err != nil || base.Scheme != "https" || base.Hostname() == "" || base.User != nil || base.ForceQuery ||
		strings.Contains(settings.BaseURL, "#") || base.RawQuery != "" {
		return nil, errors.New("opencode.base_url must be an HTTPS base URL without credentials, query or fragment")
	}
	base.Path = strings.TrimRight(base.Path, "/")
	base.RawPath = ""
	cfg.BaseURL = base
	cfg.AuthKey = strings.TrimSpace(cfg.AuthKey)
	models := settings.Models
	if models == nil {
		cfg.AllowImplicitFree = true
		models = defaultOpenCodeModels
	} else if len(models) == 0 {
		return nil, errors.New("opencode.models must contain at least one model ID")
	}
	cfg.Models = make(map[string]bool, len(models))
	for _, model := range models {
		if model == "" || strings.ContainsAny(model, "/\\\r\n\t ") {
			return nil, errors.New("opencode.models must contain bare model IDs")
		}
		cfg.Models[model] = true
	}
	if cfg.Concurrency <= 0 {
		return nil, errors.New("opencode.concurrency must be a positive integer")
	}
	if cfg.LargeContextBytes < 0 {
		return nil, errors.New("opencode.large_context_bytes must be a non-negative integer")
	}
	if cfg.LargeContextRace <= 0 {
		return nil, errors.New("opencode.large_context_concurrency must be a positive integer")
	}
	if cfg.Retries < 0 {
		return nil, errors.New("opencode.retries must be a non-negative integer")
	}
	for _, check := range []struct {
		name  string
		value time.Duration
	}{
		{"wait_timeout", cfg.WaitTimeout},
		{"first_token_timeout", cfg.FirstTokenTimeout},
		{"stream_idle_timeout", cfg.StreamIdleTimeout},
	} {
		if check.value <= 0 {
			return nil, fmt.Errorf("opencode.%s must be a positive duration, e.g. 60s", check.name)
		}
	}
	if cfg.Cooldown < 0 {
		return nil, errors.New("opencode.cooldown must be a non-negative duration")
	}
	if cfg.MaxBodyBytes <= 0 {
		return nil, errors.New("opencode.max_body_bytes must be a positive integer")
	}
	if cfg.WARPPoolSize <= 0 || cfg.WARPPoolSize > MaxOpenCodeWARPPoolSize {
		return nil, fmt.Errorf("opencode.warp_pool_size must be an integer between 1 and %d", MaxOpenCodeWARPPoolSize)
	}
	if cfg.WARPStack, err = normalizeWARPStack(cfg.WARPStack); err != nil {
		return nil, err
	}
	if cfg.ProxyURLs, err = parseProxyURLs(settings.ProxyURLs); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func normalizeWARPStack(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "ipv6", "v6":
		return "ipv6", nil
	case "ipv4", "v4":
		return "ipv4", nil
	case "dual", "both":
		return "dual", nil
	default:
		return "", errors.New("opencode.warp_stack must be ipv6, ipv4 or dual")
	}
}

func parseProxyURLs(parts []string) ([]*url.URL, error) {
	if len(parts) == 0 {
		return nil, nil
	}
	proxies := make([]*url.URL, 0, len(parts))
	for _, part := range parts {
		proxy, err := url.Parse(part)
		if err != nil || proxy.Hostname() == "" || proxy.RawQuery != "" || proxy.ForceQuery || proxy.Fragment != "" || proxy.Opaque != "" {
			return nil, errors.New("opencode.proxy_urls must contain valid proxy URLs without query or fragment")
		}
		proxy.Scheme = strings.ToLower(proxy.Scheme)
		switch proxy.Scheme {
		case "http", "https", "socks5", "socks5h":
		default:
			return nil, errors.New("opencode.proxy_urls supports http, https, socks5 and socks5h URLs")
		}
		if proxy.Path != "" && proxy.Path != "/" {
			return nil, errors.New("opencode.proxy_urls must not contain a path")
		}
		proxy.Path = ""
		proxies = append(proxies, proxy)
	}
	return proxies, nil
}
