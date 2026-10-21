package config

import (
	"flag"
	"net/url"
	"os"
	"strings"
	"time"
)

const AppName = "ai-route"

type Config struct {
	ListenAddr  string
	ManualCIDRs []string
	LogLevel    string
	LogFile     string
	Gemini      Gemini
	OpenCode    OpenCode
}

type Gemini struct {
	TargetHost     string
	UpdateInterval time.Duration
}

type OpenCode struct {
	BaseURL           *url.URL
	APIKey            string
	AuthKey           string
	FreeCompat        bool
	Models            map[string]bool
	Concurrency       int
	WaitTimeout       time.Duration
	StreamIdleTimeout time.Duration
	MaxBodyBytes      int64
}

// Load preserves environment defaults followed by explicit command-line overrides.
func Load(args []string) (Config, error) {
	cfg := Config{
		ListenAddr: ":8080",
		LogLevel:   "ERROR",
		Gemini:     Gemini{TargetHost: "generativelanguage.googleapis.com", UpdateInterval: 10 * time.Minute},
	}
	if v := os.Getenv("TARGET_HOST"); v != "" {
		cfg.Gemini.TargetHost = v
	}
	if v := os.Getenv("LISTEN_ADDR"); v != "" {
		cfg.ListenAddr = v
	}
	if v := os.Getenv("IPV6_CIDR"); v != "" {
		cfg.ManualCIDRs = splitCIDRs(v)
	}
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := os.Getenv("LOG_FILE"); v != "" {
		cfg.LogFile = v
	}
	flags := flag.NewFlagSet(AppName, flag.ContinueOnError)
	flags.StringVar(&cfg.ListenAddr, "listen", cfg.ListenAddr, "Address to listen on")
	var manual []string
	flags.Func("cidr", "Manual IPv6 CIDR(s), repeatable or comma-separated", func(value string) error { manual = append(manual, splitCIDRs(value)...); return nil })
	flags.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "Log level: DEBUG, INFO, WARN, ERROR")
	flags.StringVar(&cfg.LogFile, "log-file", cfg.LogFile, "Path to log file")
	if err := flags.Parse(args); err != nil {
		return cfg, err
	}
	if len(manual) > 0 {
		cfg.ManualCIDRs = manual
	}
	var err error
	cfg.OpenCode, err = loadOpenCode()
	return cfg, err
}

func splitCIDRs(value string) []string {
	return strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r' })
}
