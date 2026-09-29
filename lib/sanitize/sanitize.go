// Package sanitize rewrites URLs: music links via Odesli (song.link),
// everything else via careen's host-aware rules.
package sanitize

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/icco/odesli"
	"go.icco.me/linkbot/lib/careen"
)

// urlRE matches http(s) URLs, stopping at whitespace, quotes, and angle brackets.
var urlRE = regexp.MustCompile(`(?i)https?://[^\s<>"'\x60]+`)

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

// Sanitizer rewrites URLs via Odesli or careen.Clean.
type Sanitizer struct {
	odesli *odesli.Client
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

// New constructs a Sanitizer with a 5 s Careen client and a default Odesli client if o is nil.
func New(o *odesli.Client, opts ...Option) *Sanitizer {
	if o == nil {
		o = odesli.New()
	}
	s := &Sanitizer{
		odesli: o,
		hc:     &http.Client{Timeout: defaultHTTPTimeout},
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// FindURLs returns http(s) URLs in text, with trailing punctuation trimmed.
func FindURLs(text string) []string {
	matches := urlRE.FindAllString(text, -1)
	for i, match := range matches {
		match = strings.TrimRight(match, ".,;:!?")
		// Keep balanced delimiters in paths, such as Wikipedia article titles.
		for len(match) > 0 {
			end := match[len(match)-1]
			open := byte(0)
			switch end {
			case ')':
				open = '('
			case ']':
				open = '['
			case '}':
				open = '{'
			}
			if open == 0 || strings.Count(match, string(open)) >= strings.Count(match, string(end)) {
				break
			}
			match = strings.TrimRight(match[:len(match)-1], ".,;:!?")
		}
		matches[i] = match
	}
	return matches
}

// URL returns a sanitized raw, or raw itself when no rewrite applies.
func (s *Sanitizer) URL(ctx context.Context, raw string) (string, error) {
	if err := ctx.Err(); err != nil {
		return raw, err
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw, fmt.Errorf("parse url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return raw, nil
	}

	if isMusicHost(u.Hostname()) {
		resp, err := s.odesli.Resolve(ctx, raw)
		if err != nil {
			return raw, fmt.Errorf("odesli resolve: %w", err)
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

// isMusicHost reports whether host (or a subdomain) is in musicHosts.
func isMusicHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	return slices.ContainsFunc(musicHosts, func(suffix string) bool {
		return host == suffix || strings.HasSuffix(host, "."+suffix)
	})
}
