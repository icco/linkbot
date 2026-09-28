package careen

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// pinMirror forces pickArchiveMirror to return mirror until t finishes,
// so paywall-routing assertions stay deterministic across packages.
func pinMirror(t *testing.T, mirror string) {
	t.Helper()
	orig := pickArchiveMirror
	pickArchiveMirror = func() string {
		return mirror
	}
	t.Cleanup(func() {
		pickArchiveMirror = orig
	})
}

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
	pinMirror(t, "archive.ph")

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"google search", "https://www.google.com/search?q=hello+world&utm_source=bar&pws=1", "https://www.google.com/search?pws=0&q=hello+world&udm=14"},
		{"google ccTLD com.au", "https://www.google.com.au/search?q=foo&hl=en", "https://www.google.com.au/search?pws=0&q=foo&udm=14"},
		{"google apex search", "https://google.com/search?q=foo", "https://google.com/search?pws=0&q=foo&udm=14"},
		{"google root search", "https://www.google.com/?q=foo", "https://www.google.com/?pws=0&q=foo&udm=14"},
		{"google homepage", "https://www.google.com/", "https://www.google.com/"},
		{"google non-search path", "https://www.google.com/about/", "https://www.google.com/about/"},
		{"google bughunters blog unchanged", "https://bughunters.google.com/blog/scaling-memory-safety", "https://bughunters.google.com/blog/scaling-memory-safety"},
		{"google bughunters tracking stripped", "https://bughunters.google.com/blog/scaling-memory-safety?utm_source=discord", "https://bughunters.google.com/blog/scaling-memory-safety"},
		{"google subdomain search", "https://bughunters.google.com/search?q=foo", "https://bughunters.google.com/search"},
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
		{"port matches rule", "https://www.youtube.com:443/watch?v=abc&utm=x", "https://www.youtube.com:443/watch?v=abc"},
		{"trailing dot matches rule", "https://www.youtube.com./watch?v=abc&utm=x", "https://www.youtube.com./watch?v=abc"},
		{"empty query removed", "https://example.com/path?", "https://example.com/path"},
		{"lookalike redirect host", "https://notsearch.app/path?utm=x", "https://notsearch.app/path"},
		{"multiple kept values", "https://youtu.be/test?t=1&t=2&utm=x", "https://youtu.be/test?t=1&t=2"},
		{"archive with port passes through", "https://archive.ph:443/https://wsj.com/article?utm=x", "https://archive.ph:443/https://wsj.com/article?utm=x"},

		{"paywall apex routes through archive", "https://wsj.com/article?utm_source=foo", "https://archive.ph/https://wsj.com/article"},
		{"paywall subdomain routes through archive", "https://www.bloomberg.com/news/x?utm=y", "https://archive.ph/https://www.bloomberg.com/news/x"},
		{"paywall preserves a kept param", "https://www.washingtonpost.com/article?id=1&utm_source=share", "https://archive.ph/https://www.washingtonpost.com/article"},
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
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		hits++
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     http.Header{"Location": []string{r.URL.String()}},
			Body:       io.NopCloser(strings.NewReader("")),
		}, nil
	})}

	const raw = "https://search.app/loop"
	got, err := Clean(t.Context(), raw, hc)
	if err != nil {
		t.Fatalf("followRedirect: %v", err)
	}
	if hits != defaultMaxHops {
		t.Errorf("redirect loop should stop at %d hits, got %d", defaultMaxHops, hits)
	}
	if got != raw {
		t.Errorf("expected URL on the test server after cap, got %q", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type trackedBody struct {
	*strings.Reader
	closed bool
}

func (b *trackedBody) Close() error { b.closed = true; return nil }

func TestFollowRedirectLimitsBody(t *testing.T) {
	body := &trackedBody{Reader: strings.NewReader(strings.Repeat("x", bodyReadLimit+100))}
	hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     http.Header{"Location": []string{"https://example.com/article?utm_source=test"}},
			Body:       body,
		}, nil
	})}
	got, err := Clean(t.Context(), "https://search.app/short", hc)
	if err != nil || got != "https://example.com/article" {
		t.Fatalf("Clean = %q, %v", got, err)
	}
	if !body.closed || body.Len() != 100 {
		t.Errorf("body closed = %t, unread bytes = %d; want true, 100", body.closed, body.Len())
	}
}

// TestPickArchiveMirrorRotates exercises the default (random) picker:
// every returned mirror must be in archiveMirrors and, over enough
// trials, more than one mirror must show up so we know we are not
// pinned to a single domain.
func TestPickArchiveMirrorRotates(t *testing.T) {
	valid := make(map[string]struct{}, len(archiveMirrors))
	for _, m := range archiveMirrors {
		valid[m] = struct{}{}
	}

	seen := make(map[string]struct{})
	const trials = 200
	for range trials {
		m := pickArchiveMirror()
		if _, ok := valid[m]; !ok {
			t.Fatalf("pickArchiveMirror returned %q, not in archiveMirrors", m)
		}
		seen[m] = struct{}{}
	}
	if len(seen) < 2 {
		t.Errorf("pickArchiveMirror only returned %d distinct mirrors over %d trials: %v", len(seen), trials, seen)
	}
}
