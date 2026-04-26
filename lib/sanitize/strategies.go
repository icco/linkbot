// Strategy helpers for non-music URL cleaning.
//
// Rule set ported from https://github.com/timball/Careen, copyright Tim
// Ball (timball). Each helper mirrors the corresponding function in
// careen.py so the Go port stays close to the upstream reference. The
// HTTP-using strategies (apple_news, follow_redirect) live on *cleaner in
// news.go because they need access to the HTTP client and the recursion
// depth; everything in this file is a stateless URL → URL transform.

package sanitize

import (
	"context"
	"net/url"
	"strings"
)

// stripAll is Careen's default fall-through strategy: drop the query
// string and the fragment, leaving only scheme + host + path. We assume
// any query parameter on an unrecognised host is tracking junk.
func stripAll(_ context.Context, u *url.URL) (string, error) {
	out := *u
	out.RawQuery = ""
	out.Fragment = ""
	return out.String(), nil
}

// keepAll leaves the URL exactly as it came in. Used for hosts where the
// query string actually carries authentication or routing state we do not
// want to drop (e.g. admin.cloud.microsoft).
func keepAll(_ context.Context, u *url.URL) (string, error) {
	return u.String(), nil
}

// keepSpecificParams returns a strategy that retains only the parameters
// listed in keep and then merges extra on top. This mirrors Careen's
// keep_specific_params and is used for Google search (q + forced
// udm/pws), YouTube (v, t), Twitch (t), and similar hosts where a small
// allowlist of params is meaningful and everything else is tracking
// noise. extra wins on collisions because Careen merges
// {**existing, **extra}.
func keepSpecificParams(keep []string, extra map[string]string) strategy {
	keepSet := make(map[string]struct{}, len(keep))
	for _, k := range keep {
		keepSet[k] = struct{}{}
	}
	return func(_ context.Context, u *url.URL) (string, error) {
		q := u.Query()
		out := url.Values{}
		for k, vs := range q {
			if _, ok := keepSet[k]; !ok {
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

// amazonStrategy strips the /ref=… affiliate/breadcrumb tail from the
// path and clears the query and fragment. Amazon URLs tend to embed
// referrer data in the path itself, so a plain query-only strip would
// leave the path untouched. Mirrors Careen's amazon_strategy.
func amazonStrategy(_ context.Context, u *url.URL) (string, error) {
	next := *u
	if i := strings.Index(next.Path, "/ref="); i >= 0 {
		next.Path = next.Path[:i]
	}
	next.RawQuery = ""
	next.Fragment = ""
	return next.String(), nil
}
