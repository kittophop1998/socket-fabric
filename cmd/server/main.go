package main

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/joho/godotenv"

	"socketfabric/internal/bootstrap"
	"socketfabric/internal/config"
)

func main() {
	os.Exit(run())
}

func run() int {
	startupLogger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	// A local .env is optional and never overrides variables already set in the environment.
	if err := godotenv.Load(); err != nil && !errors.Is(err, fs.ErrNotExist) {
		startupLogger.Error("invalid .env file", "error", err)
		return 1
	}
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
