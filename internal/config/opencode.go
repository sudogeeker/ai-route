package config

import (
	"errors"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// OpenCodeVersion identifies the client protocol used by the direct adapter.
// Matching this version does not grant access to the upstream free tier.
const OpenCodeVersion = "1.19.4"

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

func loadOpenCode() (OpenCode, error) {
	authKey := os.Getenv("OPENCODE_AUTH_KEY")
	if authKey == "" {
		// Accept the lower-case spelling used by some Compose deployments.
		authKey = os.Getenv("opencode_auth_key")
	}
	cfg := OpenCode{
		APIKey:            os.Getenv("OPENCODE_API_KEY"),
		AuthKey:           strings.TrimSpace(authKey),
		FreeCompat:        true,
		Models:            make(map[string]bool),
		AllowImplicitFree: true,
		Concurrency:       DefaultOpenCodeConcurrency,
		LargeContextBytes: DefaultOpenCodeLargeContextBytes,
		LargeContextRace:  DefaultOpenCodeLargeContextRace,
		Retries:           DefaultOpenCodeRetries,
		WaitTimeout:       180 * time.Second,
		FirstTokenTimeout: DefaultOpenCodeFirstTokenGrace,
		StreamIdleTimeout: 60 * time.Second,
		Cooldown:          DefaultOpenCodeCooldown,
		MaxBodyBytes:      64 << 20,
	}
	base := os.Getenv("OPENCODE_BASE_URL")
	if base == "" {
		base = "https://opencode.ai/inference/openai/v1"
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return cfg, errors.New("OPENCODE_BASE_URL must be an HTTPS base URL without credentials, query or fragment")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	cfg.BaseURL = u
	if raw := os.Getenv("OPENCODE_FREE_COMPAT"); raw != "" {
		cfg.FreeCompat, err = strconv.ParseBool(raw)
		if err != nil {
			return cfg, errors.New("OPENCODE_FREE_COMPAT must be a boolean")
		}
	}
	models := defaultOpenCodeModels
	if raw, exists := os.LookupEnv("OPENCODE_MODELS"); exists {
		cfg.AllowImplicitFree = false
		models = strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' })
		if len(models) == 0 {
			return cfg, errors.New("OPENCODE_MODELS must contain at least one model ID")
		}
	}
	for _, model := range models {
		// Explicit configuration is the model allowlist; never infer a paid fallback.
		if strings.ContainsAny(model, "/\\\r\n\t ") {
			return cfg, errors.New("OPENCODE_MODELS must contain bare model IDs")
		}
		cfg.Models[model] = true
	}
	concurrency := os.Getenv("OPENCODE_CONCURRENCY")
	if concurrency == "" {
		concurrency = os.Getenv("opencode_concurrency")
	}
	if concurrency != "" {
		cfg.Concurrency, err = strconv.Atoi(strings.TrimSpace(concurrency))
		if err != nil || cfg.Concurrency <= 0 {
			return cfg, errors.New("OPENCODE_CONCURRENCY must be a positive integer")
		}
	}
	if raw := os.Getenv("OPENCODE_LARGE_CONTEXT_BYTES"); raw != "" {
		cfg.LargeContextBytes, err = strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil || cfg.LargeContextBytes < 0 {
			return cfg, errors.New("OPENCODE_LARGE_CONTEXT_BYTES must be a non-negative integer")
		}
	}
	if raw := os.Getenv("OPENCODE_LARGE_CONTEXT_CONCURRENCY"); raw != "" {
		cfg.LargeContextRace, err = strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || cfg.LargeContextRace <= 0 {
			return cfg, errors.New("OPENCODE_LARGE_CONTEXT_CONCURRENCY must be a positive integer")
		}
	}
	if raw := os.Getenv("OPENCODE_RETRIES"); raw != "" {
		cfg.Retries, err = strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || cfg.Retries < 0 {
			return cfg, errors.New("OPENCODE_RETRIES must be a non-negative integer")
		}
	}
	if raw := os.Getenv("OPENCODE_WAIT_TIMEOUT"); raw != "" {
		cfg.WaitTimeout, err = time.ParseDuration(raw)
		if err != nil || cfg.WaitTimeout <= 0 {
			return cfg, errors.New("OPENCODE_WAIT_TIMEOUT must be a positive duration, e.g. 180s")
		}
	}
	if raw := os.Getenv("OPENCODE_FIRST_TOKEN_TIMEOUT"); raw != "" {
		cfg.FirstTokenTimeout, err = time.ParseDuration(raw)
		if err != nil || cfg.FirstTokenTimeout <= 0 {
			return cfg, errors.New("OPENCODE_FIRST_TOKEN_TIMEOUT must be a positive duration, e.g. 60s")
		}
	}
	if raw := os.Getenv("OPENCODE_STREAM_IDLE_TIMEOUT"); raw != "" {
		cfg.StreamIdleTimeout, err = time.ParseDuration(raw)
		if err != nil || cfg.StreamIdleTimeout <= 0 {
			return cfg, errors.New("OPENCODE_STREAM_IDLE_TIMEOUT must be a positive duration, e.g. 60s")
		}
	}
	if raw := os.Getenv("OPENCODE_COOLDOWN"); raw != "" {
		cfg.Cooldown, err = time.ParseDuration(raw)
		if err != nil || cfg.Cooldown < 0 {
			return cfg, errors.New("OPENCODE_COOLDOWN must be a non-negative duration, e.g. 10m")
		}
	}
	if raw := os.Getenv("OPENCODE_MAX_BODY_BYTES"); raw != "" {
		cfg.MaxBodyBytes, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || cfg.MaxBodyBytes <= 0 {
			return cfg, errors.New("OPENCODE_MAX_BODY_BYTES must be a positive integer")
		}
	}
	proxy := os.Getenv("OPENCODE_PROXY_URLS")
	if proxy == "" {
		proxy = os.Getenv("OPENCODE_PROXY_URL")
	}
	if proxy != "" {
		cfg.ProxyURLs, err = parseProxyURLs(proxy)
		if err != nil {
			return cfg, err
		}
	}
	return cfg, nil
}

func parseProxyURLs(raw string) ([]*url.URL, error) {
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n' || r == '\t' || r == '\r'
	})
	if len(parts) == 0 {
		return nil, errors.New("OPENCODE_PROXY_URLS must contain at least one proxy URL")
	}
	proxies := make([]*url.URL, 0, len(parts))
	for _, part := range parts {
		proxy, err := url.Parse(part)
		if err != nil || proxy.Hostname() == "" || proxy.RawQuery != "" || proxy.ForceQuery || proxy.Fragment != "" || proxy.Opaque != "" {
			return nil, errors.New("OPENCODE_PROXY_URLS must contain valid proxy URLs without query or fragment")
		}
		proxy.Scheme = strings.ToLower(proxy.Scheme)
		switch proxy.Scheme {
		case "http", "https", "socks5", "socks5h":
		default:
			return nil, errors.New("OPENCODE_PROXY_URLS supports http, https, socks5 and socks5h URLs")
		}
		if proxy.Path != "" && proxy.Path != "/" {
			return nil, errors.New("OPENCODE_PROXY_URLS must not contain a path")
		}
		proxy.Path = ""
		proxies = append(proxies, proxy)
	}
	return proxies, nil
}
