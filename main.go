// Command linkbot runs a Discord bot and HTTP API that sanitize URLs.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/icco/linkbot/lib/api"
	"github.com/icco/linkbot/lib/config"
	"github.com/icco/linkbot/lib/discord"
	"github.com/icco/linkbot/lib/logctx"
	"github.com/icco/linkbot/lib/odesli"
	"github.com/icco/linkbot/lib/sanitize"
)

// main wires the long-lived dependencies (config, logger, Odesli client,
// sanitizer, HTTP server, optional Discord bot) and blocks until SIGINT or
// SIGTERM, after which it shuts both the HTTP server and the Discord
// gateway down with a 10 s grace window.
func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
	slog.SetDefault(log)

	cfg, err := config.Load()
	if err != nil {
		log.Error("config", "error", err)
		os.Exit(1)
	}

	odesliClient := odesli.New(log,
		odesli.WithAPIKey(cfg.OdesliAPIKey),
		odesli.WithUserCountry(config.UserCountry),
	)
	san := sanitize.New(odesliClient, log)

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           api.Router(san, log),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx = logctx.New(ctx, log)

	var bot *discord.Bot
	if cfg.DiscordToken != "" {
		b, err := discord.New(cfg.DiscordToken, san, log)
		if err != nil {
			log.Error("discord init", "error", err)
			os.Exit(1)
		}
		if err := b.Start(ctx); err != nil {
			log.Error("discord start", "error", err)
			os.Exit(1)
		}
		bot = b
	} else {
		log.Warn("DISCORD_TOKEN not set; running API only")
	}

	go func() {
		log.Info("http server starting", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("http shutdown", "error", err)
	}
	if bot != nil {
		if err := bot.Close(); err != nil {
			log.Error("discord close", "error", err)
		}
	}
}
