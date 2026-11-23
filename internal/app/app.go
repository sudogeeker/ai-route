// Package app assembles providers and owns the application's resource lifecycle.
package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"aizasy.pages.dev/ai-route/internal/config"
	"aizasy.pages.dev/ai-route/internal/ipv6"
	"aizasy.pages.dev/ai-route/internal/logging"
	"aizasy.pages.dev/ai-route/internal/opencode"
	"aizasy.pages.dev/ai-route/internal/relay"
	"aizasy.pages.dev/ai-route/internal/server"
	"aizasy.pages.dev/ai-route/internal/warp"
)

func Run(ctx context.Context, cfg config.Config) error {
	logger, err := logging.Open(cfg.LogLevel, cfg.LogFile)
	if err != nil {
		return fmt.Errorf("open logger: %w", err)
	}
	defer logger.Close()

	var sources *ipv6.SourcePool
	if len(cfg.ManualCIDRs) > 0 || usesIPv6(cfg) {
		sources, err = ipv6.NewSourcePool(cfg.ManualCIDRs)
		if err != nil {
			if len(cfg.ManualCIDRs) != 0 {
				return fmt.Errorf("initialize source IPv6 ranges: %w", err)
			}
			logger.Warnf("No local IPv6 source range; using system-assigned source addresses: %v", err)
		}
	}
	google, stopGoogle := startRelay(ctx, cfg.Gemini, sources, logger)
	defer stopGoogle()
	openai, stopOpenAI := startRelay(ctx, cfg.OpenAI, sources, logger)
	defer stopOpenAI()

	var free server.ChatProvider
	var authKey string
	var maxBodyBytes int64
	if opts := cfg.OpenCode; opts != nil {
		var warpPool *warp.Pool
		if opts.WARP {
			warpPool, err = warp.NewWithStack(ctx, logger, opts.WARPPoolSize, warp.Stack(opts.WARPStack))
			if err != nil {
				return fmt.Errorf("initialize OpenCode WARP egress: %w", err)
			}
			defer warpPool.Close()
			if len(opts.ProxyURLs) != 0 {
				logger.Warnf("opencode.warp takes precedence over opencode.proxy_urls")
			}
		}
		free = opencode.NewWithWARP(*opts, sources, logger, warpPool)
		authKey, maxBodyBytes = opts.AuthKey, opts.MaxBodyBytes
		if authKey == "" {
			logger.Warnf("opencode.auth_key is not set; OpenCode endpoints are unauthenticated")
		}
	}
	router := server.NewRouter(google, openai, free, authKey, maxBodyBytes, cfg.OpenAIHost)
	handler := server.Allowlist(logger.Middleware(router))
	httpServer := &http.Server{
		Addr: cfg.ListenAddr, Handler: handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	finished := make(chan error, 1)
	go func() { finished <- httpServer.ListenAndServe() }()
	fmt.Printf("%s started on %s (Level: %s)\n", config.AppName, cfg.ListenAddr, cfg.LogLevel)
	select {
	case err := <-finished:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			httpServer.Close()
			return err
		}
		return nil
	}
}

func usesIPv6(cfg config.Config) bool {
	for _, upstream := range []*config.Relay{cfg.Gemini, cfg.OpenAI} {
		if upstream != nil && upstream.IPFamily != "ipv4" {
			return true
		}
	}
	opts := cfg.OpenCode
	return opts != nil && !opts.WARP && len(opts.ProxyURLs) == 0
}

func startRelay(parent context.Context, cfg *config.Relay, sources *ipv6.SourcePool, logger *logging.Logger) (http.Handler, func()) {
	if cfg == nil {
		return nil, func() {}
	}
	proxy := relay.New(*cfg, sources, logger)
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		proxy.Run(ctx)
	}()
	return proxy.Handler(), func() {
		cancel()
		<-done
		proxy.Close()
	}
}
