// Package sanitize rewrites URLs to friendlier or canonical forms.
//
// The current implementation only handles music streaming links, which it
// resolves through the Odesli (song.link) API. Other categories of cleanup
// (tracking parameters, AMP, redirect unwrapping, etc.) are intentionally
// stubbed and will land in a follow-up PR.
package sanitize

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"

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

// Sanitizer rewrites URLs.
type Sanitizer struct {
	odesli *odesli.Client
	log    *slog.Logger
}

// New constructs a Sanitizer.
func New(o *odesli.Client, log *slog.Logger) *Sanitizer {
	return &Sanitizer{odesli: o, log: log}
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

	// TODO(future PR): strip tracking params, unwrap AMP, follow shorteners,
	// canonicalize trailing slashes, etc.
	return raw, nil
}

// Changed reports whether sanitization produced a different URL.
func Changed(before, after string) bool {
	return before != after && after != ""
}

func isMusicHost(host string) bool {
	host = strings.ToLower(host)
	for _, suffix := range musicHosts {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}
