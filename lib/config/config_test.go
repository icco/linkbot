package config

import (
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	// All env vars touched by Load() are cleared up front so the tests
	// run in a deterministic environment regardless of where they execute.
	envKeys := []string{
		"DISCORD_TOKEN",
		"DISCORD_CLIENT_ID",
		"DISCORD_CLIENT_SECRET",
		"ODESLI_API_KEY",
		"PORT",
	}

	type want struct {
		err          bool
		errSubstr    string
		token        string
		clientID     string
		clientSecret string
		odesliKey    string
		port         int
	}

	cases := []struct {
		name string
		env  map[string]string
		want want
	}{
		{
			name: "defaults",
			env:  map[string]string{},
			want: want{port: 8080},
		},
		{
			name: "valid client id and secret pair",
			env: map[string]string{
				"DISCORD_CLIENT_ID":     "123",
				"DISCORD_CLIENT_SECRET": "shh",
			},
			want: want{
				clientID:     "123",
				clientSecret: "shh",
				port:         8080,
			},
		},
		{
			name: "secret without id errors",
			env: map[string]string{
				"DISCORD_CLIENT_SECRET": "shh",
			},
			want: want{
				err:       true,
				errSubstr: "DISCORD_CLIENT_SECRET",
			},
		},
		{
			name: "id without secret is permitted",
			env: map[string]string{
				"DISCORD_CLIENT_ID": "123",
			},
			want: want{
				clientID: "123",
				port:     8080,
			},
		},
		{
			name: "custom port",
			env: map[string]string{
				"PORT": "9090",
			},
			want: want{port: 9090},
		},
		{
			name: "invalid port",
			env: map[string]string{
				"PORT": "not-a-number",
			},
			want: want{
				err:       true,
				errSubstr: "invalid PORT",
			},
		},
		{
			name: "out-of-range port",
			env: map[string]string{
				"PORT": "70000",
			},
			want: want{
				err:       true,
				errSubstr: "PORT must be between",
			},
		},
		{
			name: "all fields populated",
			env: map[string]string{
				"DISCORD_TOKEN":         "tok",
				"DISCORD_CLIENT_ID":     "123",
				"DISCORD_CLIENT_SECRET": "shh",
				"ODESLI_API_KEY":        "ok",
				"PORT":                  "8081",
			},
			want: want{
				token:        "tok",
				clientID:     "123",
				clientSecret: "shh",
				odesliKey:    "ok",
				port:         8081,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range envKeys {
				t.Setenv(k, "")
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			cfg, err := Load()
			if tc.want.err {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.want.errSubstr)
				}
				if tc.want.errSubstr != "" && !strings.Contains(err.Error(), tc.want.errSubstr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.want.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.DiscordToken != tc.want.token {
				t.Errorf("DiscordToken = %q, want %q", cfg.DiscordToken, tc.want.token)
			}
			if cfg.DiscordClientID != tc.want.clientID {
				t.Errorf("DiscordClientID = %q, want %q", cfg.DiscordClientID, tc.want.clientID)
			}
			if cfg.DiscordClientSecret != tc.want.clientSecret {
				t.Errorf("DiscordClientSecret mismatch")
			}
			if cfg.OdesliAPIKey != tc.want.odesliKey {
				t.Errorf("OdesliAPIKey = %q, want %q", cfg.OdesliAPIKey, tc.want.odesliKey)
			}
			if cfg.Port != tc.want.port {
				t.Errorf("Port = %d, want %d", cfg.Port, tc.want.port)
			}
		})
	}
}
