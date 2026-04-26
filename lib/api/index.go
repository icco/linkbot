package api

import (
	_ "embed"
	"html/template"
	"net/http"
	"net/url"
	"strconv"

	"github.com/icco/linkbot/lib/logctx"
)

// invitePermissions is the bitmask of Discord permissions requested by the
// invite link: Send Messages (1<<11) + Read Message History (1<<16).
const invitePermissions = (1 << 11) | (1 << 16)

//go:embed index.html
var indexHTML string

// indexTemplate is parsed once at init so request handling stays fast and
// any template syntax error fails loudly at startup rather than on first hit.
var indexTemplate = template.Must(template.New("index").Parse(indexHTML))

// indexData is the data model passed to indexTemplate. InviteURL is empty
// when DISCORD_CLIENT_ID is unset, in which case the template renders
// generic instructions instead of a clickable button.
type indexData struct {
	InviteURL string
}

// handleIndex returns the GET / handler. It renders a small static HTML page
// describing the API and (when discordClientID is non-empty) a clickable
// invite link for the Discord bot.
func handleIndex(discordClientID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data := indexData{}
		if discordClientID != "" {
			data.InviteURL = inviteURL(discordClientID)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := indexTemplate.Execute(w, data); err != nil {
			logctx.From(r.Context()).Error("render index", "error", err)
		}
	}
}

// inviteURL builds the Discord OAuth2 invite URL for the application
// identified by clientID, requesting the bot scope and the permission set
// linkbot needs to operate.
func inviteURL(clientID string) string {
	q := url.Values{}
	q.Set("client_id", clientID)
	q.Set("scope", "bot")
	q.Set("permissions", strconv.Itoa(invitePermissions))
	return "https://discord.com/oauth2/authorize?" + q.Encode()
}
