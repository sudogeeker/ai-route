package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

const AppName = "ai-route"
const defaultPath = "ai-route.yaml"

type Config struct {
	ListenAddr  string
	ManualCIDRs []string
	LogLevel    string
	LogFile     string
	Gemini      *Relay
	OpenAI      *Relay
	OpenAIHost  string
	OpenCode    *OpenCode
}

type Relay struct {
	TargetHost     string        `yaml:"target_host"`
	IPFamily       string        `yaml:"ip_family"`
	DestinationIPs []netip.Addr  `yaml:"destination_ips"`
	UpdateInterval time.Duration `yaml:"dns_refresh_interval"`
	DoH            DoH           `yaml:"doh"`
}

type DoH struct {
	URL          string       `yaml:"url"`
	BootstrapIPs []netip.Addr `yaml:"bootstrap_ips"`
}

type relayFile struct {
	Enabled bool `yaml:"enabled"`
	Relay   `yaml:",inline"`
}

type openAIFile struct {
	InboundHost string    `yaml:"inbound_host"`
	Relay       relayFile `yaml:",inline"`
}

type fileConfig struct {
	Listen string `yaml:"listen"`
	Log    struct {
		Level string `yaml:"level"`
		File  string `yaml:"file"`
	} `yaml:"log"`
	IPv6 struct {
		SourceCIDRs []string `yaml:"source_cidrs"`
	} `yaml:"ipv6"`
	Gemini   relayFile    `yaml:"gemini"`
	OpenAI   openAIFile   `yaml:"openai"`
	OpenCode openCodeFile `yaml:"opencode"`
}

func Load(args []string) (Config, error) {
	flags := flag.NewFlagSet(AppName, flag.ContinueOnError)
	path := flags.String("config", defaultPath, "Path to the YAML configuration file")
	if err := flags.Parse(args); err != nil {
		return Config{}, err
	}
	if flags.NArg() != 0 {
		return Config{}, errors.New("unexpected positional arguments; use -config <path>")
	}
	var input io.Reader
	file, err := os.Open(*path)
	if err == nil {
		defer file.Close()
		input = file
	} else if flags.NFlag() != 0 || !errors.Is(err, os.ErrNotExist) {
		return Config{}, fmt.Errorf("open config: %w", err)
	}
	cfg, err := decode(input)
	if err != nil {
		return Config{}, fmt.Errorf("config %q: %w", *path, err)
	}
	return cfg, nil
}

func decode(input io.Reader) (Config, error) {
	settings := fileConfig{
		Listen:   ":8080",
		Gemini:   relayFile{Enabled: true, Relay: defaultRelay("generativelanguage.googleapis.com", "ipv6")},
		OpenAI:   openAIFile{Relay: relayFile{Relay: defaultRelay("api.openai.com", "dual")}},
		OpenCode: defaultOpenCode(),
	}
	settings.Log.Level = "ERROR"
	if input != nil {
		decoder := yaml.NewDecoder(input)
		decoder.KnownFields(true)
		if err := decoder.Decode(&settings); err != nil && !errors.Is(err, io.EOF) {
			return Config{}, err
		}
		var extra yaml.Node
		if err := decoder.Decode(&extra); err == nil {
			return Config{}, errors.New("expected one YAML document")
		} else if !errors.Is(err, io.EOF) {
			return Config{}, err
		}
	}

	_, port, err := net.SplitHostPort(settings.Listen)
	if err != nil || port == "" {
		return Config{}, errors.New("listen must be an address with a port, e.g. :8080")
	}
	level := strings.ToUpper(strings.TrimSpace(settings.Log.Level))
	switch level {
	case "DEBUG", "INFO", "WARN", "ERROR":
	default:
		return Config{}, errors.New("log.level must be DEBUG, INFO, WARN or ERROR")
	}
	cfg := Config{
		ListenAddr:  settings.Listen,
		LogLevel:    level,
		LogFile:     settings.Log.File,
		ManualCIDRs: settings.IPv6.SourceCIDRs,
	}
	if cfg.Gemini, err = settings.Gemini.build("gemini"); err != nil {
		return Config{}, err
	}
	if cfg.OpenAI, err = settings.OpenAI.Relay.build("openai"); err != nil {
		return Config{}, err
	}
	if cfg.OpenAI != nil {
		cfg.OpenAIHost = strings.TrimSuffix(strings.ToLower(settings.OpenAI.InboundHost), ".")
		if settings.OpenAI.InboundHost != "" && !validHostname(cfg.OpenAIHost) {
			return Config{}, errors.New("openai.inbound_host must be a hostname without a scheme, port or path")
		}
	}
	if cfg.OpenCode, err = settings.OpenCode.build(); err != nil {
		return Config{}, err
	}
	if cfg.Gemini == nil && cfg.OpenAI == nil && cfg.OpenCode == nil {
		return Config{}, errors.New("at least one provider must be enabled")
	}
	return cfg, nil
}

func defaultRelay(host, family string) Relay {
	return Relay{TargetHost: host, IPFamily: family, UpdateInterval: 10 * time.Minute}
}

func (settings relayFile) build(name string) (*Relay, error) {
	if !settings.Enabled {
		return nil, nil
	}
	host, err := url.Parse("https://" + settings.TargetHost)
	if err != nil || host.Hostname() == "" || host.Host != settings.TargetHost || host.User != nil ||
		strings.HasSuffix(host.Host, ":") {
		return nil, fmt.Errorf("%s.target_host must be a hostname with an optional port", name)
	}
	if port := host.Port(); port != "" {
		if value, err := strconv.ParseUint(port, 10, 16); err != nil || value == 0 {
			return nil, fmt.Errorf("%s.target_host has an invalid port", name)
		}
	}
	ip, ipErr := netip.ParseAddr(host.Hostname())
	if strings.HasPrefix(host.Host, "[") {
		if ipErr != nil || !ip.Is6() {
			return nil, fmt.Errorf("%s.target_host must use brackets only for IPv6 literals", name)
		}
	} else if strings.Contains(host.Hostname(), ":") {
		return nil, fmt.Errorf("%s.target_host must bracket IPv6 literals", name)
	}
	if settings.UpdateInterval <= 0 {
		return nil, fmt.Errorf("%s.dns_refresh_interval must be a positive duration", name)
	}
	switch settings.IPFamily {
	case "ipv4", "ipv6", "dual":
	default:
		return nil, fmt.Errorf("%s.ip_family must be ipv4, ipv6 or dual", name)
	}
	if ipErr == nil && !settings.Relay.AllowsIP(ip) {
		return nil, fmt.Errorf("%s.target_host must be a unicast address matching ip_family", name)
	}
	for _, ip := range settings.DestinationIPs {
		if !settings.Relay.AllowsIP(ip) {
			return nil, fmt.Errorf("%s.destination_ips must contain unicast addresses matching ip_family", name)
		}
	}
	if err := settings.DoH.validate(); err != nil {
		return nil, fmt.Errorf("%s.doh: %w", name, err)
	}
	return &settings.Relay, nil
}

func (settings DoH) validate() error {
	if settings.URL == "" {
		if len(settings.BootstrapIPs) > 0 {
			return errors.New("url is required with bootstrap_ips")
		}
		return nil
	}
	endpoint, err := url.Parse(settings.URL)
	if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" || endpoint.User != nil ||
		strings.Contains(settings.URL, "#") || strings.HasSuffix(endpoint.Host, ":") {
		return errors.New("url must be an HTTPS URL without credentials or fragment")
	}
	ip, ipErr := netip.ParseAddr(endpoint.Hostname())
	if ipErr == nil {
		if !usableIP(ip) || len(settings.BootstrapIPs) > 0 {
			return errors.New("an IP literal URL must be unicast and must not also set bootstrap_ips")
		}
		if ip.Is6() != strings.HasPrefix(endpoint.Host, "[") {
			return errors.New("url must bracket IPv6 literals only")
		}
	} else if strings.HasPrefix(endpoint.Host, "[") || !validHostname(strings.TrimSuffix(endpoint.Hostname(), ".")) || len(settings.BootstrapIPs) == 0 {
		return errors.New("a hostname URL requires bootstrap_ips so DoH does not depend on system DNS")
	}
	if port := endpoint.Port(); port != "" {
		if value, err := strconv.ParseUint(port, 10, 16); err != nil || value == 0 {
			return errors.New("url has an invalid port")
		}
	}
	for _, ip := range settings.BootstrapIPs {
		if !usableIP(ip) {
			return errors.New("bootstrap_ips must contain unicast addresses")
		}
	}
	return nil
}

func validHostname(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-') {
				return false
			}
		}
	}
	return true
}

func usableIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsValid() && !ip.IsUnspecified() && !ip.IsMulticast()
}

func (cfg Relay) AllowsIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !usableIP(ip) {
		return false
	}
	switch cfg.IPFamily {
	case "ipv4":
		return ip.Is4()
	case "ipv6":
		return ip.Is6()
	default:
		return cfg.IPFamily == "dual"
	}
}
