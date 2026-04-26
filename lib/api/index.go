package api

import (
	_ "embed"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/icco/gutil/logging"
	"go.uber.org/zap"
)

// invitePermissions = Send Messages | Read Message History.
const invitePermissions = (1 << 11) | (1 << 16)

// reportdService is the path segment used on reportd.natwelch.com endpoints.
const reportdService = "linkbot"

// reportdOrigin is the trusted origin for analytics and browser report ingestion.
const reportdOrigin = "https://reportd.natwelch.com"

//go:embed index.html
var indexHTML string

// indexTemplate is parsed once so syntax errors fail at startup.
var indexTemplate = template.Must(template.New("index").Parse(indexHTML))

// indexData is the model passed to indexTemplate.
type indexData struct {
	InviteURL string
}

// indexCSP is the Content-Security-Policy applied to the landing page.
//
// 'unsafe-inline' is required for the inline <style> block and the inline
// <script type="module"> that loads web-vitals from unpkg.com; everything
// else is locked down to 'self' and the reportd ingestion origin.
var indexCSP = strings.Join([]string{
	"default-src 'self'",
	"script-src 'self' 'unsafe-inline' https://unpkg.com",
	"style-src 'self' 'unsafe-inline'",
	"img-src 'self' data:",
	"font-src 'self' data:",
	"connect-src 'self' " + reportdOrigin,
	"base-uri 'self'",
	"form-action 'self'",
	"frame-ancestors 'none'",
	"object-src 'none'",
	"upgrade-insecure-requests",
	"report-uri " + reportdOrigin + "/report/" + reportdService,
	"report-to default",
}, "; ")

// indexReportingEndpoints points the modern Reporting API at reportd.
var indexReportingEndpoints = `default="` + reportdOrigin + `/reporting/` + reportdService + `"`

// setIndexSecurityHeaders applies HTML-only security headers to w.
//
// These are set per-handler rather than as global middleware so the JSON
// API responses keep the headers they need (e.g. no CSP locking down API
// callers) and so frame-ancestors / report-uri / Reporting-Endpoints work,
// none of which can be set via <meta http-equiv>.
func setIndexSecurityHeaders(h http.Header) {
	h.Set("Content-Security-Policy", indexCSP)
	h.Set("Reporting-Endpoints", indexReportingEndpoints)
	h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), interest-cohort=()")
	h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
}

// handleIndex renders the landing page; an invite button is added when discordClientID is set.
func handleIndex(discordClientID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data := indexData{}
		if discordClientID != "" {
			data.InviteURL = inviteURL(discordClientID)
		}
		h := w.Header()
		h.Set("Content-Type", "text/html; charset=utf-8")
		setIndexSecurityHeaders(h)
		if err := indexTemplate.Execute(w, data); err != nil {
			logging.FromContext(r.Context()).Errorw("render index", zap.Error(err))
		}
	}
}

// inviteURL builds the Discord OAuth2 invite URL.
func inviteURL(clientID string) string {
	q := url.Values{}
	q.Set("client_id", clientID)
	q.Set("scope", "bot")
	q.Set("permissions", strconv.Itoa(invitePermissions))
	return "https://discord.com/oauth2/authorize?" + q.Encode()
}
