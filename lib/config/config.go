// Package config loads runtime configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
)

// UserCountry is the ISO 3166-1 alpha-2 country code sent to Odesli when
// resolving links. Hardcoded to US for now; revisit when we need
// per-deployment configuration.
const UserCountry = "US"

// Config holds the runtime configuration.
type Config struct {
	// DiscordToken is the bot token used to authorize the gateway
	// connection. When empty, the Discord bot does not start. Discord
	// requires the bot token for the gateway even when slash commands are
	// registered via OAuth2 client credentials.
	DiscordToken string

	// DiscordClientID is the Discord application client ID. When set, the
	// landing page at / renders a clickable "Add to server" invite link.
	// It is also used as the application ID for slash command
	// registration when paired with DiscordClientSecret.
	DiscordClientID string

	// DiscordClientSecret is the Discord application OAuth2 client
	// secret. When set together with DiscordClientID, linkbot exchanges
	// it for an app-level bearer token at startup and registers a
	// /sanitize global slash command. Empty means slash command
	// registration is skipped; the message-listener path still works.
	DiscordClientSecret string

	// Port is the HTTP API listen port.
	Port int

	// OdesliAPIKey is optional. The public Odesli API works without one but is rate limited.
	OdesliAPIKey string
}

// Load reads configuration from environment variables.
//
// It rejects clearly-broken combinations (e.g. a client secret with no
// client ID) but stays permissive in cases that have a sensible degraded
// mode: an unset DiscordClientSecret simply disables slash command
// registration and main is expected to log a warning.
func Load() (*Config, error) {
	c := &Config{
		DiscordToken:        os.Getenv("DISCORD_TOKEN"),
		DiscordClientID:     os.Getenv("DISCORD_CLIENT_ID"),
		DiscordClientSecret: os.Getenv("DISCORD_CLIENT_SECRET"),
		OdesliAPIKey:        os.Getenv("ODESLI_API_KEY"),
	}

	if c.DiscordClientSecret != "" && c.DiscordClientID == "" {
		return nil, errors.New("DISCORD_CLIENT_SECRET set without DISCORD_CLIENT_ID; both must be set as a pair")
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return nil, fmt.Errorf("invalid PORT %q: %w", port, err)
	}
	if n < 1 || n > 65535 {
		return nil, errors.New("PORT must be between 1 and 65535")
	}
	c.Port = n

	return c, nil
}
