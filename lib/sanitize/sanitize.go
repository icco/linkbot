// Package sanitize rewrites URLs to friendlier or canonical forms.
//
// The package handles two categories of cleanup. Music streaming links
// (Spotify, Apple Music, YouTube Music, Tidal, Deezer, …) are resolved
// through the Odesli (song.link) API to a single universal page URL.
// All other links flow through a host-aware rule registry — ported from
// timball/Careen — that strips tracking parameters, unwraps Apple News
// pages, and follows search.app shortlinks. See lib/sanitize/news.go for
// the rule set and AGENTS.md for the conventions every contributor follows.
package sanitize

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/icco/linkbot/lib/odesli"
)

// urlRE matches bare http(s) URLs in free-form text. It stops at common
// punctuation and whitespace so trailing characters in chat messages are
// not captured as part of the link.
var urlRE = regexp.MustCompile(`https?://[^\s<>"'\x60]+`)

// musicHosts is the set of streaming-service host suffixes Odesli understands.
var musicHosts = []string{
	"open.spotify.com",
	"spotify.link",
	"music.apple.com",
	"music.youtube.com",
	"youtube.com",
	"youtu.be",
	"tidal.com",
	"deezer.com",
	"music.amazon.com",
	"soundcloud.com",
	"pandora.com",
	"music.yandex.com",
	"audiomack.com",
	"anghami.com",
	"boomplay.com",
	"napster.com",
}

// defaultHTTPTimeout is the per-request timeout for outbound calls made by
// the news-cleaning strategies (apple.news unwrap, search.app redirect
// follow, etc.). It matches Careen's 5 s budget — we'd rather fall back to
// the original URL than slow a Discord reply waiting on a wrapper page.
const defaultHTTPTimeout = 5 * time.Second

// Sanitizer rewrites URLs. It owns an Odesli client for music links and a
// dedicated *http.Client for the news-cleaning strategies; the latter is
// kept separate (and short-timeout) from any caller-supplied transport so
// outbound unwraps cannot stall a Discord reply.
type Sanitizer struct {
	odesli *odesli.Client
	log    *slog.Logger
	hc     *http.Client
}

// Option configures a Sanitizer at construction time. The functional-option
// pattern matches the convention used by the Odesli client and lets us add
// future knobs (custom rule registries, paywall hooks, …) without breaking
// existing call sites.
type Option func(*Sanitizer)

// WithHTTPClient overrides the *http.Client used by the news-cleaning
// strategies. Pass a client with a longer timeout if you want apple.news
// unwrapping to be patient with slow upstreams, or a recording client in
// tests.
func WithHTTPClient(h *http.Client) Option {
	return func(s *Sanitizer) {
		s.hc = h
	}
}

// New constructs a Sanitizer. The positional Odesli client and base logger
// are required; everything else flows through Option helpers. When no
// WithHTTPClient is supplied the Sanitizer falls back to a 5 s timeout
// client, matching Careen's default.
func New(o *odesli.Client, log *slog.Logger, opts ...Option) *Sanitizer {
	s := &Sanitizer{
		odesli: o,
		log:    log,
		hc:     &http.Client{Timeout: defaultHTTPTimeout},
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// FindURLs returns all http(s) URLs in text, with trailing punctuation trimmed.
func FindURLs(text string) []string {
	matches := urlRE.FindAllString(text, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, strings.TrimRight(m, ".,;:!?)]}"))
	}
	return out
}

// URL returns a sanitized version of raw, or the original string when no
// rewrite applies. An error means sanitization was attempted but failed.
func (s *Sanitizer) URL(ctx context.Context, raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return raw, fmt.Errorf("parse url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return raw, nil
	}

	if isMusicHost(u.Host) {
		resp, err := s.odesli.Resolve(ctx, raw)
		if err != nil {
			return raw, err
		}
		return resp.PageURL, nil
	}

	cleaned, err := cleanNewsURL(ctx, u, s.hc)
	if err != nil {
		return raw, fmt.Errorf("news sanitize: %w", err)
	}
	return cleaned, nil
}

// Changed reports whether sanitization produced a different URL.
func Changed(before, after string) bool {
	return before != after && after != ""
}

// isMusicHost reports whether host (or any subdomain of host) belongs to a
// streaming service Odesli understands. Comparison is case-insensitive.
func isMusicHost(host string) bool {
	host = strings.ToLower(host)
	for _, suffix := range musicHosts {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}
