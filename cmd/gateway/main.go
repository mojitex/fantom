package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	cliproxyengine "github.com/mojitex/fantom/internal/cliproxy"
	"github.com/mojitex/fantom/internal/config"
	"github.com/mojitex/fantom/internal/server"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, err := config.Load()
	if err != nil {
		logger.Error("configuration error", "error", err)
		os.Exit(1)
	}
	handler := server.New(cfg, logger)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	adminKey := cfg.GatewayAPIKeys[0]
	core, err := cliproxyengine.New(adminKey)
	if err != nil {
		logger.Error("could not initialize embedded Go routing engine", "error", err)
		handler.Close()
		os.Exit(1)
	}
	if err := core.Start(ctx); err != nil {
		logger.Error("could not start embedded Go routing engine", "error", err)
		core.Close()
		handler.Close()
		os.Exit(1)
	}
	handler.AttachCore(core)
	logger.Info("embedded CLIProxyAPI routing engine is ready")

	httpServer := &http.Server{
		Addr: cfg.ListenAddr, Handler: handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 120 * time.Second,
		IdleTimeout: 120 * time.Second,
		MaxHeaderBytes: 1 << 20,
		// WriteTimeout is intentionally disabled: model streams can be long-lived.
	}
	go func() {
		logger.Info("Fantom AI Gateway listening", "address", cfg.ListenAddr, "providers", len(cfg.Providers))
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP server failed", "error", err)
			stop()
		}
	}()
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Warn("graceful shutdown failed", "error", err)
		_ = httpServer.Close()
	}
	handler.Close()
}
