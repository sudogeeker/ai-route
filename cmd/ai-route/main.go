package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"aizasy.pages.dev/ai-route/internal/app"
	"aizasy.pages.dev/ai-route/internal/config"
)

func main() {
	cfg, err := config.Load(os.Args[1:])
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err == nil {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		err = app.Run(ctx, cfg)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, config.AppName+":", err)
		os.Exit(1)
	}
}
