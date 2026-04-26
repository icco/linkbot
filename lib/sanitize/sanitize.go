// Package sanitize rewrites URLs to friendlier forms. Music streaming
// links are resolved through the Odesli (song.link) API; everything else
// flows through the careen package's host-aware rule engine.
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

	"github.com/icco/linkbot/lib/careen"
	"github.com/icco/linkbot/lib/odesli"
)

// urlRE matches bare http(s) URLs in free-form text, stopping at common
// punctuation and whitespace so trailing characters do not get captured.
var urlRE = regexp.MustCompile(`https?://[^\s<>"'\x60]+`)

// musicHosts lists the streaming hosts Odesli understands.
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

// defaultHTTPTimeout caps outbound calls made by careen.Clean.
const defaultHTTPTimeout = 5 * time.Second

// Sanitizer rewrites URLs. Music links go through Odesli; everything
// else goes through careen.Clean using hc.
type Sanitizer struct {
	odesli *odesli.Client
	log    *slog.Logger
	hc     *http.Client
}

// Option configures a Sanitizer at construction time.
type Option func(*Sanitizer)

// WithHTTPClient overrides the *http.Client used by careen.Clean.
func WithHTTPClient(h *http.Client) Option {
	return func(s *Sanitizer) {
		s.hc = h
	}
}

// New constructs a Sanitizer. Without WithHTTPClient it uses a 5 s
// timeout client.
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

// FindURLs returns all http(s) URLs in text, with trailing punctuation
// trimmed.
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

	cleaned, err := careen.Clean(ctx, raw, s.hc)
	if err != nil {
		return raw, fmt.Errorf("careen clean: %w", err)
	}
	return cleaned, nil
}

// Changed reports whether sanitization produced a different URL.
func Changed(before, after string) bool {
	return before != after && after != ""
}

// isMusicHost reports whether host (or any subdomain of host) belongs to
// a streaming service Odesli understands. Case-insensitive.
func isMusicHost(host string) bool {
	host = strings.ToLower(host)
	for _, suffix := range musicHosts {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}
