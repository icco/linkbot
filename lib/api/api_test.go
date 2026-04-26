package api_test

import (
	"bytes"
	"context"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.uber.org/zap"

	"github.com/icco/linkbot/lib/api"
)

// stubSanitizer is a no-op for /sanitize so the route returns 200.
type stubSanitizer struct{}

// URL returns raw unchanged.
func (stubSanitizer) URL(_ context.Context, raw string) (string, error) {
	return raw, nil
}

// TestMetricsEndpoint asserts otelhttp's HTTP server histogram lands
// in /metrics tagged with the chi route pattern.
func TestMetricsEndpoint(t *testing.T) {
	reg := prometheus.NewRegistry()
	exporter, err := otelprom.New(otelprom.WithRegisterer(reg))
	if err != nil {
		t.Fatalf("otelprom.New: %v", err)
	}
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exporter))
	otel.SetMeterProvider(mp)
	t.Cleanup(func() {
		if err := mp.Shutdown(context.Background()); err != nil {
			t.Logf("meter provider shutdown: %v", err)
		}
	})

	h := api.Router(api.Options{
		Sanitizer:      stubSanitizer{},
		Logger:         zap.NewNop().Sugar(),
		MetricsHandler: promhttp.HandlerFor(reg, promhttp.HandlerOpts{}),
	})

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	body := bytes.NewReader([]byte(`{"url":"https://example.com/track/1"}`))
	resp, err := http.Post(srv.URL+"/sanitize", "application/json", body)
	if err != nil {
		t.Fatalf("post sanitize: %v", err)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Logf("drain sanitize body: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Logf("close sanitize body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sanitize status = %d, want 200", resp.StatusCode)
	}

	metricsResp, err := http.Get(srv.URL + "/metrics") //nolint:noctx // test
	if err != nil {
		t.Fatalf("get metrics: %v", err)
	}
	defer func() {
		if err := metricsResp.Body.Close(); err != nil {
			t.Logf("close metrics body: %v", err)
		}
	}()
	if metricsResp.StatusCode != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200", metricsResp.StatusCode)
	}
	raw, err := io.ReadAll(metricsResp.Body)
	if err != nil {
		t.Fatalf("read metrics body: %v", err)
	}
	text := string(raw)

	for _, want := range []string{
		"http_server_request_duration_seconds",
		`http_route="/sanitize"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics body missing %q\nbody:\n%s", want, text)
		}
	}
}

// TestIndexSecurityHeaders asserts the landing page sets the expected
// security headers, that the per-request CSP nonce is plumbed into the
// rendered <script>/<style> tags, and that the JSON API routes don't
// pick up the HTML-only headers.
func TestIndexSecurityHeaders(t *testing.T) {
	h := api.Router(api.Options{
		Sanitizer:       stubSanitizer{},
		Logger:          zap.NewNop().Sugar(),
		DiscordClientID: "123456789012345678",
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/") //nolint:noctx // test
	if err != nil {
		t.Fatalf("get /: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("close index body: %v", err)
		}
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read index body: %v", err)
	}

	csp := resp.Header.Get("Content-Security-Policy")
	for _, want := range []string{
		"frame-ancestors 'none'",
		"connect-src 'self' https://reportd.natwelch.com",
		"script-src 'self' 'nonce-",
		"report-uri https://reportd.natwelch.com/report/linkbot",
	} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP missing %q; got %q", want, csp)
		}
	}

	for header, want := range map[string]string{
		"Reporting-Endpoints":       `default="https://reportd.natwelch.com/reporting/linkbot"`,
		"Referrer-Policy":           "strict-origin-when-cross-origin",
		"X-Content-Type-Options":    "nosniff",
		"X-Frame-Options":           "DENY",
		"Strict-Transport-Security": "max-age=31536000; includeSubDomains",
	} {
		if got := resp.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}

	m := regexp.MustCompile(`'nonce-([^']+)'`).FindStringSubmatch(csp)
	if len(m) != 2 {
		t.Fatalf("no nonce in CSP: %q", csp)
	}
	nonce := m[1]
	// html/template entity-encodes some base64 chars (e.g. '+' -> '&#43;');
	// browsers decode those before CSP nonce matching, so we do too.
	bodyStr := html.UnescapeString(string(body))
	if !strings.Contains(bodyStr, `<style nonce="`+nonce+`">`) {
		t.Errorf("style nonce attr missing %q", nonce)
	}
	if !strings.Contains(bodyStr, `<script type="module" nonce="`+nonce+`">`) {
		t.Errorf("script nonce attr missing %q", nonce)
	}
	if !strings.Contains(bodyStr, "Add to Discord") {
		t.Errorf("invite CTA missing from body")
	}
	if !strings.Contains(bodyStr, `<link rel="icon"`) {
		t.Errorf("favicon link missing from body")
	}
	if !strings.Contains(bodyStr, `src="/avatar.png"`) {
		t.Errorf("brand avatar img missing from body")
	}

	hc, err := http.Get(srv.URL + "/healthcheck") //nolint:noctx // test
	if err != nil {
		t.Fatalf("get /healthcheck: %v", err)
	}
	defer func() {
		if err := hc.Body.Close(); err != nil {
			t.Logf("close healthcheck body: %v", err)
		}
	}()
	for _, header := range []string{
		"Content-Security-Policy",
		"Reporting-Endpoints",
		"X-Frame-Options",
		"Strict-Transport-Security",
	} {
		if got := hc.Header.Get(header); got != "" {
			t.Errorf("/healthcheck unexpectedly set %s = %q", header, got)
		}
	}
}

// TestStaticAssets verifies the embedded brand assets are served with the
// correct content type, aren't empty, and pick up the same security
// headers as the landing page.
func TestStaticAssets(t *testing.T) {
	h := api.Router(api.Options{
		Sanitizer: stubSanitizer{},
		Logger:    zap.NewNop().Sugar(),
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	wantHeaders := map[string]string{
		"Reporting-Endpoints":       `default="https://reportd.natwelch.com/reporting/linkbot"`,
		"Referrer-Policy":           "strict-origin-when-cross-origin",
		"X-Content-Type-Options":    "nosniff",
		"X-Frame-Options":           "DENY",
		"Strict-Transport-Security": "max-age=31536000; includeSubDomains",
	}

	for path, wantType := range map[string]string{
		"/favicon.svg": "image/svg+xml",
		"/avatar.png":  "image/png",
	} {
		resp, err := http.Get(srv.URL + path) //nolint:noctx // test
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if err := resp.Body.Close(); err != nil {
			t.Logf("close %s body: %v", path, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s status = %d, want 200", path, resp.StatusCode)
		}
		if got := resp.Header.Get("Content-Type"); got != wantType {
			t.Errorf("%s Content-Type = %q, want %q", path, got, wantType)
		}
		if len(body) == 0 {
			t.Errorf("%s body is empty", path)
		}
		if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Errorf("%s CSP missing frame-ancestors; got %q", path, csp)
		}
		for header, want := range wantHeaders {
			if got := resp.Header.Get(header); got != want {
				t.Errorf("%s %s = %q, want %q", path, header, got, want)
			}
		}
	}
}
