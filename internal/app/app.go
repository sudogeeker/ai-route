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
)

func Run(ctx context.Context, cfg config.Config) error {
	logger, err := logging.Open(cfg.LogLevel, cfg.LogFile)
	if err != nil {
		return fmt.Errorf("open logger: %w", err)
	}
	defer logger.Close()

	sources, err := ipv6.NewSourcePool(cfg.ManualCIDRs)
	if err != nil {
		return fmt.Errorf("initialize source IPv6 ranges: %w", err)
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

	free := opencode.New(cfg.OpenCode, sources, logger)
	if cfg.OpenCode.AuthKey == "" {
		logger.Warnf("OPENCODE_AUTH_KEY is not set; OpenCode endpoints are unauthenticated")
	}
	router := server.NewRouter(google.Handler(), free, cfg.OpenCode.AuthKey, cfg.OpenCode.MaxBodyBytes)
	// Keep the original path allowlist outside logging and both provider handlers.
	handler := gemini.Allowlist(logger.Middleware(router))
	httpServer := &http.Server{Addr: cfg.ListenAddr, Handler: handler}
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
