package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"socketfabric/internal/bootstrap"
	"socketfabric/internal/config"
)

func main() {
	os.Exit(run())
}

func run() int {
	startupLogger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := config.Load()
	if err != nil {
		startupLogger.Error("invalid server configuration", "error", err)
		return 1
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	server, err := bootstrap.NewServer(cfg, logger)
	if err != nil {
		logger.Error("initialize HTTP server", "error", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := server.Run(ctx); err != nil {
		logger.Error("HTTP server failed", "error", err)
		return 1
	}
	return 0
}
