// Package odesli is a client for the Odesli (song.link) API. The
// default transport is wrapped with otelhttp for auto traces and
// http.client.request.duration.
package odesli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/icco/gutil/logging"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.uber.org/zap"
)

// DefaultBaseURL is the public Odesli endpoint.
const DefaultBaseURL = "https://api.song.link/v1-alpha.1/links"

// userAgent is sent on every Odesli request.
const userAgent = "linkbot/0.1 (+https://github.com/icco/linkbot)"

// Client talks to the Odesli API.
type Client struct {
	http    *http.Client
	baseURL string
	apiKey  string
	country string
}

// Option configures a Client.
type Option func(*Client)

// WithAPIKey sets an Odesli API key (raises rate limits).
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

// WithHTTPClient overrides the HTTP client; callers own its
// instrumentation.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		c.http = h
	}
}

// New returns a Client with a 15 s timeout, otelhttp-wrapped transport.
func New(opts ...Option) *Client {
	c := &Client{
		http: &http.Client{
			Timeout:   15 * time.Second,
			Transport: otelhttp.NewTransport(http.DefaultTransport),
		},
		baseURL: DefaultBaseURL,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Response is the subset of /links we use.
type Response struct {
	EntityUniqueID string `json:"entityUniqueId"`
	UserCountry    string `json:"userCountry"`
	PageURL        string `json:"pageUrl"`
}

// Resolve returns the song.link page URL for a streaming link.
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
	req.Header.Set("User-Agent", userAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("odesli request: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			logging.FromContext(ctx).Warnw("close odesli response body", zap.Error(err))
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

// truncate clips s to n bytes, appending "..." when cut, so giant
// error bodies don't blow up logs.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
