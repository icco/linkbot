package careen

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}

// TestCleanRules walks one URL per non-HTTP-following rule plus a few
// default-strip cases.
func TestCleanRules(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"google search", "https://www.google.com/search?q=hello+world&utm_source=bar&pws=1", "https://www.google.com/search?pws=0&q=hello+world&udm=14"},
		{"google ccTLD com.au", "https://www.google.com.au/search?q=foo&hl=en", "https://www.google.com.au/search?pws=0&q=foo&udm=14"},
		{"google co.uk falls through (co not in TLD list)", "https://www.google.co.uk/search?q=foo&hl=en", "https://www.google.co.uk/search"},
		{"google workspace", "https://docs.google.com/document/d/abc?tab=t.0&authuser=1&utm=x", "https://docs.google.com/document/d/abc?authuser=1&tab=t.0"},
		{"amazon ref tail", "https://www.amazon.com/Some-Product/dp/B000TEST/ref=cm_sw_r_other?utm=x&pf=1", "https://www.amazon.com/Some-Product/dp/B000TEST"},
		{"amazon co.uk", "https://www.amazon.co.uk/dp/B000/ref=foo", "https://www.amazon.co.uk/dp/B000"},
		{"reddit", "https://www.reddit.com/r/golang/comments/x/post?utm_source=share&context=3", "https://www.reddit.com/r/golang/comments/x/post"},
		{"youtube", "https://www.youtube.com/watch?v=dQw4w9WgXcQ&t=42&feature=share&pp=tracking", "https://www.youtube.com/watch?t=42&v=dQw4w9WgXcQ"},
		{"youtu.be", "https://youtu.be/dQw4w9WgXcQ?t=42&feature=share", "https://youtu.be/dQw4w9WgXcQ?t=42"},
		{"twitch", "https://www.twitch.tv/somestreamer/clip/abc?t=01h02m&filter=clips", "https://www.twitch.tv/somestreamer/clip/abc?t=01h02m"},
		{"nytimes", "https://www.nytimes.com/2026/01/01/world/article.html?unlocked_article_code=abcd&smid=share", "https://www.nytimes.com/2026/01/01/world/article.html?unlocked_article_code=abcd"},
		{"admin.cloud.microsoft", "https://admin.cloud.microsoft/?ref=AdminPortal&route=foo", "https://admin.cloud.microsoft/?ref=AdminPortal&route=foo"},
		{"unknown host strips query+fragment", "https://example.com/some/path?utm_source=foo&utm_medium=bar#frag", "https://example.com/some/path"},
		{"non-http scheme passes through", "mailto:someone@example.com?subject=hi", "mailto:someone@example.com?subject=hi"},
		{"uppercase host matches rule", "https://WWW.YOUTUBE.COM/watch?v=abc&utm=x", "https://WWW.YOUTUBE.COM/watch?v=abc"},

		{"paywall apex routes through archive", "https://wsj.com/article?utm_source=foo", archivePrefix + "https://wsj.com/article"},
		{"paywall subdomain routes through archive", "https://www.bloomberg.com/news/x?utm=y", archivePrefix + "https://www.bloomberg.com/news/x"},
		{"paywall preserves a kept param", "https://www.washingtonpost.com/article?id=1&utm_source=share", archivePrefix + "https://www.washingtonpost.com/article"},
		{"nytimes excluded from paywall list", "https://www.nytimes.com/2026/01/01/world/article.html?unlocked_article_code=abcd&smid=share", "https://www.nytimes.com/2026/01/01/world/article.html?unlocked_article_code=abcd"},
		{"keep_all rule (admin.cloud) skips archive routing", "https://admin.cloud.microsoft/?ref=AdminPortal", "https://admin.cloud.microsoft/?ref=AdminPortal"},
		{"already at archive.ph passes through", "https://archive.ph/https://wsj.com/article?utm=x", "https://archive.ph/https://wsj.com/article?utm=x"},
		{"already at archive.today passes through", "https://archive.today/https://wsj.com/article", "https://archive.today/https://wsj.com/article"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Clean(context.Background(), tc.in, http.DefaultClient)
			if err != nil {
				t.Fatalf("Clean(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("Clean(%q):\n got: %q\nwant: %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestAppleNews drives the apple.news strategy against an httptest server
// that emits the redirectToUrlAfterTimeout snippet.
func TestAppleNews(t *testing.T) {
	const target = "https://www.example.com/article?utm_source=foo"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `<script>redirectToUrlAfterTimeout(%q)</script>`, target)
	}))
	defer srv.Close()

	c := &cleaner{http: srv.Client(), maxHops: defaultMaxHops}
	got, err := c.appleNews()(context.Background(), mustParse(t, srv.URL))
	if err != nil {
		t.Fatalf("appleNews: %v", err)
	}
	const want = "https://www.example.com/article"
	if got != want {
		t.Errorf("appleNews: got %q, want %q", got, want)
	}
}

// TestAppleNewsNoMatch verifies graceful pass-through when the wrapper
// page does not contain the redirect token.
func TestAppleNewsNoMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `<html>nothing useful</html>`)
	}))
	defer srv.Close()

	c := &cleaner{http: srv.Client(), maxHops: defaultMaxHops}
	u := mustParse(t, srv.URL+"/wrap")
	got, err := c.appleNews()(context.Background(), u)
	if err != nil {
		t.Fatalf("appleNews: %v", err)
	}
	if got != u.String() {
		t.Errorf("appleNews: got %q, want pass-through %q", got, u.String())
	}
}

// TestFollowRedirect exercises a 302 → real URL hop and confirms the
// destination flows through the engine.
func TestFollowRedirect(t *testing.T) {
	const target = "https://www.example.com/landing?utm_source=googleapp"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target)
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	c := &cleaner{http: srv.Client(), maxHops: defaultMaxHops}
	got, err := c.followRedirect()(context.Background(), mustParse(t, srv.URL+"/short"))
	if err != nil {
		t.Fatalf("followRedirect: %v", err)
	}
	const want = "https://www.example.com/landing"
	if got != want {
		t.Errorf("followRedirect: got %q, want %q", got, want)
	}
}

// TestFollowRedirectNon3xx asserts a 200 OK leaves the URL untouched.
func TestFollowRedirectNon3xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := &cleaner{http: srv.Client(), maxHops: defaultMaxHops}
	u := mustParse(t, srv.URL+"/short")
	got, err := c.followRedirect()(context.Background(), u)
	if err != nil {
		t.Fatalf("followRedirect: %v", err)
	}
	if got != u.String() {
		t.Errorf("followRedirect: got %q, want pass-through %q", got, u.String())
	}
}

// TestFollowRedirectRelativeLocation covers Location: /next being
// resolved against the request URL before recursion.
func TestFollowRedirectRelativeLocation(t *testing.T) {
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

	c := &cleaner{http: srv.Client(), maxHops: defaultMaxHops}
	got, err := c.followRedirect()(context.Background(), mustParse(t, srv.URL+"/short"))
	if err != nil {
		t.Fatalf("followRedirect: %v", err)
	}
	want := srv.URL + "/landing"
	if got != want {
		t.Errorf("followRedirect: got %q, want %q", got, want)
	}
}

// TestRecursionCap pre-loads the hop counter to the cap and confirms the
// engine returns the URL untouched without invoking any strategy.
func TestRecursionCap(t *testing.T) {
	c := &cleaner{http: http.DefaultClient, maxHops: 3, hop: 3}
	const raw = "https://www.google.com/search?q=foo&utm_source=bar"
	got, err := c.clean(context.Background(), mustParse(t, raw))
	if err != nil {
		t.Fatalf("clean: %v", err)
	}
	if got != raw {
		t.Errorf("clean at cap: got %q, want %q", got, raw)
	}
}

// TestRecursionCapViaRedirectChain pins a self-redirecting server and
// confirms the engine bounds the hits.
func TestRecursionCapViaRedirectChain(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Location", r.URL.String())
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	c := &cleaner{http: srv.Client(), maxHops: defaultMaxHops}
	got, err := c.followRedirect()(context.Background(), mustParse(t, srv.URL+"/loop"))
	if err != nil {
		t.Fatalf("followRedirect: %v", err)
	}
	if hits > defaultMaxHops {
		t.Errorf("redirect loop should be capped at %d hits, got %d", defaultMaxHops, hits)
	}
	if !strings.HasPrefix(got, srv.URL) {
		t.Errorf("expected URL on the test server after cap, got %q", got)
	}
}
