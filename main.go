// Copyright (C) 2026 Nat Welch
//
// This program is free software: you can redistribute it and/or modify it under
// the terms of the GNU General Public License as published by the Free Software
// Foundation, either version 3 of the License, or (at your option) any later
// version. See the LICENSE file, or <https://www.gnu.org/licenses/>.

// Command linkbot runs a Discord bot and HTTP API that sanitize URLs.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.icco.me/gutil/logging"
	"go.icco.me/odesli"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.uber.org/zap"

	"go.icco.me/linkbot/lib/api"
	"go.icco.me/linkbot/lib/config"
	"go.icco.me/linkbot/lib/discord"
	"go.icco.me/linkbot/lib/sanitize"
)

// odesliUserAgent identifies linkbot to Odesli. icco/odesli defaults to naming
// itself, so we override it to keep the identification the vendored copy sent.
const odesliUserAgent = "linkbot/0.1 (+https://github.com/icco/linkbot)"

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

// run wires dependencies and blocks until SIGINT/SIGTERM.
func run() error {
	log, err := logging.NewLogger("linkbot")
	if err != nil {
		fallback, ferr := zap.NewProduction()
		if ferr != nil {
			return fmt.Errorf("logger init: %w / %w", err, ferr)
		}
		fallback.Warn("falling back to zap.NewProduction logger", zap.Error(err))
		log = fallback.Sugar()
	}
	defer func() {
		if err := log.Sync(); err != nil {
			log.Debugw("logger sync", zap.Error(err))
		}
	}()

	registry := prometheus.NewRegistry()
	exporter, err := otelprom.New(otelprom.WithRegisterer(registry))
	if err != nil {
		return fmt.Errorf("otel prometheus exporter: %w", err)
	}
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exporter))
	otel.SetMeterProvider(mp)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := mp.Shutdown(shutdownCtx); err != nil {
			log.Warnw("meter provider shutdown", zap.Error(err))
		}
	}()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	// icco/odesli defaults to a plain transport so it carries no otel
	// dependency of its own; instrumentation is ours to supply.
	odesliClient := odesli.New(
		odesli.WithAPIKey(cfg.OdesliAPIKey),
		odesli.WithUserCountry(config.UserCountry),
		odesli.WithUserAgent(odesliUserAgent),
		odesli.WithHTTPClient(&http.Client{
			Timeout:   15 * time.Second,
			Transport: otelhttp.NewTransport(http.DefaultTransport),
		}),
	)
	san := sanitize.New(odesliClient)

	srv := &http.Server{
		Addr: fmt.Sprintf(":%d", cfg.Port),
		Handler: api.Router(api.Options{
			Sanitizer:       san,
			Logger:          log,
			DiscordClientID: cfg.DiscordClientID,
			MetricsHandler:  promhttp.HandlerFor(registry, promhttp.HandlerOpts{}),
		}),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      35 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx = logging.NewContext(ctx, log)

	if cfg.DiscordToken != "" {
		bot, err := discord.New(cfg.DiscordToken, san)
		if err != nil {
			return fmt.Errorf("discord init: %w", err)
		}
		defer func() {
			if err := bot.Close(); err != nil {
				log.Errorw("discord close", zap.Error(err))
			}
		}()
		if err := bot.Start(ctx); err != nil {
			return fmt.Errorf("discord start: %w", err)
		}

		if cfg.DiscordClientID != "" {
			if err := bot.RegisterCommands(ctx, cfg.DiscordClientID); err != nil {
				log.Warnw("discord slash command registration failed; bot still running", zap.Error(err))
			}
		} else {
			log.Warn("DISCORD_CLIENT_ID not set; skipping slash command registration")
		}
	} else {
		log.Warn("DISCORD_TOKEN not set; running API only")
	}

	log.Infow("http server starting", "addr", srv.Addr)
	return serve(ctx, srv)
}

// serve propagates listener failures and drains active requests on cancellation.
func serve(ctx context.Context, srv *http.Server) error {
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- srv.ListenAndServe()
	}()
	select {
	case err := <-serverErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("http serve: %w", err)
	case <-ctx.Done():
	}
	logging.FromContext(ctx).Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("http shutdown: %w", errors.Join(err, srv.Close()))
	}
	err := <-serverErr
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http serve: %w", err)
	}
	return nil
}
