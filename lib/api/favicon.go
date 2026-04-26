package api

import (
	_ "embed"
	"net/http"

	"github.com/icco/gutil/logging"
	"go.uber.org/zap"
)

// faviconSVG is the source-of-truth brand mark; avatarPNG is rasterized from it.
//
//go:embed favicon.svg
var faviconSVG []byte

// avatarPNG is favicon.svg rasterized to 1024x1024 (regen with qlmanage).
//
//go:embed avatar.png
var avatarPNG []byte

// handleFavicon serves the embedded SVG favicon.
func handleFavicon(w http.ResponseWriter, r *http.Request) {
	writeAsset(w, r, "image/svg+xml", faviconSVG)
}

// handleAvatar serves the embedded 1024x1024 brand PNG.
func handleAvatar(w http.ResponseWriter, r *http.Request) {
	writeAsset(w, r, "image/png", avatarPNG)
}

// writeAsset writes a static asset with sniff and cache headers, logging any write error.
func writeAsset(w http.ResponseWriter, r *http.Request, contentType string, body []byte) {
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "public, max-age=86400")
	if _, err := w.Write(body); err != nil {
		logging.FromContext(r.Context()).Errorw("write asset", "type", contentType, zap.Error(err))
	}
}
