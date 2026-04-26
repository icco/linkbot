// Package careen drops URL tracking params, unwraps redirect hosts, and
// routes known paywalled sites through an archive-mirror prefix.
//
// Host rules and strategies are a Go port of github.com/timball/Careen
// (©Tim Ball).
package careen

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/icco/gutil/logging"
	"go.uber.org/zap"
)

const (
	userAgent      = "linkbot/0.1 (+https://github.com/icco/linkbot)"
	defaultMaxHops = 3
	bodyReadLimit  = 1 << 20
)

// archiveMirrors are interchangeable archive.today aliases used for
// paywall routing and already-archived loop prevention.
var archiveMirrors = []string{
	"archive.fo",
	"archive.is",
	"archive.li",
	"archive.md",
	"archive.ph",
	"archive.today",
}

// pickArchiveMirror returns a mirror host from archiveMirrors. It is a
// var so tests can pin a deterministic mirror via pinMirror.
var pickArchiveMirror = func() string {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(archiveMirrors))))
	if err != nil {
		return archiveMirrors[0]
	}
	return archiveMirrors[n.Int64()]
}

// paywallHosts triggers archive routing in clean. NYT is intentionally
// excluded: its rule preserves the official unlocked_article_code
// gift-link param, so archiving on top would be redundant.
var paywallHosts = []*regexp.Regexp{
	regexp.MustCompile(`(^|\.)bloomberg\.com$`),
	regexp.MustCompile(`(^|\.)bostonglobe\.com$`),
	regexp.MustCompile(`(^|\.)businessinsider\.com$`),
	regexp.MustCompile(`(^|\.)economist\.com$`),
	regexp.MustCompile(`(^|\.)ft\.com$`),
	regexp.MustCompile(`(^|\.)latimes\.com$`),
	regexp.MustCompile(`(^|\.)medium\.com$`),
	regexp.MustCompile(`(^|\.)newyorker\.com$`),
	regexp.MustCompile(`(^|\.)nymag\.com$`),
	regexp.MustCompile(`(^|\.)telegraph\.co\.uk$`),
	regexp.MustCompile(`(^|\.)theatlantic\.com$`),
	regexp.MustCompile(`(^|\.)thetimes\.co\.uk$`),
	regexp.MustCompile(`(^|\.)washingtonpost\.com$`),
	regexp.MustCompile(`(^|\.)wired\.com$`),
	regexp.MustCompile(`(^|\.)wsj\.com$`),
}

// strategy produces a cleaned URL for u.
type strategy func(context.Context, *url.URL) (string, error)

// rule pairs a host regexp with a strategy factory; noArchive=true
// suppresses paywall-archive routing for trusted workspace hosts.
type rule struct {
	pattern   *regexp.Regexp
	make      func(*cleaner) strategy
	noArchive bool
}

// static wraps a stateless strategy as a rule factory.
func static(s strategy) func(*cleaner) strategy {
	return func(_ *cleaner) strategy {
		return s
	}
}

// rules dispatch host → strategy. First match wins; default is stripAll.
// Populated in init to break the var-init cycle through appleNews.
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
		{pattern: regexp.MustCompile(`^admin\.cloud\.microsoft$`), make: static(keepAll), noArchive: true},
		{pattern: regexp.MustCompile(`search\.app$`), make: func(c *cleaner) strategy { return c.followRedirect() }},
	}
}

// cleaner is per-call state; not safe for concurrent use.
type cleaner struct {
	http    *http.Client
	maxHops int
	hop     int
}

// Clean returns a cleaned raw; non-http(s) URLs pass through. hc must be non-nil.
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

// clean dispatches u to the matching strategy and applies paywall
// routing to the result. The hop cap bounds redirect/recursion chains.
func (c *cleaner) clean(ctx context.Context, u *url.URL) (string, error) {
	if u.Scheme != "http" && u.Scheme != "https" {
		return u.String(), nil
	}
	host := strings.ToLower(u.Host)
	if slices.Contains(archiveMirrors, host) {
		return u.String(), nil
	}
	if c.hop >= c.maxHops {
		logging.FromContext(ctx).Warnw("careen: hop cap reached", "url", u.String(), "max_hops", c.maxHops)
		return u.String(), nil
	}
	c.hop++
	defer func() {
		c.hop--
	}()

	s, noArchive := dispatch(c, host)
	cleaned, err := s(ctx, u)
	if err != nil {
		return cleaned, err
	}
	if noArchive || !isPaywalled(host) {
		return cleaned, nil
	}
	return "https://" + pickArchiveMirror() + "/" + cleaned, nil
}

// dispatch returns the strategy and noArchive flag for host. Default
// is stripAll with archive routing enabled.
func dispatch(c *cleaner, host string) (strategy, bool) {
	for _, r := range rules {
		if r.pattern.MatchString(host) {
			return r.make(c), r.noArchive
		}
	}
	return stripAll, false
}

// isPaywalled reports whether host matches any paywallHosts pattern.
func isPaywalled(host string) bool {
	return slices.ContainsFunc(paywallHosts, func(re *regexp.Regexp) bool {
		return re.MatchString(host)
	})
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

// keepSpecificParams keeps only named query params, merges extra, drops fragment.
func keepSpecificParams(keep []string, extra map[string]string) strategy {
	return func(_ context.Context, u *url.URL) (string, error) {
		out := url.Values{}
		for k, vs := range u.Query() {
			if !slices.Contains(keep, k) {
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

// appleNewsRE captures the embedded redirect target in apple.news wrappers.
var appleNewsRE = regexp.MustCompile(`redirectToUrlAfterTimeout\("([^"]+)"`)

// appleNews scrapes the wrapper for its redirect URL and recurses;
// any failure returns u untouched.
func (c *cleaner) appleNews() strategy {
	return func(ctx context.Context, u *url.URL) (string, error) {
		log := logging.FromContext(ctx)
		body, err := c.fetch(ctx, u)
		if err != nil {
			log.Warnw("careen apple.news: fetch failed", "url", u.String(), zap.Error(err))
			return u.String(), nil
		}
		m := appleNewsRE.FindStringSubmatch(body)
		if len(m) < 2 {
			return u.String(), nil
		}
		next, err := url.Parse(m[1])
		if err != nil {
			log.Warnw("careen apple.news: bad redirect", "url", u.String(), "redirect", m[1], zap.Error(err))
			return u.String(), nil
		}
		return c.clean(ctx, next)
	}
}

// followRedirect GETs without auto-redirect and recurses on Location.
func (c *cleaner) followRedirect() strategy {
	return func(ctx context.Context, u *url.URL) (string, error) {
		log := logging.FromContext(ctx)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			log.Warnw("careen follow: build request", "url", u.String(), zap.Error(err))
			return u.String(), nil
		}
		req.Header.Set("User-Agent", userAgent)

		nr := *c.http
		nr.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		}
		resp, err := nr.Do(req)
		if err != nil {
			log.Warnw("careen follow: request failed", "url", u.String(), zap.Error(err))
			return u.String(), nil
		}
		defer func() {
			if err := resp.Body.Close(); err != nil {
				log.Warnw("careen follow: close body", zap.Error(err))
			}
		}()
		if _, err := io.Copy(io.Discard, resp.Body); err != nil && !errors.Is(err, io.EOF) {
			log.Debugw("careen follow: drain body", zap.Error(err))
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
			log.Warnw("careen follow: bad Location", "url", u.String(), "location", loc, zap.Error(err))
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
			logging.FromContext(ctx).Warnw("careen fetch: close body", zap.Error(err))
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
