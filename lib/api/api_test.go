package api_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
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
