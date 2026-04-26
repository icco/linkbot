// Package careen rewrites URLs to drop tracking parameters and unwrap
// redirect/wrapper hosts (Google search, Amazon, Apple News, YouTube,
// search.app, …).
//
// The host-pattern rules and per-host strategies are a Go port of
// github.com/timball/Careen, copyright Tim Ball (@timball). The
// opinionated paywall-bypass and archive-mirror logic from Careen is
// deliberately not ported.
package careen

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/icco/linkbot/lib/logctx"
)

const (
	userAgent      = "linkbot/0.1 (+https://github.com/icco/linkbot)"
	defaultMaxHops = 3
	bodyReadLimit  = 1 << 20
)

// strategy produces a cleaned URL for u.
type strategy func(context.Context, *url.URL) (string, error)

// rule pairs a host regexp with a strategy factory. The factory takes the
// active *cleaner so HTTP-using strategies can recurse.
type rule struct {
	pattern *regexp.Regexp
	make    func(*cleaner) strategy
}

// static wraps a stateless strategy as a factory.
func static(s strategy) func(*cleaner) strategy {
	return func(_ *cleaner) strategy {
		return s
	}
}

// rules is populated in init to break the var-init cycle caused by
// rules → cleaner.appleNews → cleaner.clean → rules.
var rules []rule

func init() {
	rules = []rule{
		{
			pattern: regexp.MustCompile(`^(admin|docs|drive|sheets|slides|forms|mail|calendar|sites|meet|chat|contacts)\.google\.`),
			make:    static(keepSpecificParams([]string{"tab", "gid", "usp", "authuser"}, nil)),
		},
		{
			pattern: regexp.MustCompile(`(^|\.)google\.(com|ad|ae|al|am|as|at|az|ba|be|bf|bg|bi|bj|bs|bt|by|ca|cd|cf|cg|ch|ci|cl|cm|cn|cv|cz|de|dj|dk|dm|dz|ee|es|fi|fm|fr|ga|ge|gg|gl|gm|gp|gr|hn|hr|ht|hu|ie|im|iq|is|it|je|jo|kg|ki|kz|la|li|lk|lt|lu|lv|md|me|mg|mk|ml|mn|ms|mu|mv|mw|ne|nl|no|nr|nu|pl|pn|ps|pt|ro|rs|ru|rw|sc|se|sh|si|sk|sm|sn|so|sr|st|td|tg|tk|tl|tm|tn|to|tr|tt|ua|vg|vu|ws)(\.[a-z]{2,3})?$`),
			make:    static(keepSpecificParams([]string{"q"}, map[string]string{"udm": "14", "pws": "0"})),
		},
		{
			pattern: regexp.MustCompile(`(^|\.)amazon\.(com|ca|com\.mx|com\.br|co\.uk|de|fr|it|es|nl|se|pl|com\.tr|ae|sa|eg|in|com\.au|co\.jp)(\.[a-z]{2,3})?$`),
			make:    static(amazonStrategy),
		},
		{pattern: regexp.MustCompile(`(^|\.)reddit\.com$`), make: static(stripAll)},
		{pattern: regexp.MustCompile(`(^|\.)youtube\.com$`), make: static(keepSpecificParams([]string{"v", "t"}, nil))},
		{pattern: regexp.MustCompile(`^youtu\.be$`), make: static(keepSpecificParams([]string{"t"}, nil))},
		{pattern: regexp.MustCompile(`(^|\.)twitch\.tv$`), make: static(keepSpecificParams([]string{"t"}, nil))},
		{pattern: regexp.MustCompile(`^apple\.news$`), make: func(c *cleaner) strategy { return c.appleNews() }},
		{pattern: regexp.MustCompile(`(^|\.)nytimes\.com$`), make: static(keepSpecificParams([]string{"unlocked_article_code"}, nil))},
		{pattern: regexp.MustCompile(`^admin\.cloud\.microsoft$`), make: static(keepAll)},
		{pattern: regexp.MustCompile(`search\.app$`), make: func(c *cleaner) strategy { return c.followRedirect() }},
	}
}

// cleaner is the per-call state. It is not safe for concurrent use; hop
// is mutated as the engine recurses.
type cleaner struct {
	http    *http.Client
	maxHops int
	hop     int
}

// Clean returns a cleaned version of raw. Non-http(s) URLs are returned
// unchanged. hc must not be nil.
func Clean(ctx context.Context, raw string, hc *http.Client) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return raw, fmt.Errorf("parse url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return raw, nil
	}
	c := &cleaner{http: hc, maxHops: defaultMaxHops}
	return c.clean(ctx, u)
}

// clean dispatches u to the matching strategy, enforcing the recursion
// cap.
func (c *cleaner) clean(ctx context.Context, u *url.URL) (string, error) {
	if u.Scheme != "http" && u.Scheme != "https" {
		return u.String(), nil
	}
	if c.hop >= c.maxHops {
		logctx.From(ctx).Warn("careen: hop cap reached", "url", u.String(), "max_hops", c.maxHops)
		return u.String(), nil
	}
	c.hop++
	defer func() {
		c.hop--
	}()

	host := strings.ToLower(u.Host)
	for _, r := range rules {
		if r.pattern.MatchString(host) {
			return r.make(c)(ctx, u)
		}
	}
	return stripAll(ctx, u)
}

// stripAll drops the query and fragment.
func stripAll(_ context.Context, u *url.URL) (string, error) {
	out := *u
	out.RawQuery = ""
	out.Fragment = ""
	return out.String(), nil
}

// keepAll returns u unmodified.
func keepAll(_ context.Context, u *url.URL) (string, error) {
	return u.String(), nil
}

// keepSpecificParams keeps only the named query params, then merges
// extra on top. The fragment is always dropped.
func keepSpecificParams(keep []string, extra map[string]string) strategy {
	set := make(map[string]struct{}, len(keep))
	for _, k := range keep {
		set[k] = struct{}{}
	}
	return func(_ context.Context, u *url.URL) (string, error) {
		q := u.Query()
		out := url.Values{}
		for k, vs := range q {
			if _, ok := set[k]; !ok {
				continue
			}
			for _, v := range vs {
				out.Add(k, v)
			}
		}
		for k, v := range extra {
			out.Set(k, v)
		}
		next := *u
		next.RawQuery = out.Encode()
		next.Fragment = ""
		return next.String(), nil
	}
}

// amazonStrategy drops the /ref=… path tail and the query.
func amazonStrategy(_ context.Context, u *url.URL) (string, error) {
	next := *u
	if i := strings.Index(next.Path, "/ref="); i >= 0 {
		next.Path = next.Path[:i]
	}
	next.RawQuery = ""
	next.Fragment = ""
	return next.String(), nil
}

// appleNewsRE captures the destination URL embedded in apple.news wrapper
// pages, which inline a redirectToUrlAfterTimeout("...") call.
var appleNewsRE = regexp.MustCompile(`redirectToUrlAfterTimeout\("([^"]+)"`)

// appleNews scrapes the wrapper for its embedded redirect URL and
// recurses. On any failure it returns u untouched.
func (c *cleaner) appleNews() strategy {
	return func(ctx context.Context, u *url.URL) (string, error) {
		log := logctx.From(ctx)
		body, err := c.fetch(ctx, u)
		if err != nil {
			log.Warn("careen apple.news: fetch failed", "url", u.String(), "error", err)
			return u.String(), nil
		}
		m := appleNewsRE.FindStringSubmatch(body)
		if len(m) < 2 {
			return u.String(), nil
		}
		next, err := url.Parse(m[1])
		if err != nil {
			log.Warn("careen apple.news: bad redirect", "url", u.String(), "redirect", m[1], "error", err)
			return u.String(), nil
		}
		return c.clean(ctx, next)
	}
}

// followRedirect issues a GET with redirects disabled and recurses on the
// Location header. Non-3xx responses leave u untouched.
func (c *cleaner) followRedirect() strategy {
	return func(ctx context.Context, u *url.URL) (string, error) {
		log := logctx.From(ctx)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			log.Warn("careen follow: build request", "url", u.String(), "error", err)
			return u.String(), nil
		}
		req.Header.Set("User-Agent", userAgent)

		nr := *c.http
		nr.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		}
		resp, err := nr.Do(req)
		if err != nil {
			log.Warn("careen follow: request failed", "url", u.String(), "error", err)
			return u.String(), nil
		}
		defer func() {
			if err := resp.Body.Close(); err != nil {
				log.Warn("careen follow: close body", "error", err)
			}
		}()
		if _, err := io.Copy(io.Discard, resp.Body); err != nil && !errors.Is(err, io.EOF) {
			log.Debug("careen follow: drain body", "error", err)
		}

		if resp.StatusCode < http.StatusMultipleChoices || resp.StatusCode >= http.StatusBadRequest {
			return u.String(), nil
		}
		loc := resp.Header.Get("Location")
		if loc == "" {
			return u.String(), nil
		}
		next, err := url.Parse(loc)
		if err != nil {
			log.Warn("careen follow: bad Location", "url", u.String(), "location", loc, "error", err)
			return u.String(), nil
		}
		if !next.IsAbs() {
			next = u.ResolveReference(next)
		}
		return c.clean(ctx, next)
	}
}

// fetch GETs u and returns the body, capped at bodyReadLimit.
func (c *cleaner) fetch(ctx context.Context, u *url.URL) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("request: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			logctx.From(ctx).Warn("careen fetch: close body", "error", err)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, bodyReadLimit))
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}
	return string(body), nil
}
