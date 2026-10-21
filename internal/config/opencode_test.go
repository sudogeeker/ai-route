package config

import (
	"os"
	"testing"
	"time"
)

func TestOpenCodeConfig(t *testing.T) {
	for _, key := range []string{"OPENCODE_BASE_URL", "OPENCODE_API_KEY", "OPENCODE_AUTH_KEY", "opencode_auth_key", "OPENCODE_FREE_COMPAT", "OPENCODE_MODELS", "OPENCODE_WAIT_TIMEOUT", "OPENCODE_STREAM_IDLE_TIMEOUT", "OPENCODE_MAX_BODY_BYTES", "OPENCODE_CONCURRENCY", "opencode_concurrency"} {
		t.Setenv(key, "")
	}
	os.Unsetenv("OPENCODE_MODELS")
	defaults, err := loadOpenCode()
	if err != nil || len(defaults.Models) != 9 || defaults.Models["jev-1.13-free"] || !defaults.FreeCompat {
		t.Fatalf("bad default model list: %+v %v", defaults.Models, err)
	}
	// An explicitly empty allowlist is invalid; use a small operator-supplied list here.
	t.Setenv("OPENCODE_MODELS", "big-pickle,mimo-v2.6-flash-free")
	cfg, err := loadOpenCode()
	if err != nil || len(cfg.Models) != 2 || cfg.Concurrency != DefaultOpenCodeConcurrency || cfg.WaitTimeout != 120*time.Second || cfg.StreamIdleTimeout != 60*time.Second || cfg.MaxBodyBytes != 64<<20 {
		t.Fatalf("bad config: %+v %v", cfg, err)
	}
	for _, setting := range []struct{ key, value string }{
		{"OPENCODE_BASE_URL", "http://example.com"}, {"OPENCODE_BASE_URL", "https://key@example.com"}, {"OPENCODE_BASE_URL", "https://example.com?key=secret"},
		{"OPENCODE_WAIT_TIMEOUT", "0s"}, {"OPENCODE_MAX_BODY_BYTES", "-1"}, {"OPENCODE_MODELS", ""}, {"OPENCODE_MODELS", "opencode/big-pickle"},
		{"OPENCODE_STREAM_IDLE_TIMEOUT", "0s"}, {"OPENCODE_STREAM_IDLE_TIMEOUT", "invalid"},
		{"OPENCODE_FREE_COMPAT", "invalid"},
	} {
		t.Run(setting.key+setting.value, func(t *testing.T) {
			t.Setenv(setting.key, setting.value)
			if _, err := loadOpenCode(); err == nil {
				t.Fatal("invalid setting accepted")
			}
		})
	}
	t.Setenv("OPENCODE_STREAM_IDLE_TIMEOUT", "15s")
	if cfg, err := loadOpenCode(); err != nil || cfg.StreamIdleTimeout != 15*time.Second {
		t.Fatalf("cannot configure stream idle timeout: %+v %v", cfg, err)
	}
	t.Setenv("OPENCODE_FREE_COMPAT", "false")
	if cfg, err := loadOpenCode(); err != nil || cfg.FreeCompat {
		t.Fatalf("cannot disable compatibility: %+v %v", cfg, err)
	}
}

func TestOpenCodeAuthKey(t *testing.T) {
	t.Setenv("OPENCODE_AUTH_KEY", "")
	t.Setenv("opencode_auth_key", "")
	cfg, err := loadOpenCode()
	if err != nil || cfg.AuthKey != "" {
		t.Fatalf("unexpected empty auth key: %+v %v", cfg, err)
	}
	t.Setenv("opencode_auth_key", " lower-case ")
	cfg, err = loadOpenCode()
	if err != nil || cfg.AuthKey != "lower-case" {
		t.Fatalf("lower-case auth key was not loaded: %+v %v", cfg, err)
	}
	t.Setenv("OPENCODE_AUTH_KEY", " upper-case ")
	cfg, err = loadOpenCode()
	if err != nil || cfg.AuthKey != "upper-case" {
		t.Fatalf("upper-case auth key did not win: %+v %v", cfg, err)
	}
}

func TestOpenCodeConcurrency(t *testing.T) {
	for _, item := range []struct {
		name, upper, lower string
		want               int
		invalid            bool
	}{
		{name: "default", want: 10},
		{name: "single", upper: "1", want: 1},
		{name: "larger", upper: "32", want: 32},
		{name: "no arbitrary cap", upper: "100000", want: 100000},
		{name: "lowercase", lower: "7", want: 7},
		{name: "uppercase wins", upper: "20", lower: "7", want: 20},
		{name: "whitespace", upper: " 16 ", want: 16},
		{name: "zero", upper: "0", invalid: true},
		{name: "negative", lower: "-2", invalid: true},
		{name: "fraction", upper: "2.5", invalid: true},
		{name: "text", upper: "auto", invalid: true},
		{name: "overflow", upper: "9999999999999999999999999", invalid: true},
		{name: "invalid uppercase does not fall back", upper: "bad", lower: "7", invalid: true},
	} {
		t.Run(item.name, func(t *testing.T) {
			t.Setenv("OPENCODE_CONCURRENCY", "")
			t.Setenv("opencode_concurrency", item.lower)
			if item.upper != "" {
				t.Setenv("OPENCODE_CONCURRENCY", item.upper)
			}
			cfg, err := loadOpenCode()
			if item.invalid {
				if err == nil {
					t.Fatal("invalid concurrency accepted")
				}
				return
			}
			if err != nil || cfg.Concurrency != item.want {
				t.Fatalf("concurrency=%d, want=%d, error=%v", cfg.Concurrency, item.want, err)
			}
		})
	}
}
