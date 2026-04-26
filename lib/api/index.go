package api

import (
	_ "embed"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/icco/gutil/logging"
	"github.com/unrolled/secure"
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
	Nonce     string
}

// indexCSP is the landing-page CSP; $NONCE is expanded per-request by unrolled/secure.
var indexCSP = strings.Join([]string{
	"default-src 'self'",
	"script-src 'self' $NONCE https://unpkg.com",
	"style-src 'self' $NONCE",
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

// indexSecure is the security-headers middleware for HTML routes.
// ForceSTSHeader: true because linkbot sits behind a TLS-terminating proxy.
var indexSecure = secure.New(secure.Options{
	ContentSecurityPolicy: indexCSP,
	ReferrerPolicy:        "strict-origin-when-cross-origin",
	ContentTypeNosniff:    true,
	FrameDeny:             true,
	PermissionsPolicy:     "camera=(), microphone=(), geolocation=(), interest-cohort=()",
	STSSeconds:            31536000,
	STSIncludeSubdomains:  true,
	ForceSTSHeader:        true,
})

// reportingEndpointsHeader sets the Reporting-Endpoints header (not handled by unrolled/secure).
func reportingEndpointsHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Reporting-Endpoints", indexReportingEndpoints)
		next.ServeHTTP(w, r)
	})
}

// handleIndex renders the landing page; an invite button is added when discordClientID is set.
func handleIndex(discordClientID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data := indexData{
			Nonce: secure.CSPNonce(r.Context()),
		}
		if discordClientID != "" {
			data.InviteURL = inviteURL(discordClientID)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
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
