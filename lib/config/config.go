// Package config loads runtime configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
)

// UserCountry is the ISO 3166-1 country code sent to Odesli.
const UserCountry = "US"

// Config holds the runtime configuration.
type Config struct {
	// DiscordToken authorizes the gateway; empty disables the bot.
	DiscordToken string

	// DiscordClientID is the Discord application/client ID; powers the
	// invite link and slash command registration.
	DiscordClientID string

	// Port is the HTTP API listen port.
	Port int

	// OdesliAPIKey is optional; raises Odesli rate limits.
	OdesliAPIKey string
}

// Load reads configuration from environment variables.
func Load() (*Config, error) {
	c := &Config{
		DiscordToken:    os.Getenv("DISCORD_TOKEN"),
		DiscordClientID: os.Getenv("DISCORD_CLIENT_ID"),
		OdesliAPIKey:    os.Getenv("ODESLI_API_KEY"),
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
