package api

import (
	_ "embed"
	"net/http"
)

//go:embed favicon.svg
var faviconSVG []byte

// avatarPNG is favicon.svg rasterized to 1024x1024 for the Discord
// avatar and the landing-page brand mark. Regenerate with:
//
//	qlmanage -t -s 1024 -o /tmp lib/api/favicon.svg && \
//	  cp /tmp/favicon.svg.png lib/api/avatar.png
//
//go:embed avatar.png
var avatarPNG []byte

// handleFavicon serves the embedded SVG favicon.
func handleFavicon(w http.ResponseWriter, _ *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "image/svg+xml")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(faviconSVG)
}

// handleAvatar serves the embedded 1024x1024 brand PNG.
func handleAvatar(w http.ResponseWriter, _ *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "image/png")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(avatarPNG)
}
