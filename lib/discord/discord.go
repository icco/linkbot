// Package discord wires the linkbot to Discord through bwmarrin/discordgo.
//
// Two surfaces are supported side by side:
//   - the original message listener that reads channel messages and replies
//     with a sanitized URL when one is found;
//   - a /sanitize global slash command, registered via the OAuth2
//     client-credentials grant from [golang.org/x/oauth2/clientcredentials]
//     and serviced through an InteractionCreate handler.
//
// The bot token is still required for the gateway connection — Discord
// does not allow client-credentials to authorize a gateway, so OAuth2
// only powers REST calls (slash command registration and similar).
package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/gorilla/websocket"
	"github.com/icco/gutil/logging"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"

	"github.com/icco/linkbot/lib/sanitize"
)

// recentLookback is how many prior channel messages we scan when
// deduping a sanitized URL.
const recentLookback = 20

// closeDisallowedIntents is the gateway close code for "Disallowed
// intent(s)" — a privileged intent not enabled in the Developer Portal.
const closeDisallowedIntents = 4014

// readyTimeout bounds how long Start waits for READY before continuing
// without a populated session state.
const readyTimeout = 10 * time.Second

// errReadyTimeout signals that READY did not arrive within readyTimeout.
var errReadyTimeout = errors.New("discord ready event not received before timeout")

// discordRESTBaseURL is the Discord REST API root used for slash command
// registration. v10 is the current GA version; bump when Discord deprecates.
const discordRESTBaseURL = "https://discord.com/api/v10"

// userAgent is sent on every outbound REST request originating from this
// package (slash command registration today).
const userAgent = "linkbot/0.1 (+https://github.com/icco/linkbot)"

// sanitizeCommandName is the global slash command name registered with
// Discord. Kept as a package-level const so it can be referenced from both
// RegisterCommands and the interaction handler without drift.
const sanitizeCommandName = "sanitize"

// errorBodyLimit caps how many bytes of an error response body we surface
// in wrapped errors when Discord returns a non-2xx for command
// registration.
const errorBodyLimit = 512

// meterName is the OTel meter scope.
const meterName = "linkbot/discord"

// instOnce guards lazy init of messagesCounter.
var instOnce sync.Once

var (
	messagesCounter metric.Int64Counter
	instErr         error
)

// initInstruments creates the package's OTel counter.
func initInstruments() {
	c, err := otel.Meter(meterName).Int64Counter(
		"discord_messages_total",
		metric.WithDescription("Number of Discord messages bucketed by linkbot's action."),
	)
	if err != nil {
		instErr = fmt.Errorf("discord_messages_total: %w", err)
		return
	}
	messagesCounter = c
}

// recordAction increments messagesCounter; no-op when init failed.
func recordAction(ctx context.Context, action string) {
	if messagesCounter == nil {
		return
	}
	messagesCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("action", action)))
}

// applicationCommandOption mirrors the subset of Discord's
// application-command-option schema we send when registering /sanitize.
// Type 3 = STRING per Discord's command option type table.
type applicationCommandOption struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Type        int    `json:"type"`
	Required    bool   `json:"required,omitempty"`
}

// applicationCommand mirrors the subset of Discord's application-command
// schema we send when registering /sanitize. Type 1 = CHAT_INPUT (slash).
type applicationCommand struct {
	Name        string                     `json:"name"`
	Description string                     `json:"description"`
	Type        int                        `json:"type"`
	Options     []applicationCommandOption `json:"options,omitempty"`
}

// sanitizeCommand returns the application command definition we register
// with Discord. Returning a fresh value avoids accidental cross-test
// aliasing in callers that mutate the slice.
func sanitizeCommand() applicationCommand {
	return applicationCommand{
		Name:        sanitizeCommandName,
		Description: "Sanitize a URL",
		Type:        1,
		Options: []applicationCommandOption{
			{
				Name:        "url",
				Description: "URL to sanitize",
				Type:        3,
				Required:    true,
			},
		},
	}
}

// Bot listens for messages on Discord and replies with sanitized URLs. It
// also serves the /sanitize slash command when registered.
type Bot struct {
	session   *discordgo.Session
	san       *sanitize.Sanitizer
	ready     chan struct{}
	readyOnce sync.Once
}

// New creates a Bot; call Start to open the gateway.
func New(token string, san *sanitize.Sanitizer, base *zap.SugaredLogger) (*Bot, error) {
	s, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, fmt.Errorf("discordgo: %w", err)
	}
	s.Identify.Intents = discordgo.IntentsGuildMessages |
		discordgo.IntentsDirectMessages |
		discordgo.IntentMessageContent

	b := &Bot{
		session: s,
		san:     san,
		ready:   make(chan struct{}),
	}
	s.AddHandler(b.onReady)
	s.AddHandler(b.handleMessage(base))
	s.AddHandler(b.handleInteraction(base))
	return b, nil
}

// onReady signals Start that the gateway READY event arrived. sync.Once
// guards against double-close on reconnect-driven re-fires.
func (b *Bot) onReady(_ *discordgo.Session, _ *discordgo.Ready) {
	b.readyOnce.Do(func() {
		close(b.ready)
	})
}

// Start opens the gateway, waits up to readyTimeout for READY, and on a
// close-4014 from Open() wraps the error with a Developer Portal hint.
func (b *Bot) Start(ctx context.Context) error {
	if err := b.session.Open(); err != nil {
		if hint := intentHint(err, b.applicationID()); hint != "" {
			return fmt.Errorf("discord open: %s: %w", hint, err)
		}
		return fmt.Errorf("discord open: %w", err)
	}

	log := logging.FromContext(ctx)
	err := waitForReady(ctx, b.ready, readyTimeout)
	switch {
	case err == nil:
		if u := b.session.State.User; u != nil {
			log.Infow("discord bot connected", "user", u.Username, "user_id", u.ID)
		} else {
			log.Warn("discord ready received but no user state")
		}
	case errors.Is(err, errReadyTimeout):
		log.Warnw("discord ready event not received before timeout", "timeout", readyTimeout)
	default:
		return err
	}
	return nil
}

// Close shuts down the gateway connection.
func (b *Bot) Close() error {
	return b.session.Close()
}

// applicationID returns the bot's application ID from session state, or
// "". Defensive about nil pointers since callers run on the unhappy path.
func (b *Bot) applicationID() string {
	if b == nil || b.session == nil || b.session.State == nil {
		return ""
	}
	if app := b.session.State.Application; app != nil {
		return app.ID
	}
	return ""
}

// waitForReady blocks until ready closes, ctx is done, or timeout
// elapses. Returns errReadyTimeout on timeout and a wrapped ctx.Err on
// cancel.
func waitForReady(ctx context.Context, ready <-chan struct{}, timeout time.Duration) error {
	select {
	case <-ready:
		return nil
	case <-time.After(timeout):
		return errReadyTimeout
	case <-ctx.Done():
		return fmt.Errorf("discord ready wait: %w", ctx.Err())
	}
}

// intentHint returns a Developer Portal hint when err's chain contains
// a websocket close 4014, or "" otherwise. With a non-empty appID it
// deep-links to that bot's page; otherwise it links to the portal root.
func intentHint(err error, appID string) string {
	if err == nil {
		return ""
	}
	var ce *websocket.CloseError
	if !errors.As(err, &ce) || ce.Code != closeDisallowedIntents {
		return ""
	}
	if appID != "" {
		return fmt.Sprintf(
			"gateway rejected privileged intent(s) (close 4014); enable Message Content Intent at https://discord.com/developers/applications/%s/bot",
			appID,
		)
	}
	return "gateway rejected privileged intent(s) (close 4014); enable Message Content Intent for your application at https://discord.com/developers/applications"
}

// RegisterCommands PUT-overwrites the global application command set so
// that linkbot's /sanitize slash command is available in every guild that
// has installed the application.
//
// applicationID is passed explicitly (rather than read from the session)
// to keep the dependency direction clean: command registration must work
// even if the gateway has not yet finished its READY handshake. main
// supplies cfg.DiscordClientID which equals the application ID for bot
// applications.
//
// httpClient is expected to attach an OAuth2 bearer token automatically
// (e.g. produced by [golang.org/x/oauth2/clientcredentials.Config.Client]),
// which keeps token caching, refresh, and Basic-auth encoding out of this
// package. A plain *http.Client also works when the caller has wired auth
// some other way; in that case Discord will return 401 and the wrapped
// status code surfaces the misconfiguration.
func (b *Bot) RegisterCommands(ctx context.Context, httpClient *http.Client, applicationID string) error {
	if applicationID == "" {
		return fmt.Errorf("register commands: empty applicationID")
	}
	if httpClient == nil {
		return fmt.Errorf("register commands: nil http client")
	}
	log := logging.FromContext(ctx)

	body, err := json.Marshal([]applicationCommand{sanitizeCommand()})
	if err != nil {
		return fmt.Errorf("register commands: marshal body: %w", err)
	}

	url := fmt.Sprintf("%s/applications/%s/commands", discordRESTBaseURL, applicationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("register commands: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("register commands: request: %w", err)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			log.Warnw("close register commands response body", zap.Error(cerr))
		}
	}()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("register commands: read body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("register commands: discord %d: %s",
			resp.StatusCode, truncate(string(respBody), errorBodyLimit))
	}
	log.Infow("discord slash commands registered",
		"command", sanitizeCommandName,
		"application_id", applicationID,
		"status", resp.StatusCode,
	)
	return nil
}

// handleMessage returns the MessageCreate handler. Bot/own messages
// are ignored to avoid feedback loops.
func (b *Bot) handleMessage(base *zap.SugaredLogger) func(*discordgo.Session, *discordgo.MessageCreate) {
	return func(s *discordgo.Session, m *discordgo.MessageCreate) {
		instOnce.Do(initInstruments)
		if instErr != nil && base != nil {
			base.Warnw("discord metrics unavailable", zap.Error(instErr))
		}

		if m.Author == nil || m.Author.Bot {
			return
		}
		urls := sanitize.FindURLs(m.Content)
		if len(urls) == 0 {
			return
		}

		ctx, cancel := context.WithTimeout(
			logging.NewContext(context.Background(), base,
				"channel_id", m.ChannelID,
				"message_id", m.ID,
				"author_id", m.Author.ID,
			),
			20*time.Second,
		)
		defer cancel()

		replies := b.buildReplies(ctx, s, m, urls)
		if len(replies) == 0 {
			return
		}

		reply := strings.Join(replies, "\n")
		if _, err := s.ChannelMessageSendReply(m.ChannelID, reply, m.Reference()); err != nil {
			logging.FromContext(ctx).Errorw("discord reply failed", zap.Error(err))
			recordAction(ctx, "errored")
			return
		}
		recordAction(ctx, "replied")
	}
}

// handleInteraction returns the discordgo InteractionCreate handler. Only
// application-command interactions for /sanitize are serviced; other
// interaction types fall through silently so future commands or component
// callbacks can be layered on without surprising existing users.
func (b *Bot) handleInteraction(base *zap.SugaredLogger) func(*discordgo.Session, *discordgo.InteractionCreate) {
	return func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		if i.Type != discordgo.InteractionApplicationCommand {
			return
		}
		data := i.ApplicationCommandData()
		if data.Name != sanitizeCommandName {
			return
		}

		fields := []interface{}{
			"interaction_id", i.ID,
			"command", data.Name,
		}
		if i.ChannelID != "" {
			fields = append(fields, "channel_id", i.ChannelID)
		}
		if i.GuildID != "" {
			fields = append(fields, "guild_id", i.GuildID)
		}
		if user := interactionUser(i); user != nil {
			fields = append(fields, "user_id", user.ID)
		}

		ctx, cancel := context.WithTimeout(
			logging.NewContext(context.Background(), base, fields...),
			20*time.Second,
		)
		defer cancel()
		log := logging.FromContext(ctx)

		raw := optionString(data.Options, "url")
		if raw == "" {
			respondInteractionError(s, i, log, "missing required `url` option")
			return
		}

		clean, err := b.san.URL(ctx, raw)
		if err != nil {
			log.Errorw("interaction sanitize failed", "url", raw, zap.Error(err))
			respondInteractionError(s, i, log, "could not sanitize that URL")
			return
		}

		var content string
		if sanitize.Changed(raw, clean) {
			content = clean
		} else {
			content = "No sanitization needed: " + raw
		}

		if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: content,
			},
		}); err != nil {
			log.Errorw("interaction respond failed", zap.Error(err))
		}
	}
}

// interactionUser returns the user who triggered the interaction. Discord
// puts the user on Member.User for guild interactions and on User for DMs;
// the helper hides that branching from callers.
func interactionUser(i *discordgo.InteractionCreate) *discordgo.User {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User
	}
	return i.User
}

// optionString returns the string value of the named option, or "" when
// the option is absent or not a string. Discord guarantees the type but
// we still defensively check before calling StringValue.
func optionString(opts []*discordgo.ApplicationCommandInteractionDataOption, name string) string {
	for _, o := range opts {
		if o == nil {
			continue
		}
		if o.Name == name && o.Type == discordgo.ApplicationCommandOptionString {
			return o.StringValue()
		}
	}
	return ""
}

// respondInteractionError sends an ephemeral error message back to the
// invoking user. The internal error is already logged by the caller; we
// only surface a short, user-safe summary to avoid leaking internals.
func respondInteractionError(s *discordgo.Session, i *discordgo.InteractionCreate, log *zap.SugaredLogger, summary string) {
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: summary,
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	})
	if err != nil {
		log.Errorw("interaction error respond failed", zap.Error(err))
	}
}

// buildReplies sanitizes each URL and drops unchanged ones or any
// already present in the message or recent channel history, so we
// don't pile on top of another bot's reply.
func (b *Bot) buildReplies(ctx context.Context, s *discordgo.Session, m *discordgo.MessageCreate, urls []string) []string {
	log := logging.FromContext(ctx)

	var replies []string
	for _, raw := range urls {
		clean, err := b.san.URL(ctx, raw)
		if err != nil {
			log.Warnw("sanitize failed", "url", raw, zap.Error(err))
			recordAction(ctx, "errored")
			continue
		}
		if !sanitize.Changed(raw, clean) {
			recordAction(ctx, "skipped")
			continue
		}
		if strings.Contains(m.Content, clean) {
			log.Debugw("sanitized url already in source message", "url", clean)
			recordAction(ctx, "skipped")
			continue
		}
		seen, err := recentlyPosted(s, m.ChannelID, m.ID, clean)
		if err != nil {
			log.Warnw("could not check recent messages", zap.Error(err))
		} else if seen {
			log.Debugw("sanitized url already in channel", "url", clean)
			recordAction(ctx, "skipped")
			continue
		}
		replies = append(replies, clean)
	}
	return replies
}

// recentlyPosted reports whether target appears in the last
// recentLookback messages of channelID before beforeID.
func recentlyPosted(s *discordgo.Session, channelID, beforeID, target string) (bool, error) {
	msgs, err := s.ChannelMessages(channelID, recentLookback, beforeID, "", "")
	if err != nil {
		return false, fmt.Errorf("channel messages: %w", err)
	}
	for _, prior := range msgs {
		if strings.Contains(prior.Content, target) {
			return true, nil
		}
	}
	return false, nil
}

// truncate clips s to at most n bytes, appending an ellipsis when
// truncation occurs. Used to keep error bodies bounded when Discord
// returns a non-2xx for command registration.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
