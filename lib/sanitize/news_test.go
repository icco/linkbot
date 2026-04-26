// Tests for the news/general-link cleaner. The rule set is ported from
// timball/Careen, so the test cases mirror the behaviour the Python
// implementation produces for each domain class.

package sanitize

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// mustParse is a tiny helper for table tests: parsing a URL is never
// supposed to fail in a fixture, so a failure here is a test bug.
func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}

// newTestCleaner returns a *cleaner configured for the unit tests: the
// caller's HTTP client is used as-is, and the recursion cap matches the
// production default so we exercise the same code paths.
func newTestCleaner(hc *http.Client) *cleaner {
	return &cleaner{
		http:    hc,
		maxHops: defaultMaxNewsHops,
	}
}

// TestCleanNewsURL_RuleRegistry walks the rule registry with one URL per
// non-HTTP-following rule plus a few default-strip cases. The HTTP-using
// rules (apple.news, search.app) get their own focused tests below because
// they need an httptest.Server to assert the unwrap/redirect logic.
func TestCleanNewsURL_RuleRegistry(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "google search keeps q and forces udm/pws",
			in:   "https://www.google.com/search?q=hello+world&utm_source=bar&pws=1",
			want: "https://www.google.com/search?pws=0&q=hello+world&udm=14",
		},
		{
			name: "google ccTLD with secondary suffix",
			in:   "https://www.google.com.au/search?q=foo&hl=en",
			want: "https://www.google.com.au/search?pws=0&q=foo&udm=14",
		},
		{
			name: "google co.uk falls through (co not in Careen TLD list)",
			in:   "https://www.google.co.uk/search?q=foo&hl=en",
			want: "https://www.google.co.uk/search",
		},
		{
			name: "google workspace keeps tab and authuser",
			in:   "https://docs.google.com/document/d/abc?tab=t.0&authuser=1&utm=x",
			want: "https://docs.google.com/document/d/abc?authuser=1&tab=t.0",
		},
		{
			name: "amazon strips ref tail and query",
			in:   "https://www.amazon.com/Some-Product/dp/B000TEST/ref=cm_sw_r_other?utm=x&pf=1",
			want: "https://www.amazon.com/Some-Product/dp/B000TEST",
		},
		{
			name: "amazon co.uk hits the rule",
			in:   "https://www.amazon.co.uk/dp/B000/ref=foo",
			want: "https://www.amazon.co.uk/dp/B000",
		},
		{
			name: "reddit nukes everything",
			in:   "https://www.reddit.com/r/golang/comments/x/post?utm_source=share&context=3",
			want: "https://www.reddit.com/r/golang/comments/x/post",
		},
		{
			name: "youtube keeps v and t",
			in:   "https://www.youtube.com/watch?v=dQw4w9WgXcQ&t=42&feature=share&pp=tracking",
			want: "https://www.youtube.com/watch?t=42&v=dQw4w9WgXcQ",
		},
		{
			name: "youtu.be keeps t",
			in:   "https://youtu.be/dQw4w9WgXcQ?t=42&feature=share",
			want: "https://youtu.be/dQw4w9WgXcQ?t=42",
		},
		{
			name: "twitch keeps t",
			in:   "https://www.twitch.tv/somestreamer/clip/abc?t=01h02m&filter=clips",
			want: "https://www.twitch.tv/somestreamer/clip/abc?t=01h02m",
		},
		{
			name: "nytimes keeps unlocked_article_code",
			in:   "https://www.nytimes.com/2026/01/01/world/article.html?unlocked_article_code=abcd&smid=share",
			want: "https://www.nytimes.com/2026/01/01/world/article.html?unlocked_article_code=abcd",
		},
		{
			name: "admin.cloud.microsoft is left untouched",
			in:   "https://admin.cloud.microsoft/?ref=AdminPortal&route=foo",
			want: "https://admin.cloud.microsoft/?ref=AdminPortal&route=foo",
		},
		{
			name: "default unknown host strips query and fragment",
			in:   "https://example.com/some/path?utm_source=foo&utm_medium=bar#frag",
			want: "https://example.com/some/path",
		},
		{
			name: "default keeps existing path verbatim",
			in:   "https://blog.example.org/2026/04/some-post/?ref=newsletter",
			want: "https://blog.example.org/2026/04/some-post/",
		},
		{
			name: "non-http schemes pass through untouched",
			in:   "mailto:someone@example.com?subject=hi",
			want: "mailto:someone@example.com?subject=hi",
		},
		{
			name: "uppercase host still matches rule via lowercased compare",
			in:   "https://WWW.YOUTUBE.COM/watch?v=abc&utm=x",
			want: "https://WWW.YOUTUBE.COM/watch?v=abc",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			u := mustParse(t, tc.in)
			got, err := cleanNewsURL(context.Background(), u, http.DefaultClient)
			if err != nil {
				t.Fatalf("cleanNewsURL(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("cleanNewsURL(%q):\n got: %q\nwant: %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestAppleNewsStrategy verifies the apple.news wrapper unwrap by handing
// the strategy a fake apple.news page hosted on httptest. The redirected
// URL is on an unknown host so the recursion falls into stripAll, giving
// us a deterministic expected output.
func TestAppleNewsStrategy(t *testing.T) {
	const target = "https://www.example.com/article?utm_source=foo"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Apple ships a chunk of HTML with a JS bootstrap that calls
		// redirectToUrlAfterTimeout("..."). The strategy only cares about
		// that one regex hit, so a tiny stub page is enough.
		_, _ = fmt.Fprintf(w, `<html><body><script>setTimeout(function(){redirectToUrlAfterTimeout(%q)},1000);</script></body></html>`, target)
	}))
	defer srv.Close()

	c := newTestCleaner(srv.Client())
	fn := c.appleNewsStrategy()
	u := mustParse(t, srv.URL)

	got, err := fn(context.Background(), u)
	if err != nil {
		t.Fatalf("apple news strategy: %v", err)
	}
	const want = "https://www.example.com/article"
	if got != want {
		t.Errorf("apple news strategy:\n got: %q\nwant: %q", got, want)
	}
}

// TestAppleNewsStrategyNoMatch verifies the strategy degrades gracefully
// when the response does not contain the redirectToUrlAfterTimeout token.
// In that case Careen returns the original URL — same behaviour we want.
func TestAppleNewsStrategyNoMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `<html><body>nothing useful here</body></html>`)
	}))
	defer srv.Close()

	c := newTestCleaner(srv.Client())
	fn := c.appleNewsStrategy()
	u := mustParse(t, srv.URL+"/wrap")

	got, err := fn(context.Background(), u)
	if err != nil {
		t.Fatalf("apple news strategy: %v", err)
	}
	if got != u.String() {
		t.Errorf("apple news strategy expected pass-through:\n got: %q\nwant: %q", got, u.String())
	}
}

// TestFollowRedirectStrategy sets up a 302 → real URL hop and asserts the
// strategy follows it and runs the destination through the engine. The
// destination host is unknown so it falls into stripAll, mirroring how a
// search.app shortlink that leads to e.g. a tracker-laden article would be
// cleaned.
func TestFollowRedirectStrategy(t *testing.T) {
	const target = "https://www.example.com/landing?utm_source=googleapp"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target)
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	c := newTestCleaner(srv.Client())
	fn := c.followRedirect()
	u := mustParse(t, srv.URL+"/short")

	got, err := fn(context.Background(), u)
	if err != nil {
		t.Fatalf("follow redirect: %v", err)
	}
	const want = "https://www.example.com/landing"
	if got != want {
		t.Errorf("follow redirect:\n got: %q\nwant: %q", got, want)
	}
}

// TestFollowRedirectStrategyNon3xx asserts that a 200 OK response causes
// the strategy to return the original URL unchanged. We do not want to
// silently fabricate a target URL when the upstream isn't actually
// redirecting us anywhere.
func TestFollowRedirectStrategyNon3xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestCleaner(srv.Client())
	fn := c.followRedirect()
	u := mustParse(t, srv.URL+"/short")

	got, err := fn(context.Background(), u)
	if err != nil {
		t.Fatalf("follow redirect: %v", err)
	}
	if got != u.String() {
		t.Errorf("follow redirect expected pass-through:\n got: %q\nwant: %q", got, u.String())
	}
}

// TestFollowRedirectStrategyRelativeLocation covers the "Location: /next"
// case — a relative URL that has to be resolved against the request URL
// before the engine can recurse. Many real shortlink services emit
// relative redirects, so this path matters.
func TestFollowRedirectStrategyRelativeLocation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/short":
			w.Header().Set("Location", "/landing?utm=x")
			w.WriteHeader(http.StatusFound)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	c := newTestCleaner(srv.Client())
	fn := c.followRedirect()
	u := mustParse(t, srv.URL+"/short")

	got, err := fn(context.Background(), u)
	if err != nil {
		t.Fatalf("follow redirect: %v", err)
	}
	want := srv.URL + "/landing"
	if got != want {
		t.Errorf("follow redirect:\n got: %q\nwant: %q", got, want)
	}
}

// TestRecursionCap verifies that a cleaner refuses to recurse beyond
// maxHops, returning the current URL untouched and (in production) logging
// a warning. We simulate this by pre-loading the cleaner's hop counter to
// the cap; clean() should bail out before invoking any strategy.
func TestRecursionCap(t *testing.T) {
	c := &cleaner{
		http:    http.DefaultClient,
		maxHops: 3,
		hop:     3,
	}
	const raw = "https://www.google.com/search?q=foo&utm_source=bar"
	u := mustParse(t, raw)

	got, err := c.clean(context.Background(), u)
	if err != nil {
		t.Fatalf("clean: %v", err)
	}
	if got != raw {
		t.Errorf("expected URL untouched at the cap:\n got: %q\nwant: %q", got, raw)
	}
}

// TestRecursionCapViaRedirectChain sets up a server that always 302s back
// to itself. With follow_redirect handling search.app-style links, an
// infinite redirect loop must be capped by the engine rather than spinning
// forever or hitting the underlying http.Client redirect limit.
func TestRecursionCapViaRedirectChain(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Location", r.URL.String())
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	c := newTestCleaner(srv.Client())
	fn := c.followRedirect()
	u := mustParse(t, srv.URL+"/loop")

	got, err := fn(context.Background(), u)
	if err != nil {
		t.Fatalf("follow redirect: %v", err)
	}
	// The engine's hop counter caps recursion, so we should not hammer
	// the test server endlessly. defaultMaxNewsHops is 3, and each hop
	// makes exactly one HTTP call — so we expect at most defaultMaxNewsHops
	// hits before the cap kicks in. The exact returned URL doesn't matter
	// here; what matters is that the loop terminated.
	if hits > defaultMaxNewsHops {
		t.Errorf("redirect loop should be capped at %d hits, got %d", defaultMaxNewsHops, hits)
	}
	if !strings.HasPrefix(got, srv.URL) {
		t.Errorf("expected URL to remain on the test server after cap, got %q", got)
	}
}

// TestNewSanitizerDefaultsHTTPClient confirms that New attaches a 5 s
// timeout client when WithHTTPClient is omitted. The default is documented
// behaviour, so a regression here would silently widen our outbound
// timeouts.
func TestNewSanitizerDefaultsHTTPClient(t *testing.T) {
	s := New(nil, nil)
	if s.hc == nil {
		t.Fatal("expected default *http.Client, got nil")
	}
	if s.hc.Timeout != defaultHTTPTimeout {
		t.Errorf("default timeout: got %v want %v", s.hc.Timeout, defaultHTTPTimeout)
	}
}

// TestWithHTTPClient confirms the option overrides the default HTTP client.
func TestWithHTTPClient(t *testing.T) {
	custom := &http.Client{Timeout: 42 * time.Second}
	s := New(nil, nil, WithHTTPClient(custom))
	if s.hc != custom {
		t.Errorf("WithHTTPClient did not install custom client")
	}
}

// TestSanitizerURL_NewsCleansUnknownHost end-to-end smoke-tests that
// Sanitizer.URL routes non-music links through the news cleaner. We use
// example.com to avoid touching the network — the default strip_all
// strategy never makes an outbound call.
func TestSanitizerURL_NewsCleansUnknownHost(t *testing.T) {
	s := New(nil, nil)
	got, err := s.URL(context.Background(), "https://example.com/foo?utm_source=bar")
	if err != nil {
		t.Fatalf("URL: %v", err)
	}
	const want = "https://example.com/foo"
	if got != want {
		t.Errorf("URL:\n got: %q\nwant: %q", got, want)
	}
}
