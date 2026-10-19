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
const OpenCodeVersion = "2.0.29"

// OpenCodeUserAgent is the 2.x CLI fingerprint expected by the inference gate.
const OpenCodeUserAgent = "opencode/latest/" + OpenCodeVersion + "/cli"

var defaultOpenCodeModels = []string{
	"big-pickle", "beam-free", "mimo-v2.6-flash-free",
	"nemotron-3.5-super-vl-free", "nemotron-3.5-lightning-free",
	"longcat-2.5-preview-free", "deepseek-v4.1-flash-free",
	"glm-5.3-flash-free", "space-bunny-free",
}

func loadOpenCode() (OpenCode, error) {
	authKey := os.Getenv("OPENCODE_AUTH_KEY")
	if authKey == "" {
		// Accept the lower-case spelling used by some Compose deployments.
		authKey = os.Getenv("opencode_auth_key")
	}
	cfg := OpenCode{
		APIKey:       os.Getenv("OPENCODE_API_KEY"),
		AuthKey:      strings.TrimSpace(authKey),
		FreeCompat:   true,
		Models:       make(map[string]bool),
		WaitTimeout:  120 * time.Second,
		MaxBodyBytes: 64 << 20,
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
	if raw := os.Getenv("OPENCODE_WAIT_TIMEOUT"); raw != "" {
		cfg.WaitTimeout, err = time.ParseDuration(raw)
		if err != nil || cfg.WaitTimeout <= 0 {
			return cfg, errors.New("OPENCODE_WAIT_TIMEOUT must be a positive duration, e.g. 120s")
		}
	}
	if raw := os.Getenv("OPENCODE_MAX_BODY_BYTES"); raw != "" {
		cfg.MaxBodyBytes, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || cfg.MaxBodyBytes <= 0 {
			return cfg, errors.New("OPENCODE_MAX_BODY_BYTES must be a positive integer")
		}
	}
	return cfg, nil
}
