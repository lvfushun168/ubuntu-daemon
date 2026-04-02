package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"openclaw/dameon/internal/app"
	"openclaw/dameon/internal/config"
)

func main() {
	cfgPath := config.ConfigPathFromEnv()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatalf("load config failed: %v", err)
	}

	logger := log.New(os.Stdout, "[openclaw-daemon] ", log.LstdFlags|log.Lmicroseconds)

	daemonApp, err := app.New(cfg, logger)
	if err != nil {
		logger.Fatalf("bootstrap daemon failed: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := daemonApp.Run(ctx); err != nil {
		logger.Fatalf("run daemon failed: %v", err)
	}
}
