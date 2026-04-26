// Package api exposes an HTTP API for sanitizing URLs.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/icco/gutil/logging"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.uber.org/zap"

	"github.com/icco/linkbot/lib/sanitize"
)

// serverName is the otelhttp span/metric scope.
const serverName = "linkbot"

// sanitizer is the slice of *sanitize.Sanitizer the handlers need; the
// interface lets tests inject a fake.
type sanitizer interface {
	URL(ctx context.Context, raw string) (string, error)
}

// Options configures the HTTP router. MetricsHandler, when set, is
// mounted at GET /metrics.
type Options struct {
	Sanitizer       sanitizer
	Logger          *zap.SugaredLogger
	DiscordClientID string
	MetricsHandler  http.Handler
}

// Router returns the linkbot HTTP handler. otelhttp wraps the chi
// router (skipping /metrics so scrapes don't self-instrument); inside,
// gutil's Middleware injects the per-request logger and routeTag
// stamps the chi route pattern onto otelhttp's metric labeler.
func Router(opts Options) http.Handler {
	r := chi.NewRouter()
	r.Use(logging.Middleware(opts.Logger.Desugar()))
	r.Use(routeTag)
	r.Use(middleware.Timeout(30 * time.Second))

	r.Get("/", handleIndex(opts.DiscordClientID))
	r.Get("/healthcheck", handleHealthcheck)
	r.Post("/sanitize", handleSanitize(opts.Sanitizer))

	if opts.MetricsHandler != nil {
		r.Method(http.MethodGet, "/metrics", opts.MetricsHandler)
	}

	return otelhttp.NewHandler(r, serverName,
		otelhttp.WithFilter(func(req *http.Request) bool {
			return req.URL.Path != "/metrics"
		}),
	)
}

// routeTag tags the otelhttp metric with the chi route pattern so
// http.server.request.duration buckets by route, not raw URL.
func routeTag(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		labeler, ok := otelhttp.LabelerFromContext(r.Context())
		if !ok {
			return
		}
		if pattern := chi.RouteContext(r.Context()).RoutePattern(); pattern != "" {
			labeler.Add(semconv.HTTPRoute(pattern))
		}
	})
}

// sanitizeRequest is the JSON body accepted by POST /sanitize.
type sanitizeRequest struct {
	URL string `json:"url"`
}

// sanitizeResponse is the JSON body returned by POST /sanitize.
type sanitizeResponse struct {
	URL       string `json:"url"`
	Sanitized string `json:"sanitized"`
	Changed   bool   `json:"changed"`
}

// handleSanitize handles POST /sanitize.
func handleSanitize(san sanitizer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req sanitizeRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&req); err != nil {
			writeError(r, w, http.StatusBadRequest, fmt.Errorf("invalid json body: %w", err))
			return
		}
		if req.URL == "" {
			writeError(r, w, http.StatusBadRequest, errors.New("url is required"))
			return
		}

		clean, err := san.URL(r.Context(), req.URL)
		if err != nil {
			writeError(r, w, http.StatusBadGateway, fmt.Errorf("sanitize: %w", err))
			return
		}

		writeJSON(r.Context(), w, http.StatusOK, sanitizeResponse{
			URL:       req.URL,
			Sanitized: clean,
			Changed:   sanitize.Changed(req.URL, clean),
		})
	}
}

// handleHealthcheck is the liveness/readiness probe.
func handleHealthcheck(w http.ResponseWriter, r *http.Request) {
	writeJSON(r.Context(), w, http.StatusOK, map[string]string{"status": "ok"})
}

// writeJSON writes body as JSON with status; encode errors are logged
// to the request-scoped logger.
func writeJSON(ctx context.Context, w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil && !errors.Is(err, http.ErrHandlerTimeout) {
		logging.FromContext(ctx).Errorw("write json", zap.Error(err))
	}
}

// writeError logs err and emits a JSON error body. The error type
// (vs string) preserves wrapped causes.
func writeError(r *http.Request, w http.ResponseWriter, status int, err error) {
	logging.FromContext(r.Context()).Errorw("http error",
		"status", status,
		"method", r.Method,
		"path", r.URL.Path,
		zap.Error(err),
	)
	writeJSON(r.Context(), w, status, map[string]string{"error": err.Error()})
}
