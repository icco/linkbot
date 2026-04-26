// Package discordoauth is a tiny OAuth2 client for Discord's
// client-credentials grant. It is used for app-level REST calls such as
// global slash command registration where Discord requires a bearer token
// instead of the usual `Bot <token>` header.
//
// The client-credentials grant returns a short-lived application access
// token tied to the application's client_id/client_secret pair. It cannot
// authorize the gateway (Discord still requires the bot token there) but
// it is the supported path for app-owned REST operations.
//
// See https://discord.com/developers/docs/topics/oauth2#client-credentials-grant.
package discordoauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// DefaultBaseURL is the Discord REST API root used for token exchange.
const DefaultBaseURL = "https://discord.com/api/v10"

// DefaultScope is the OAuth2 scope requested by default. It is the minimum
// scope required to PUT/POST application command definitions.
const DefaultScope = "applications.commands.update"

// userAgent is the User-Agent header sent on every outbound request.
const userAgent = "linkbot/0.1 (+https://github.com/icco/linkbot)"

// refreshLeeway is the slack we keep before a token's recorded expiry. We
// proactively refresh while ≤ refreshLeeway of validity remains so an
// in-flight request never races a hard expiry.
const refreshLeeway = 30 * time.Second

// errorBodyLimit caps how many bytes of an error response body we will
// surface in the wrapped error. Keeps logs sane if Discord ever returns a
// large HTML error page.
const errorBodyLimit = 512

// Client wraps the Discord client-credentials OAuth2 flow.
//
// A single Client is safe for concurrent use; the access token is cached
// under a mutex and refreshed lazily when it nears expiry.
type Client struct {
	clientID     string
	clientSecret string
	scopes       []string
	http         *http.Client
	log          *slog.Logger
	baseURL      string

	mu    sync.Mutex
	token string
	exp   time.Time
}

// Option configures a Client at construction time.
type Option func(*Client)

// WithHTTPClient overrides the HTTP client used for token exchange.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		c.http = h
	}
}

// WithBaseURL overrides the Discord API base URL (default DefaultBaseURL).
// Mostly useful in tests that want to point at an httptest.Server.
func WithBaseURL(u string) Option {
	return func(c *Client) {
		c.baseURL = u
	}
}

// WithScopes overrides the OAuth2 scopes requested (default DefaultScope).
func WithScopes(scopes []string) Option {
	return func(c *Client) {
		c.scopes = scopes
	}
}

// New constructs a Client. clientID and clientSecret are required; passing
// empty strings is allowed but Token will fail at the first call.
func New(clientID, clientSecret string, log *slog.Logger, opts ...Option) *Client {
	c := &Client{
		clientID:     clientID,
		clientSecret: clientSecret,
		scopes:       []string{DefaultScope},
		http:         &http.Client{Timeout: 15 * time.Second},
		log:          log,
		baseURL:      DefaultBaseURL,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// tokenResponse is the subset of Discord's /oauth2/token response that we
// consume. Discord also returns refresh_token and other fields for some
// grants; client_credentials does not, so we ignore them.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
	Scope       string `json:"scope"`
}

// Token returns a non-expired bearer access token, refreshing it via the
// client-credentials grant if the cached token is missing or near expiry.
//
// Concurrency-safe via an internal mutex. The mutex is held across the
// network round-trip on a refresh so callers that arrive together share a
// single exchange instead of stampeding the token endpoint.
func (c *Client) Token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.token != "" && time.Now().Add(refreshLeeway).Before(c.exp) {
		return c.token, nil
	}

	tok, exp, err := c.fetch(ctx)
	if err != nil {
		return "", err
	}
	c.token = tok
	c.exp = exp
	return c.token, nil
}

// fetch performs a single client-credentials exchange. It is only called
// from Token while c.mu is held, so it does not touch c.token / c.exp
// directly.
func (c *Client) fetch(ctx context.Context) (string, time.Time, error) {
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("scope", strings.Join(c.scopes, " "))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("build oauth request: %w", err)
	}
	req.SetBasicAuth(c.clientID, c.clientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("discord oauth request: %w", err)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			c.log.Warn("close discord oauth response body", "error", cerr)
		}
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("read oauth body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", time.Time{}, fmt.Errorf("discord oauth %d: %s",
			resp.StatusCode, truncate(string(body), errorBodyLimit))
	}

	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", time.Time{}, fmt.Errorf("decode oauth body: %w", err)
	}
	if tr.AccessToken == "" {
		return "", time.Time{}, fmt.Errorf("discord oauth: empty access_token")
	}

	exp := time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	c.log.Info("discord oauth ready",
		"expires_in", tr.ExpiresIn,
		"scope", tr.Scope,
		"token_type", tr.TokenType,
	)
	return tr.AccessToken, exp, nil
}

// truncate clips s to at most n bytes, appending an ellipsis when
// truncation occurs. Used to keep error bodies bounded.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
