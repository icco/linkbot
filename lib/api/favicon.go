package api

import (
	_ "embed"
	"net/http"
)

//go:embed favicon.svg
var faviconSVG []byte

// handleFavicon serves the embedded SVG favicon.
func handleFavicon(w http.ResponseWriter, _ *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "image/svg+xml")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(faviconSVG)
}
