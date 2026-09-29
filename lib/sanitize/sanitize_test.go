package sanitize

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"go.icco.me/odesli"
)

// TestNewDefaultsHTTPClient confirms New attaches a 5 s timeout client
// when WithHTTPClient is omitted.
func TestNewDefaultsHTTPClient(t *testing.T) {
	s := New(nil)
	if s.hc == nil {
		t.Fatal("expected default *http.Client, got nil")
	}
	if s.hc.Timeout != defaultHTTPTimeout {
		t.Errorf("default timeout: got %v want %v", s.hc.Timeout, defaultHTTPTimeout)
	}
}

func TestFindURLs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		text string
		want []string
	}{
		{"no links", nil},
		{"<https://example.com/a>, https://example.com/b!", []string{"https://example.com/a", "https://example.com/b"}},
		{"[wiki](https://en.wikipedia.org/wiki/Go_(programming_language)).", []string{"https://en.wikipedia.org/wiki/Go_(programming_language)"}},
		{"https://example.com/a[1] https://example.com/{x}", []string{"https://example.com/a[1]", "https://example.com/{x}"}},
		{"HTTPS://EXAMPLE.COM/path", []string{"HTTPS://EXAMPLE.COM/path"}},
		{"`https://example.com/`", []string{"https://example.com/"}},
	} {
		t.Run(tc.text, func(t *testing.T) {
			if got := FindURLs(tc.text); !slices.Equal(got, tc.want) {
				t.Errorf("FindURLs = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMusicRouting(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"https://open.spotify.com:443/track/abc",
		"https://OPEN.SPOTIFY.COM./track/abc",
	} {
		t.Run(raw, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.URL.Query().Get("url"); got != raw {
					t.Errorf("lookup = %q, want %q", got, raw)
				}
				_, _ = fmt.Fprint(w, `{"pageUrl":"https://song.link/test"}`)
			}))
			t.Cleanup(srv.Close)
			s := New(odesli.New(odesli.WithBaseURL(srv.URL), odesli.WithHTTPClient(srv.Client())))
			got, err := s.URL(t.Context(), raw)
			if err != nil || got != "https://song.link/test" {
				t.Errorf("URL = %q, %v", got, err)
			}
		})
	}
	if isMusicHost("notspotify.com") || isMusicHost("open.spotify.com.example.com") {
		t.Error("matched a lookalike music host")
	}
}

func TestURLCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	const raw = "https://open.spotify.com/track/abc"
	got, err := New(nil).URL(ctx, raw)
	if got != raw || !errors.Is(err, context.Canceled) {
		t.Errorf("URL = %q, %v; want original URL and cancellation", got, err)
	}
}

// TestWithHTTPClient confirms the option overrides the default client.
func TestWithHTTPClient(t *testing.T) {
	custom := &http.Client{Timeout: 42 * time.Second}
	s := New(nil, WithHTTPClient(custom))
	if s.hc != custom {
		t.Errorf("WithHTTPClient did not install custom client")
	}
}

// TestURLRoutesUnknownHostThroughCareen smoke-tests that Sanitizer.URL
// hands non-music links to careen.Clean.
func TestURLRoutesUnknownHostThroughCareen(t *testing.T) {
	s := New(nil)
	got, err := s.URL(context.Background(), "https://example.com/foo?utm_source=bar")
	if err != nil {
		t.Fatalf("URL: %v", err)
	}
	const want = "https://example.com/foo"
	if got != want {
		t.Errorf("URL: got %q, want %q", got, want)
	}
}
