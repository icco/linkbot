// Package api exposes an HTTP API for sanitizing URLs.
package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/icco/linkbot/lib/sanitize"
)

// Server is the HTTP API.
type Server struct {
	san *sanitize.Sanitizer
	log *slog.Logger
}

// New constructs a Server.
func New(san *sanitize.Sanitizer, log *slog.Logger) *Server {
	return &Server{san: san, log: log}
}

// Router returns the chi router with all routes mounted.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))

	r.Get("/healthz", s.handleHealthz)
	r.Post("/sanitize", s.handleSanitize)
	return r
}

type sanitizeRequest struct {
	URL string `json:"url"`
}

type sanitizeResponse struct {
	URL       string `json:"url"`
	Sanitized string `json:"sanitized"`
	Changed   bool   `json:"changed"`
}

func (s *Server) handleSanitize(w http.ResponseWriter, r *http.Request) {
	var req sanitizeRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	if req.URL == "" {
		writeError(w, http.StatusBadRequest, "url is required")
		return
	}

	clean, err := s.san.URL(r.Context(), req.URL)
	if err != nil {
		s.log.Warn("sanitize failed", "url", req.URL, "error", err)
		writeError(w, http.StatusBadGateway, "could not sanitize url")
		return
	}

	writeJSON(w, http.StatusOK, sanitizeResponse{
		URL:       req.URL,
		Sanitized: clean,
		Changed:   sanitize.Changed(req.URL, clean),
	})
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil && !errors.Is(err, http.ErrHandlerTimeout) {
		slog.Error("write json", "error", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
