package config

import (
	"errors"
	"flag"
	"reflect"
	"testing"
)

func TestEnvironmentAndFlagPrecedence(t *testing.T) {
	t.Setenv("OPENCODE_MODELS", "big-pickle")
	t.Setenv("OPENCODE_BASE_URL", "")
	t.Setenv("OPENCODE_AUTH_KEY", "")
	t.Setenv("opencode_auth_key", "")
	t.Setenv("OPENCODE_WAIT_TIMEOUT", "")
	t.Setenv("OPENCODE_MAX_BODY_BYTES", "")
	t.Setenv("LISTEN_ADDR", ":9999")
	t.Setenv("IPV6_CIDR", "2001:db8::/48")
	t.Setenv("TARGET_HOST", "google.example")
	t.Setenv("LOG_LEVEL", "INFO")
	cfg, err := Load([]string{"-listen", ":8888", "-cidr", "2001:db8:1::/64,2001:db8:2::/64", "-cidr", "2001:db8:3::/64", "-log-level", "WARN"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != ":8888" || cfg.LogLevel != "WARN" || cfg.Gemini.TargetHost != "google.example" {
		t.Fatalf("wrong configuration: %+v", cfg)
	}
	want := []string{"2001:db8:1::/64", "2001:db8:2::/64", "2001:db8:3::/64"}
	if !reflect.DeepEqual(cfg.ManualCIDRs, want) {
		t.Fatalf("CIDR flags did not replace the environment: %v", cfg.ManualCIDRs)
	}
	// Independent loads cannot retain flags or state from a previous application.
	again, err := Load(nil)
	if err != nil || again.ListenAddr != ":9999" || len(again.ManualCIDRs) != 1 || again.LogLevel != "INFO" {
		t.Fatalf("configuration leaked between loads: %+v %v", again, err)
	}
}

func TestHelpDoesNotRequireNetworkOrProviderConfiguration(t *testing.T) {
	t.Setenv("OPENCODE_MODELS", "")
	if _, err := Load([]string{"-h"}); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("unexpected help result: %v", err)
	}
}
