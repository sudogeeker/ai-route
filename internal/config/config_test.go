package config

import (
	"errors"
	"flag"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLoadYAML(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("LISTEN_ADDR", ":9999")
	t.Setenv("OPENCODE_WARP", "true")
	defaults, err := Load(nil)
	if err != nil || defaults.ListenAddr != ":8080" || defaults.Gemini == nil || defaults.Gemini.IPFamily != "ipv6" ||
		defaults.OpenCode != nil || defaults.OpenAI != nil {
		t.Fatalf("unexpected defaults: %+v, %v", defaults, err)
	}
	if _, err := Load([]string{"-config", defaultPath}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("explicit missing file: %v", err)
	}
	if _, err := Load([]string{"-h", "-config", defaultPath}); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("unexpected help result: %v", err)
	}

	const content = `
listen: "127.0.0.1:8888"
log:
  level: warn
  file: "/var/log/ai-route.log"
ipv6:
  source_cidrs: ["2001:db8:1::/64", "2001:db8:2::/64"]
gemini:
  target_host: google.example
  dns_refresh_interval: 15s
  doh:
    url: https://1.1.1.1/dns-query
openai:
  enabled: true
  inbound_host: OPENAI.gateway.example.
  target_host: openai.example:8443
  destination_ips: ["192.0.2.1", "2001:db8::1"]
  doh:
    url: https://resolver.example/dns-query
    bootstrap_ips: ["192.0.2.2"]
opencode:
  enabled: false
  warp: true
`
	if err := os.WriteFile(defaultPath, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != "127.0.0.1:8888" || cfg.LogLevel != "WARN" || cfg.LogFile != "/var/log/ai-route.log" ||
		!reflect.DeepEqual(cfg.ManualCIDRs, []string{"2001:db8:1::/64", "2001:db8:2::/64"}) {
		t.Fatalf("unexpected server settings: %+v", cfg)
	}
	if cfg.Gemini.TargetHost != "google.example" || cfg.Gemini.IPFamily != "ipv6" || cfg.Gemini.UpdateInterval != 15*time.Second ||
		cfg.Gemini.DoH.URL != "https://1.1.1.1/dns-query" {
		t.Fatalf("unexpected Gemini settings: %+v", cfg.Gemini)
	}
	if cfg.OpenAIHost != "openai.gateway.example" || cfg.OpenAI.TargetHost != "openai.example:8443" ||
		cfg.OpenAI.DoH.URL != "https://resolver.example/dns-query" ||
		!reflect.DeepEqual(cfg.OpenAI.DoH.BootstrapIPs, []netip.Addr{netip.MustParseAddr("192.0.2.2")}) ||
		cfg.OpenAI.IPFamily != "dual" || cfg.OpenAI.UpdateInterval != 10*time.Minute ||
		!reflect.DeepEqual(cfg.OpenAI.DestinationIPs, []netip.Addr{netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("2001:db8::1")}) || cfg.OpenCode != nil {
		t.Fatalf("unexpected OpenAI or OpenCode settings: %+v / %+v", cfg.OpenAI, cfg.OpenCode)
	}

	path := filepath.Join(t.TempDir(), "custom.yaml")
	if err := os.WriteFile(path, []byte("gemini: {enabled: false}\nopenai: {enabled: true}"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load([]string{"-config", path})
	if err != nil || cfg.Gemini != nil || cfg.OpenAI == nil || cfg.OpenAI.TargetHost != "api.openai.com" || cfg.ListenAddr != ":8080" {
		t.Fatalf("explicit file did not replace the default file: %+v, %v", cfg, err)
	}
}

func TestDoHURLWithoutBootstrap(t *testing.T) {
	const settings = `
gemini:
  doh: {url: https://resolver.example/dns-query}
openai:
  enabled: true
  doh: {url: https://other-resolver.example/dns-query, bootstrap_ips: []}
`
	cfg, err := decode(strings.NewReader(settings))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Gemini.DoH.URL != "https://resolver.example/dns-query" || cfg.OpenAI.DoH.URL != "https://other-resolver.example/dns-query" ||
		len(cfg.Gemini.DoH.BootstrapIPs) != 0 || len(cfg.OpenAI.DoH.BootstrapIPs) != 0 {
		t.Fatalf("unexpected resolver configuration: Gemini=%+v OpenAI=%+v", cfg.Gemini.DoH, cfg.OpenAI.DoH)
	}
}

func TestRejectInvalidYAMLSettings(t *testing.T) {
	for _, test := range []struct{ yaml, message string }{
		{"unknown: true", "unknown"},
		{"log: {levl: DEBUG}", "levl"},
		{"openai: {ip_familly: ipv4}", "ip_familly"},
		{"opencode: {authkey: key}", "authkey"},
		{"listen: ':8080'\nlisten: ':8888'", "already defined"},
		{"---\n{}\n---\n{}", "one YAML document"},
		{"listen: localhost", "listen"},
		{"log: {level: DEBUGGING}", "log.level"},
		{"gemini: {enabled: false}", "one provider"},
		{"gemini: {dns_refresh_interval: 0s}", "gemini.dns_refresh_interval"},
		{"gemini: {target_host: 'https://example.com'}", "gemini.target_host"},
		{"gemini: {target_host: 'example.com/path'}", "gemini.target_host"},
		{"gemini: {target_host: 'example.com:99999'}", "gemini.target_host"},
		{"gemini: {target_host: '2001:db8::1:443'}", "gemini.target_host"},
		{"openai: {enabled: true, ip_family: any}", "openai.ip_family"},
		{"openai: {enabled: true, ip_family: ipv6, destination_ips: ['192.0.2.1']}", "openai.destination_ips"},
		{"openai: {enabled: true, destination_ips: ['0.0.0.0']}", "openai.destination_ips"},
		{"openai: {enabled: true, inbound_host: 'https://gateway.example/v1'}", "openai.inbound_host"},
		{"openai: {enabled: true, inbound_host: 'gateway.example:8080'}", "openai.inbound_host"},
		{"gemini: {doh: {url: 'http://1.1.1.1/dns-query'}}", "gemini.doh"},
		{"gemini: {doh: {url: 'https://-resolver.example/dns-query'}}", "hostname"},
		{"gemini: {doh: {url: 'https://resolver.example/dns-query', bootstrap_ips: ['0.0.0.0']}}", "bootstrap_ips"},
		{"gemini: {doh: {url: 'https://1.1.1.1/dns-query', bootstrap_ips: ['1.1.1.1']}}", "bootstrap_ips"},
		{"gemini: {doh: {bootstrap_ips: ['1.1.1.1']}}", "url is required"},
	} {
		t.Run(test.yaml, func(t *testing.T) {
			if _, err := decode(strings.NewReader(test.yaml)); err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("want %q in validation error, got %v", test.message, err)
			}
		})
	}
}
