// Command linkbot runs a Discord bot and HTTP API that sanitize URLs.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/icco/gutil/logging"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.uber.org/zap"

	"github.com/icco/linkbot/lib/api"
	"github.com/icco/linkbot/lib/config"
	"github.com/icco/linkbot/lib/discord"
	"github.com/icco/linkbot/lib/odesli"
	"github.com/icco/linkbot/lib/sanitize"
)

// main wires dependencies and blocks until SIGINT/SIGTERM, then
// shuts everything down within a 10 s grace window.
func main() {
	log, err := logging.NewLogger("linkbot")
	if err != nil {
		fallback, ferr := zap.NewProduction()
		if ferr != nil {
			fmt.Fprintf(os.Stderr, "logger init: %v / %v\n", err, ferr)
			os.Exit(1)
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
		log.Errorw("otel prometheus exporter", zap.Error(err))
		os.Exit(1)
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
		log.Errorw("config", zap.Error(err))
		os.Exit(1)
	}

	odesliClient := odesli.New(
		odesli.WithAPIKey(cfg.OdesliAPIKey),
		odesli.WithUserCountry(config.UserCountry),
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
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx = logging.NewContext(ctx, log)

	var bot *discord.Bot
	if cfg.DiscordToken != "" {
		b, err := discord.New(cfg.DiscordToken, san, log)
		if err != nil {
			log.Errorw("discord init", zap.Error(err))
			os.Exit(1)
		}
		if err := b.Start(ctx); err != nil {
			log.Errorw("discord start", zap.Error(err))
			os.Exit(1)
		}
		bot = b
	} else {
		log.Warn("DISCORD_TOKEN not set; running API only")
	}

	go func() {
		log.Infow("http server starting", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Errorw("http server", zap.Error(err))
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Errorw("http shutdown", zap.Error(err))
	}
	if bot != nil {
		if err := bot.Close(); err != nil {
			log.Errorw("discord close", zap.Error(err))
		}
	}
}
