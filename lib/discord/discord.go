// Package discord wires linkbot to Discord via bwmarrin/discordgo.
//
// Surfaces:
//   - a message listener that replies with sanitized URLs;
//   - a /sanitize global slash command registered via OAuth2
//     client-credentials and served from an InteractionCreate handler.
//
// The bot token still authenticates the gateway; OAuth2 only signs REST
// calls (slash command registration today).
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

// recentLookback is how many prior channel messages we scan to dedupe.
const recentLookback = 20

// closeDisallowedIntents is the gateway close code when a privileged
// intent isn't enabled in the Developer Portal.
const closeDisallowedIntents = 4014

// readyTimeout bounds how long Start waits for the READY event.
const readyTimeout = 10 * time.Second

// errReadyTimeout signals that READY did not arrive in time.
var errReadyTimeout = errors.New("discord ready event not received before timeout")

// discordRESTBaseURL is Discord's v10 REST API root.
const discordRESTBaseURL = "https://discord.com/api/v10"

// userAgent is sent on every outbound REST request.
const userAgent = "linkbot/0.1 (+https://github.com/icco/linkbot)"

// sanitizeCommandName is the global slash command name.
const sanitizeCommandName = "sanitize"

// errorBodyLimit caps how many bytes of a Discord error body we surface.
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

// applicationCommandOption is Discord's command-option schema subset.
// Type 3 = STRING.
type applicationCommandOption struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Type        int    `json:"type"`
	Required    bool   `json:"required,omitempty"`
}

// applicationCommand is Discord's command schema subset.
// Type 1 = CHAT_INPUT.
type applicationCommand struct {
	Name        string                     `json:"name"`
	Description string                     `json:"description"`
	Type        int                        `json:"type"`
	Options     []applicationCommandOption `json:"options,omitempty"`
}

// sanitizeCommand returns the /sanitize command definition.
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

// Bot replies to messages with sanitized URLs and serves /sanitize.
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

// onReady signals Start that READY arrived; safe across reconnects.
func (b *Bot) onReady(_ *discordgo.Session, _ *discordgo.Ready) {
	b.readyOnce.Do(func() {
		close(b.ready)
	})
}

// Start opens the gateway and waits up to readyTimeout for READY,
// adding a Developer Portal hint when Open() fails with close 4014.
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

// applicationID returns the bot's application ID, or "" if unknown.
func (b *Bot) applicationID() string {
	if b == nil || b.session == nil || b.session.State == nil {
		return ""
	}
	if app := b.session.State.Application; app != nil {
		return app.ID
	}
	return ""
}

// waitForReady blocks until ready closes, ctx is done, or timeout fires.
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

// intentHint returns a Developer Portal hint when err wraps a close
// 4014, or "" otherwise. Deep-links to appID's bot page when set.
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

// RegisterCommands PUT-overwrites the global slash commands with
// /sanitize. httpClient should attach OAuth2 (e.g. from
// [golang.org/x/oauth2/clientcredentials.Config.Client]); applicationID
// is the bot's app/client ID.
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

// handleMessage returns the MessageCreate handler; bot/own messages
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

// handleInteraction returns the InteractionCreate handler; only
// /sanitize is serviced, everything else is ignored.
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

// interactionUser returns the invoking user (Member.User for guilds,
// User for DMs).
func interactionUser(i *discordgo.InteractionCreate) *discordgo.User {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User
	}
	return i.User
}

// optionString returns the named string option, or "" if absent.
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

// respondInteractionError sends an ephemeral error reply with summary.
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

// buildReplies sanitizes urls and drops unchanged or already-posted ones.
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

// truncate clips s to n bytes, appending "..." if it had to cut.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
