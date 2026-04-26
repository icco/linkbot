// Package odesli is a small client for the public Odesli (song.link) API.
//
// See https://www.notion.so/odesli/Public-API-d8093b1bb35f4f91a85c0e337c1ff8d5.
package odesli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"
)

// DefaultBaseURL is the public Odesli endpoint.
const DefaultBaseURL = "https://api.song.link/v1-alpha.1/links"

// Client talks to the Odesli API.
type Client struct {
	http    *http.Client
	baseURL string
	apiKey  string
	country string
	log     *slog.Logger
}

// Option configures a Client.
type Option func(*Client)

// WithAPIKey sets an Odesli API key (optional; raises rate limits).
func WithAPIKey(k string) Option {
	return func(c *Client) {
		c.apiKey = k
	}
}

// WithUserCountry sets the ISO 3166-1 alpha-2 country code sent to Odesli.
func WithUserCountry(cc string) Option {
	return func(c *Client) {
		c.country = cc
	}
}

// WithBaseURL overrides the API base URL (mostly for tests).
func WithBaseURL(u string) Option {
	return func(c *Client) {
		c.baseURL = u
	}
}

// WithHTTPClient overrides the HTTP client.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		c.http = h
	}
}

// New returns a Client with sensible defaults.
func New(log *slog.Logger, opts ...Option) *Client {
	c := &Client{
		http:    &http.Client{Timeout: 15 * time.Second},
		baseURL: DefaultBaseURL,
		log:     log,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Response is the subset of the Odesli /links response we care about.
type Response struct {
	EntityUniqueID string `json:"entityUniqueId"`
	UserCountry    string `json:"userCountry"`
	PageURL        string `json:"pageUrl"`
}

// Resolve returns the canonical song.link page URL for a streaming-service link.
func (c *Client) Resolve(ctx context.Context, link string) (*Response, error) {
	q := url.Values{}
	q.Set("url", link)
	if c.country != "" {
		q.Set("userCountry", c.country)
	}
	if c.apiKey != "" {
		q.Set("key", c.apiKey)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "linkbot/0.1 (+https://github.com/icco/linkbot)")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("odesli request: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			c.log.Warn("close odesli response body", "error", err)
		}
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("odesli %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	var out Response
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode body: %w", err)
	}
	if out.PageURL == "" {
		return nil, fmt.Errorf("odesli returned no pageUrl for %s", link)
	}
	return &out, nil
}

// truncate returns s clipped to at most n bytes, appending an ellipsis when
// truncation occurs. Used for Odesli error bodies so we never log a giant
// HTML error page in full.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
