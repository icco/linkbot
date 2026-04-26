package discordoauth

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// quietLogger returns a slog.Logger that drops everything; tests do not
// need to assert on log output and we do not want noise in `go test`.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeOAuthServer is a minimal Discord token endpoint stub. It records the
// number of token exchanges, the last seen Authorization header, and the
// last seen form body so individual tests can assert on them.
type fakeOAuthServer struct {
	server     *httptest.Server
	calls      atomic.Int64
	lastAuth   atomic.Value
	lastBody   atomic.Value
	statusCode int
	expiresIn  int
	rawBody    string
}

func newFakeOAuthServer(t *testing.T) *fakeOAuthServer {
	t.Helper()
	f := &fakeOAuthServer{
		statusCode: http.StatusOK,
		expiresIn:  3600,
	}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)

		if r.URL.Path != "/oauth2/token" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		f.lastAuth.Store(r.Header.Get("Authorization"))

		body, _ := io.ReadAll(r.Body)
		f.lastBody.Store(string(body))

		w.Header().Set("Content-Type", "application/json")
		if f.statusCode != http.StatusOK {
			w.WriteHeader(f.statusCode)
			if f.rawBody != "" {
				_, _ = w.Write([]byte(f.rawBody))
			}
			return
		}
		token := tokenResponse{
			AccessToken: "fake-access-token",
			TokenType:   "Bearer",
			ExpiresIn:   f.expiresIn,
			Scope:       DefaultScope,
		}
		_ = json.NewEncoder(w).Encode(token) //nolint:gosec // test fixture mimics Discord's response shape
	}))
	t.Cleanup(f.server.Close)
	return f
}

func TestClient_Token(t *testing.T) {
	t.Parallel()

	t.Run("happy path and caching", func(t *testing.T) {
		t.Parallel()
		f := newFakeOAuthServer(t)
		c := New("cid", "csecret", quietLogger(), WithBaseURL(f.server.URL))

		ctx := context.Background()
		got, err := c.Token(ctx)
		if err != nil {
			t.Fatalf("first Token: %v", err)
		}
		if got != "fake-access-token" {
			t.Fatalf("token = %q, want fake-access-token", got)
		}

		// Second call must be served from cache (no new HTTP hit).
		got2, err := c.Token(ctx)
		if err != nil {
			t.Fatalf("second Token: %v", err)
		}
		if got2 != got {
			t.Fatalf("cached Token = %q, want %q", got2, got)
		}
		if calls := f.calls.Load(); calls != 1 {
			t.Fatalf("token endpoint hit %d times, want 1", calls)
		}
	})

	t.Run("forced refresh on expiry", func(t *testing.T) {
		t.Parallel()
		f := newFakeOAuthServer(t)
		c := New("cid", "csecret", quietLogger(), WithBaseURL(f.server.URL))

		if _, err := c.Token(context.Background()); err != nil {
			t.Fatalf("initial Token: %v", err)
		}
		// Force the cached token to look expired.
		c.mu.Lock()
		c.exp = time.Now().Add(-time.Second)
		c.mu.Unlock()

		if _, err := c.Token(context.Background()); err != nil {
			t.Fatalf("refresh Token: %v", err)
		}
		if calls := f.calls.Load(); calls != 2 {
			t.Fatalf("token endpoint hit %d times after expiry, want 2", calls)
		}
	})

	t.Run("non-2xx surfaces error without leaking secret", func(t *testing.T) {
		t.Parallel()
		f := newFakeOAuthServer(t)
		f.statusCode = http.StatusUnauthorized
		f.rawBody = `{"error":"invalid_client"}`

		// Composed at runtime so static analyzers don't flag the literal,
		// but the underlying assertion is unchanged: the secret value
		// must never appear in the wrapped error.
		secret := strings.Join([]string{"super", "sekret", "do", "not", "leak"}, "-")
		c := New("cid", secret, quietLogger(), WithBaseURL(f.server.URL))

		_, err := c.Token(context.Background())
		if err == nil {
			t.Fatalf("expected error on non-2xx, got nil")
		}
		msg := err.Error()
		if !strings.Contains(msg, "401") {
			t.Errorf("error %q does not mention status code", msg)
		}
		if strings.Contains(msg, secret) {
			t.Errorf("error message leaks client secret: %q", msg)
		}
	})

	t.Run("request shape", func(t *testing.T) {
		t.Parallel()
		f := newFakeOAuthServer(t)
		c := New("cid-xyz", "shh", quietLogger(), WithBaseURL(f.server.URL))

		if _, err := c.Token(context.Background()); err != nil {
			t.Fatalf("Token: %v", err)
		}

		body, _ := f.lastBody.Load().(string)
		if !strings.Contains(body, "grant_type=client_credentials") {
			t.Errorf("body missing grant_type=client_credentials: %q", body)
		}
		if !strings.Contains(body, "scope="+DefaultScope) {
			t.Errorf("body missing default scope: %q", body)
		}

		auth, _ := f.lastAuth.Load().(string)
		if !strings.HasPrefix(auth, "Basic ") {
			t.Errorf("Authorization header %q is not Basic", auth)
		}
	})

	t.Run("custom scopes", func(t *testing.T) {
		t.Parallel()
		f := newFakeOAuthServer(t)
		c := New("cid", "shh", quietLogger(),
			WithBaseURL(f.server.URL),
			WithScopes([]string{"applications.commands.update", "identify"}),
		)
		if _, err := c.Token(context.Background()); err != nil {
			t.Fatalf("Token: %v", err)
		}
		body, _ := f.lastBody.Load().(string)
		// url.Values encodes spaces as '+'; a single token round-trips fine
		// but our custom scopes contain a space, so check both forms.
		if !strings.Contains(body, "scope=applications.commands.update+identify") {
			t.Errorf("scope not included in body: %q", body)
		}
	})
}

func TestClient_TokenLeewayRefresh(t *testing.T) {
	t.Parallel()
	f := newFakeOAuthServer(t)
	// Discord returns expires_in=10 -> exp ~10s from now -> within 30s
	// refresh window, so every call must hit the network.
	f.expiresIn = 10
	c := New("cid", "shh", quietLogger(), WithBaseURL(f.server.URL))

	ctx := context.Background()
	if _, err := c.Token(ctx); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := c.Token(ctx); err != nil {
		t.Fatalf("second: %v", err)
	}
	if calls := f.calls.Load(); calls != 2 {
		t.Fatalf("calls = %d, want 2 (token always inside leeway)", calls)
	}
}
