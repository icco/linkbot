package api

import (
	_ "embed"
	"html/template"
	"net/http"
	"net/url"
	"strconv"

	"github.com/icco/gutil/logging"
	"go.uber.org/zap"
)

// invitePermissions = Send Messages | Read Message History.
const invitePermissions = (1 << 11) | (1 << 16)

//go:embed index.html
var indexHTML string

// indexTemplate is parsed once so syntax errors fail at startup.
var indexTemplate = template.Must(template.New("index").Parse(indexHTML))

// indexData is the model passed to indexTemplate.
type indexData struct {
	InviteURL string
}

// handleIndex renders the landing page; an invite button is added when discordClientID is set.
func handleIndex(discordClientID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data := indexData{}
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
