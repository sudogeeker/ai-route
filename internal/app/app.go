// Package app assembles providers and owns the application's resource lifecycle.
package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"aizasy.pages.dev/ai-route/internal/config"
	"aizasy.pages.dev/ai-route/internal/gemini"
	"aizasy.pages.dev/ai-route/internal/ipv6"
	"aizasy.pages.dev/ai-route/internal/logging"
	"aizasy.pages.dev/ai-route/internal/opencode"
	"aizasy.pages.dev/ai-route/internal/server"
	"aizasy.pages.dev/ai-route/internal/warp"
)

func Run(ctx context.Context, cfg config.Config) error {
	logger, err := logging.Open(cfg.LogLevel, cfg.LogFile)
	if err != nil {
		return fmt.Errorf("open logger: %w", err)
	}
	defer logger.Close()

	sources, err := ipv6.NewSourcePool(cfg.ManualCIDRs)
	if err != nil {
		if len(cfg.ManualCIDRs) != 0 {
			return fmt.Errorf("initialize source IPv6 ranges: %w", err)
		}
		// OpenCode can use a proxy pool or normal IPv4 connectivity. Gemini
		// remains IPv6-only and will fail until a usable source is available.
		logger.Warnf("No local IPv6 source range: %v", err)
		sources = nil
	}
	google := gemini.New(cfg.Gemini, sources, logger)
	defer google.Close()
	if err := google.Refresh(ctx); err != nil {
		logger.Warnf("Initial AAAA resolve failed: %v", err)
	}
	updaterCtx, stopUpdater := context.WithCancel(ctx)
	updaterDone := make(chan struct{})
	go func() { defer close(updaterDone); google.Run(updaterCtx) }()
	defer func() { stopUpdater(); <-updaterDone }()

	var warpPool *warp.Pool
	if cfg.OpenCode.WARP {
		stack, parseErr := warp.ParseStack(cfg.OpenCode.WARPStack)
		if parseErr != nil {
			return fmt.Errorf("parse OpenCode WARP stack: %w", parseErr)
		}
		warpPool, err = warp.NewWithStack(ctx, logger, cfg.OpenCode.WARPPoolSize, stack)
		if err != nil {
			return fmt.Errorf("initialize OpenCode WARP egress: %w", err)
		}
		defer warpPool.Close()
		if len(cfg.OpenCode.ProxyURLs) != 0 {
			logger.Warnf("OPENCODE_WARP takes precedence over OPENCODE_PROXY_URLS")
		}
	}
	free := opencode.NewWithWARP(cfg.OpenCode, sources, logger, warpPool)
	if cfg.OpenCode.AuthKey == "" {
		logger.Warnf("OPENCODE_AUTH_KEY is not set; OpenCode endpoints are unauthenticated")
	}
	router := server.NewRouter(google.Handler(), free, cfg.OpenCode.AuthKey, cfg.OpenCode.MaxBodyBytes)
	// Keep the original path allowlist outside logging and both provider handlers.
	handler := gemini.Allowlist(logger.Middleware(router))
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
