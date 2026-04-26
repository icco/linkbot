package sanitize

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

// Rule set ported from https://github.com/timball/Careen, copyright Tim Ball
// (timball). Tim's project — and the companion iCloud Shortcut — are the
// authoritative reference for which query parameters and path fragments are
// safe to drop per host. We re-implement the rules in Go but the policy
// decisions are his.

// newsUserAgent is the User-Agent we send when scraping wrapper pages
// (apple.news) or sniffing redirects (search.app). It follows the
// linkbot/<version> (+repo URL) convention from AGENTS.md so the operators
// of those services can identify and contact us.
const newsUserAgent = "linkbot/0.1 (+https://github.com/icco/linkbot)"

// defaultMaxNewsHops bounds recursion through redirect-following strategies
// (apple_news, follow_redirect). Careen has no explicit cap and instead
// relies on the default strip_all strategy terminating the chain; we bound
// it defensively so a misbehaving server cannot pin a goroutine.
const defaultMaxNewsHops = 3

// strategy produces a cleaned URL string for u. Strategies that fetch the
// network (apple_news, follow_redirect) are constructed with a *cleaner so
// they can recurse via cleaner.clean once they have a new URL in hand.
type strategy func(context.Context, *url.URL) (string, error)

// rule pairs a host-name regexp with a strategy factory. The factory takes
// the active *cleaner so that recursion-aware strategies can call back into
// it; stateless strategies ignore the argument and return a closed-over
// strategy via static.
type rule struct {
	pattern *regexp.Regexp
	make    func(*cleaner) strategy
}

// static wraps a stateless strategy as a strategy factory. The stateless
// strategies (stripAll, keepAll, keepSpecificParams, amazonStrategy) live
// in strategies.go.
func static(s strategy) func(*cleaner) strategy {
	return func(_ *cleaner) strategy {
		return s
	}
}

// newsRules is the host-pattern → strategy registry, ported from
// timball/Careen. The first matching pattern wins; if no rule matches, the
// engine falls through to stripAll. Order matters because the Google admin
// rule must shadow the broader Google search rule.
//
// We populate newsRules in init rather than as a literal initializer
// because the apple_news and search.app entries close over methods on
// *cleaner whose bodies transitively reference newsRules itself, which the
// Go compiler would otherwise reject as an initialization cycle.
//
// Rule set ported from https://github.com/timball/Careen, copyright Tim
// Ball (timball).
var newsRules []rule

func init() {
	newsRules = []rule{
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
		{
			pattern: regexp.MustCompile(`(^|\.)reddit\.com$`),
			make:    static(stripAll),
		},
		{
			pattern: regexp.MustCompile(`(^|\.)youtube\.com$`),
			make:    static(keepSpecificParams([]string{"v", "t"}, nil)),
		},
		{
			pattern: regexp.MustCompile(`^youtu\.be$`),
			make:    static(keepSpecificParams([]string{"t"}, nil)),
		},
		{
			pattern: regexp.MustCompile(`(^|\.)twitch\.tv$`),
			make:    static(keepSpecificParams([]string{"t"}, nil)),
		},
		{
			pattern: regexp.MustCompile(`^apple\.news$`),
			make: func(c *cleaner) strategy {
				return c.appleNewsStrategy()
			},
		},
		{
			pattern: regexp.MustCompile(`(^|\.)nytimes\.com$`),
			make:    static(keepSpecificParams([]string{"unlocked_article_code"}, nil)),
		},
		{
			pattern: regexp.MustCompile(`^admin\.cloud\.microsoft$`),
			make:    static(keepAll),
		},
		{
			pattern: regexp.MustCompile(`search\.app$`),
			make: func(c *cleaner) strategy {
				return c.followRedirect()
			},
		},
	}
}

// cleaner carries the state needed to apply news-cleaning strategies to a
// URL, including the HTTP client used by the scraping/redirect strategies
// and a recursion-depth counter. A cleaner is created per top-level
// invocation; it is not safe for concurrent use because hop is mutated as
// the engine recurses.
type cleaner struct {
	http    *http.Client
	maxHops int
	hop     int
}

// cleanNewsURL is the package-internal entry point used by Sanitizer.URL.
// It walks the rule registry, picks the first matching strategy (or
// stripAll as the default), applies it, and recurses for the
// redirect-following strategies up to maxHops levels deep. hc must not be
// nil; callers should pass the Sanitizer's *http.Client.
func cleanNewsURL(ctx context.Context, u *url.URL, hc *http.Client) (string, error) {
	c := &cleaner{
		http:    hc,
		maxHops: defaultMaxNewsHops,
	}
	return c.clean(ctx, u)
}

// clean dispatches u to the matching strategy. It enforces the recursion
// cap, logging a warning and returning the current URL untouched when the
// cap is hit so a misconfigured redirect chain cannot stall a request.
func (c *cleaner) clean(ctx context.Context, u *url.URL) (string, error) {
	if u.Scheme != "http" && u.Scheme != "https" {
		return u.String(), nil
	}
	if c.hop >= c.maxHops {
		logctx.From(ctx).Warn("news cleaner: recursion cap reached, returning current URL",
			"url", u.String(), "hop", c.hop, "max_hops", c.maxHops)
		return u.String(), nil
	}
	c.hop++
	defer func() {
		c.hop--
	}()

	host := strings.ToLower(u.Host)
	s := c.selectStrategy(host)
	return s(ctx, u)
}

// selectStrategy walks newsRules and returns the first strategy whose host
// pattern matches. When nothing matches it falls back to stripAll, the
// Careen default for unknown hosts.
func (c *cleaner) selectStrategy(host string) strategy {
	for _, r := range newsRules {
		if r.pattern.MatchString(host) {
			return r.make(c)
		}
	}
	return stripAll
}

// appleNewsRedirectRE captures the destination URL embedded in apple.news
// wrapper pages. Apple ships an HTML page that inlines a JS bootstrap
// calling redirectToUrlAfterTimeout("https://...") with the canonical
// publisher URL, so we scrape that line rather than render the page.
var appleNewsRedirectRE = regexp.MustCompile(`redirectToUrlAfterTimeout\("([^"]+)"`)

// appleNewsStrategy returns a strategy that resolves apple.news wrapper
// links to the underlying publisher URL, then recurses through the engine
// so the publisher URL itself is cleaned (e.g. nytimes.com).
func (c *cleaner) appleNewsStrategy() strategy {
	return func(ctx context.Context, u *url.URL) (string, error) {
		log := logctx.From(ctx)
		body, err := c.fetchString(ctx, http.MethodGet, u)
		if err != nil {
			log.Warn("apple news: fetch failed; returning original",
				"url", u.String(), "error", err)
			return u.String(), nil
		}
		m := appleNewsRedirectRE.FindStringSubmatch(body)
		if len(m) < 2 {
			log.Warn("apple news: redirect token not found; returning original",
				"url", u.String())
			return u.String(), nil
		}
		next, err := url.Parse(m[1])
		if err != nil {
			log.Warn("apple news: invalid redirect URL; returning original",
				"url", u.String(), "redirect", m[1], "error", err)
			return u.String(), nil
		}
		return c.clean(ctx, next)
	}
}

// followRedirect returns a strategy that issues a GET with redirects
// disabled, reads the Location header from the first 3xx response, and
// recurses through the engine so the destination is also cleaned.
// Non-3xx responses or empty Location headers cause the strategy to return
// the original URL untouched, mirroring Careen's failure mode.
func (c *cleaner) followRedirect() strategy {
	return func(ctx context.Context, u *url.URL) (string, error) {
		log := logctx.From(ctx)

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			log.Warn("follow redirect: build request failed; returning original",
				"url", u.String(), "error", err)
			return u.String(), nil
		}
		req.Header.Set("User-Agent", newsUserAgent)

		// Copy the configured client and replace its CheckRedirect so we see
		// the first 3xx response instead of letting net/http auto-follow.
		// Copying preserves the timeout and any custom Transport the caller
		// configured.
		nr := *c.http
		nr.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		}

		resp, err := nr.Do(req)
		if err != nil {
			log.Warn("follow redirect: request failed; returning original",
				"url", u.String(), "error", err)
			return u.String(), nil
		}
		defer func() {
			if err := resp.Body.Close(); err != nil {
				log.Warn("follow redirect: close response body",
					"url", u.String(), "error", err)
			}
		}()
		if _, err := io.Copy(io.Discard, resp.Body); err != nil && !errors.Is(err, io.EOF) {
			log.Debug("follow redirect: drain body",
				"url", u.String(), "error", err)
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
			log.Warn("follow redirect: invalid Location header; returning original",
				"url", u.String(), "location", loc, "error", err)
			return u.String(), nil
		}
		if !next.IsAbs() {
			next = u.ResolveReference(next)
		}
		return c.clean(ctx, next)
	}
}

// fetchString issues an HTTP request to u using the cleaner's client and
// returns the response body as a string. It enforces the linkbot
// User-Agent and bounds the response read so a hostile server can't hand
// us an unbounded body. Used by appleNewsStrategy.
func (c *cleaner) fetchString(ctx context.Context, method string, u *url.URL) (string, error) {
	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", newsUserAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("request: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			logctx.From(ctx).Warn("fetch string: close body",
				"url", u.String(), "error", err)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	// 1 MiB is plenty for an apple.news wrapper page (they are ~50–100 KiB
	// today) and small enough that a hostile server cannot starve us of
	// memory.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}
	return string(body), nil
}
