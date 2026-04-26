package sanitize

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// TestNewDefaultsHTTPClient confirms New attaches a 5 s timeout client
// when WithHTTPClient is omitted.
func TestNewDefaultsHTTPClient(t *testing.T) {
	s := New(nil, nil)
	if s.hc == nil {
		t.Fatal("expected default *http.Client, got nil")
	}
	if s.hc.Timeout != defaultHTTPTimeout {
		t.Errorf("default timeout: got %v want %v", s.hc.Timeout, defaultHTTPTimeout)
	}
}

// TestWithHTTPClient confirms the option overrides the default client.
func TestWithHTTPClient(t *testing.T) {
	custom := &http.Client{Timeout: 42 * time.Second}
	s := New(nil, nil, WithHTTPClient(custom))
	if s.hc != custom {
		t.Errorf("WithHTTPClient did not install custom client")
	}
}

// TestURLRoutesUnknownHostThroughCareen smoke-tests that Sanitizer.URL
// hands non-music links to careen.Clean.
func TestURLRoutesUnknownHostThroughCareen(t *testing.T) {
	s := New(nil, nil)
	got, err := s.URL(context.Background(), "https://example.com/foo?utm_source=bar")
	if err != nil {
		t.Fatalf("URL: %v", err)
	}
	const want = "https://example.com/foo"
	if got != want {
		t.Errorf("URL: got %q, want %q", got, want)
	}
}
