// Package api exposes an HTTP API for sanitizing URLs.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/icco/linkbot/lib/logctx"
	"github.com/icco/linkbot/lib/sanitize"
)

// Options configures the HTTP router. Sanitizer and Logger are required;
// DiscordClientID is optional and, when set, lets the landing page render a
// clickable Discord invite link.
type Options struct {
	Sanitizer       *sanitize.Sanitizer
	Logger          *slog.Logger
	DiscordClientID string
}

// Router returns an http.Handler with all routes mounted. The base logger is
// attached to every request's context via logctx, so handlers retrieve it
// from ctx instead of carrying it on a server struct.
func Router(opts Options) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(loggerMiddleware(opts.Logger))
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))

	r.Get("/", handleIndex(opts.DiscordClientID))
	r.Get("/healthcheck", handleHealthcheck)
	r.Post("/sanitize", handleSanitize(opts.Sanitizer))
	return r
}

// loggerMiddleware decorates the request context with a slog.Logger that
// carries the chi request ID, then defers to the next handler.
func loggerMiddleware(base *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			log := base
			if id, ok := r.Context().Value(middleware.RequestIDKey).(string); ok && id != "" {
				log = log.With("request_id", id)
			}
			next.ServeHTTP(w, r.WithContext(logctx.New(r.Context(), log)))
		})
	}
}

// sanitizeRequest is the JSON body accepted by POST /sanitize.
type sanitizeRequest struct {
	URL string `json:"url"`
}

// sanitizeResponse is the JSON body returned by POST /sanitize. It echoes the
// input URL alongside the sanitized form and a Changed flag so callers do not
// have to compare the strings themselves.
type sanitizeResponse struct {
	URL       string `json:"url"`
	Sanitized string `json:"sanitized"`
	Changed   bool   `json:"changed"`
}

// handleSanitize returns an http.HandlerFunc that reads a sanitizeRequest,
// runs it through san, and writes a sanitizeResponse. Errors are reported
// through writeError so they are also logged.
func handleSanitize(san *sanitize.Sanitizer) http.HandlerFunc {
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

// handleHealthcheck is the liveness/readiness endpoint; it always returns 200
// with a tiny JSON body so load balancers and orchestrators can probe the
// service without needing any external dependencies to be reachable.
func handleHealthcheck(w http.ResponseWriter, r *http.Request) {
	writeJSON(r.Context(), w, http.StatusOK, map[string]string{"status": "ok"})
}

// writeJSON marshals body and writes it with the given status. Encode errors
// are logged through the request-scoped logger.
func writeJSON(ctx context.Context, w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil && !errors.Is(err, http.ErrHandlerTimeout) {
		logctx.From(ctx).Error("write json", "error", err)
	}
}

// writeError logs err and emits a JSON error body. Taking an error (rather
// than a bare string) keeps the function type-safe and ensures every client
// error is also recorded in the server log.
func writeError(r *http.Request, w http.ResponseWriter, status int, err error) {
	logctx.From(r.Context()).Error("http error",
		"status", status,
		"method", r.Method,
		"path", r.URL.Path,
		"error", err,
	)
	writeJSON(r.Context(), w, status, map[string]string{"error": err.Error()})
}
