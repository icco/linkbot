// Package config loads runtime configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
)

// Config holds the runtime configuration.
type Config struct {
	// DiscordToken is the bot token. When empty, the Discord bot does not start.
	DiscordToken string

	// Port is the HTTP API listen port.
	Port int

	// OdesliAPIKey is optional. The public Odesli API works without one but is rate limited.
	OdesliAPIKey string

	// UserCountry is an optional ISO 3166-1 alpha-2 country code passed to Odesli.
	UserCountry string
}

// Load reads configuration from environment variables.
func Load() (*Config, error) {
	c := &Config{
		DiscordToken: os.Getenv("DISCORD_TOKEN"),
		OdesliAPIKey: os.Getenv("ODESLI_API_KEY"),
		UserCountry:  os.Getenv("ODESLI_USER_COUNTRY"),
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
