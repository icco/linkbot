// Package discord wires the linkbot to Discord through bwmarrin/discordgo.
//
// Two surfaces are supported side by side:
//   - the original message listener that reads channel messages and replies
//     with a sanitized URL when one is found;
//   - a /sanitize global slash command, registered via the OAuth2
//     client-credentials grant in [github.com/icco/linkbot/lib/discordoauth]
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
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/gorilla/websocket"
	"github.com/icco/linkbot/lib/discordoauth"
	"github.com/icco/linkbot/lib/logctx"
	"github.com/icco/linkbot/lib/sanitize"
)

// recentLookback is how many prior messages we examine to decide whether a
// sanitized URL has already been posted in the channel.
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
	http      *http.Client
	ready     chan struct{}
	readyOnce sync.Once
}

// New creates a new Discord bot. It does not open the gateway connection.
// The base logger is propagated to handlers via context, not stored on Bot.
func New(token string, san *sanitize.Sanitizer, base *slog.Logger) (*Bot, error) {
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
		http:    &http.Client{Timeout: 15 * time.Second},
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

	log := logctx.From(ctx)
	err := waitForReady(ctx, b.ready, readyTimeout)
	switch {
	case err == nil:
		if u := b.session.State.User; u != nil {
			log.Info("discord bot connected", "user", u.Username, "user_id", u.ID)
		} else {
			log.Warn("discord ready received but no user state")
		}
	case errors.Is(err, errReadyTimeout):
		log.Warn("discord ready event not received before timeout", "timeout", readyTimeout)
	default:
		return err
	}
	return nil
}

// Close shuts down the gateway connection.
func (b *Bot) Close() error {
	return b.session.Close()
}

// applicationID returns the bot's application ID from session state, or "".
// Defensive about nil pointers since callers run on the unhappy path.
func (b *Bot) applicationID() string {
	if b == nil || b.session == nil || b.session.State == nil {
		return ""
	}
	if app := b.session.State.Application; app != nil {
		return app.ID
	}
	return ""
}

// waitForReady blocks until ready closes, ctx is done, or timeout elapses.
// Returns errReadyTimeout on timeout and a wrapped ctx.Err() on cancel.
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

// intentHint returns a Developer Portal hint when err's chain contains a
// websocket close 4014, or "" otherwise. With a non-empty appID it
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
// The bearer token comes from the OAuth2 client-credentials grant via the
// supplied [discordoauth.Client]. The bot token is intentionally not used
// here; Discord accepts both for application command endpoints, but using
// the OAuth flow exercises the documented modern path.
func (b *Bot) RegisterCommands(ctx context.Context, oauth *discordoauth.Client, applicationID string) error {
	if applicationID == "" {
		return fmt.Errorf("register commands: empty applicationID")
	}
	if oauth == nil {
		return fmt.Errorf("register commands: nil oauth client")
	}
	log := logctx.From(ctx)

	token, err := oauth.Token(ctx)
	if err != nil {
		return fmt.Errorf("register commands: oauth token: %w", err)
	}

	body, err := json.Marshal([]applicationCommand{sanitizeCommand()})
	if err != nil {
		return fmt.Errorf("register commands: marshal body: %w", err)
	}

	url := fmt.Sprintf("%s/applications/%s/commands", discordRESTBaseURL, applicationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("register commands: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := b.http.Do(req)
	if err != nil {
		return fmt.Errorf("register commands: request: %w", err)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			log.Warn("close register commands response body", "error", cerr)
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
	log.Info("discord slash commands registered",
		"command", sanitizeCommandName,
		"application_id", applicationID,
		"status", resp.StatusCode,
	)
	return nil
}

// handleMessage returns the discordgo MessageCreate handler. It closes over
// the base logger so each event can derive a per-message child logger
// (channel/message/author IDs) and stash it on a fresh, time-bounded context.
// Bot/own messages are ignored to avoid feedback loops.
func (b *Bot) handleMessage(base *slog.Logger) func(*discordgo.Session, *discordgo.MessageCreate) {
	return func(s *discordgo.Session, m *discordgo.MessageCreate) {
		if m.Author == nil || m.Author.Bot {
			return
		}
		urls := sanitize.FindURLs(m.Content)
		if len(urls) == 0 {
			return
		}

		log := base.With(
			"channel_id", m.ChannelID,
			"message_id", m.ID,
			"author_id", m.Author.ID,
		)
		ctx, cancel := context.WithTimeout(logctx.New(context.Background(), log), 20*time.Second)
		defer cancel()

		replies := b.buildReplies(ctx, s, m, urls)
		if len(replies) == 0 {
			return
		}

		reply := strings.Join(replies, "\n")
		if _, err := s.ChannelMessageSendReply(m.ChannelID, reply, m.Reference()); err != nil {
			logctx.From(ctx).Error("discord reply failed", "error", err)
		}
	}
}

// handleInteraction returns the discordgo InteractionCreate handler. Only
// application-command interactions for /sanitize are serviced; other
// interaction types fall through silently so future commands or component
// callbacks can be layered on without surprising existing users.
func (b *Bot) handleInteraction(base *slog.Logger) func(*discordgo.Session, *discordgo.InteractionCreate) {
	return func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		if i.Type != discordgo.InteractionApplicationCommand {
			return
		}
		data := i.ApplicationCommandData()
		if data.Name != sanitizeCommandName {
			return
		}

		log := base.With(
			"interaction_id", i.ID,
			"command", data.Name,
		)
		if i.ChannelID != "" {
			log = log.With("channel_id", i.ChannelID)
		}
		if i.GuildID != "" {
			log = log.With("guild_id", i.GuildID)
		}
		if user := interactionUser(i); user != nil {
			log = log.With("user_id", user.ID)
		}

		ctx, cancel := context.WithTimeout(logctx.New(context.Background(), log), 20*time.Second)
		defer cancel()

		raw := optionString(data.Options, "url")
		if raw == "" {
			respondInteractionError(s, i, log, "missing required `url` option")
			return
		}

		clean, err := b.san.URL(ctx, raw)
		if err != nil {
			log.Error("interaction sanitize failed", "url", raw, "error", err)
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
			log.Error("interaction respond failed", "error", err)
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
func respondInteractionError(s *discordgo.Session, i *discordgo.InteractionCreate, log *slog.Logger, summary string) {
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: summary,
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	})
	if err != nil {
		log.Error("interaction error respond failed", "error", err)
	}
}

// buildReplies sanitizes each URL and filters out ones that are unchanged or
// that already appear (a) in the message itself or (b) in the recent
// channel history. The latter avoids piling on if another bot or user has
// already shared the song.link version.
func (b *Bot) buildReplies(ctx context.Context, s *discordgo.Session, m *discordgo.MessageCreate, urls []string) []string {
	log := logctx.From(ctx)

	var replies []string
	for _, raw := range urls {
		clean, err := b.san.URL(ctx, raw)
		if err != nil {
			log.Warn("sanitize failed", "url", raw, "error", err)
			continue
		}
		if !sanitize.Changed(raw, clean) {
			continue
		}
		if strings.Contains(m.Content, clean) {
			log.Debug("sanitized url already in source message", "url", clean)
			continue
		}
		seen, err := recentlyPosted(s, m.ChannelID, m.ID, clean)
		if err != nil {
			log.Warn("could not check recent messages", "error", err)
		} else if seen {
			log.Debug("sanitized url already in channel", "url", clean)
			continue
		}
		replies = append(replies, clean)
	}
	return replies
}

// recentlyPosted reports whether target appears in any of the last
// recentLookback messages posted in channelID before beforeID. It is used to
// suppress duplicate sanitized-link replies.
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
