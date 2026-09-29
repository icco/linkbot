// Package discord replies with sanitized URLs and serves /sanitize via discordgo.
// Both gateway events and slash commands authenticate with the bot token.
package discord

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/bwmarrin/discordgo"
	"github.com/gorilla/websocket"
	"go.icco.me/gutil/logging"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"

	"go.icco.me/linkbot/lib/sanitize"
)

// recentLookback is how many prior channel messages we scan to dedupe.
const recentLookback = 20

const maxMessageLength = 2000

// closeDisallowedIntents is the gateway close code when a privileged
// intent isn't enabled in the Developer Portal.
const closeDisallowedIntents = 4014

// readyTimeout bounds how long Start waits for the READY event.
const readyTimeout = 10 * time.Second

// errReadyTimeout signals that READY did not arrive in time.
var errReadyTimeout = errors.New("discord ready event not received before timeout")

// sanitizeCommandName is the global slash command name.
const sanitizeCommandName = "sanitize"

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

// sanitizeCommand returns the /sanitize command definition.
func sanitizeCommand() *discordgo.ApplicationCommand {
	return &discordgo.ApplicationCommand{
		Name:        sanitizeCommandName,
		Type:        discordgo.ChatApplicationCommand,
		Description: "Sanitize a URL",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Name:        "url",
				Description: "URL to sanitize",
				Type:        discordgo.ApplicationCommandOptionString,
				Required:    true,
			},
		},
	}
}

// Bot replies to messages with sanitized URLs and serves /sanitize.
type Bot struct {
	session   *discordgo.Session
	san       sanitizer
	ready     chan struct{}
	readyOnce sync.Once
}

type sanitizer interface {
	URL(context.Context, string) (string, error)
}

// New creates a Bot; call Start to open the gateway.
func New(token string, san sanitizer) (*Bot, error) {
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
	b.session.AddHandler(b.handleMessage(ctx))
	b.session.AddHandler(b.handleInteraction(ctx))
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

// RegisterCommands bulk-overwrites the global slash commands with /sanitize.
// applicationID is the bot's app/client ID; pass cfg.DiscordClientID.
func (b *Bot) RegisterCommands(ctx context.Context, applicationID string) error {
	if applicationID == "" {
		return errors.New("register commands: empty applicationID")
	}
	cmds := []*discordgo.ApplicationCommand{sanitizeCommand()}
	if _, err := b.session.ApplicationCommandBulkOverwrite(applicationID, "", cmds, discordgo.WithContext(ctx)); err != nil {
		return fmt.Errorf("register commands: %w", err)
	}
	logging.FromContext(ctx).Infow("discord slash commands registered",
		"command", sanitizeCommandName,
		"application_id", applicationID,
	)
	return nil
}

// handleMessage returns the MessageCreate handler; bot/own messages
// are ignored to avoid feedback loops.
func (b *Bot) handleMessage(parent context.Context) func(*discordgo.Session, *discordgo.MessageCreate) {
	return func(s *discordgo.Session, m *discordgo.MessageCreate) {
		instOnce.Do(func() {
			initInstruments()
			if instErr != nil {
				logging.FromContext(parent).Warnw("discord metrics unavailable", zap.Error(instErr))
			}
		})

		if m.Author == nil || m.Author.Bot {
			return
		}
		urls := sanitize.FindURLs(m.Content)
		if len(urls) == 0 {
			return
		}

		ctx, cancel := context.WithTimeout(
			logging.NewContext(parent, logging.FromContext(parent),
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

		for _, reply := range splitReplies(replies) {
			if _, err := s.ChannelMessageSendComplex(m.ChannelID, &discordgo.MessageSend{
				Content:         reply,
				Reference:       m.Reference(),
				AllowedMentions: &discordgo.MessageAllowedMentions{},
			}, discordgo.WithContext(ctx)); err != nil {
				logging.FromContext(ctx).Errorw("discord reply failed", zap.Error(err))
				recordAction(ctx, "errored")
				return
			}
		}
		recordAction(ctx, "replied")
	}
}

// handleInteraction returns the InteractionCreate handler; only
// /sanitize is serviced, everything else is ignored.
func (b *Bot) handleInteraction(parent context.Context) func(*discordgo.Session, *discordgo.InteractionCreate) {
	return func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		if i.Type != discordgo.InteractionApplicationCommand {
			return
		}
		data := i.ApplicationCommandData()
		if data.Name != sanitizeCommandName {
			return
		}

		fields := []any{
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
			logging.NewContext(parent, logging.FromContext(parent), fields...),
			20*time.Second,
		)
		defer cancel()
		log := logging.FromContext(ctx)

		raw := optionString(data.Options, "url")
		if raw == "" {
			respondInteractionError(ctx, s, i, "missing required `url` option")
			return
		}

		// Discord requires acknowledgement within three seconds, before network lookups.
		if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral},
		}, discordgo.WithContext(ctx)); err != nil {
			log.Errorw("interaction acknowledge failed", zap.Error(err))
			return
		}

		clean, err := b.san.URL(ctx, raw)
		if err == nil && messageLength(clean) > maxMessageLength {
			err = errors.New("sanitized URL exceeds Discord's message limit")
		}
		if err != nil {
			log.Errorw("interaction sanitize failed", "url", raw, zap.Error(err))
			summary := "could not sanitize that URL"
			if _, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &summary}, discordgo.WithContext(ctx)); err != nil {
				log.Errorw("interaction error edit failed", zap.Error(err))
			}
			return
		}

		if sanitize.Changed(raw, clean) {
			// Complete the private defer first; Discord otherwise treats the first
			// followup as an edit and preserves its ephemeral flag.
			if _, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
				Content:         &clean,
				AllowedMentions: &discordgo.MessageAllowedMentions{},
			}, discordgo.WithContext(ctx)); err != nil {
				log.Errorw("interaction edit failed", zap.Error(err))
				return
			}
			if _, err := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
				Content:         clean,
				AllowedMentions: &discordgo.MessageAllowedMentions{},
			}, discordgo.WithContext(ctx)); err != nil {
				log.Errorw("interaction respond failed", zap.Error(err))
				return
			}
		}
		if err := s.InteractionResponseDelete(i.Interaction, discordgo.WithContext(ctx)); err != nil {
			log.Errorw("interaction dismiss failed", zap.Error(err))
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
func respondInteractionError(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, summary string) {
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: summary,
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	}, discordgo.WithContext(ctx))
	if err != nil {
		logging.FromContext(ctx).Errorw("interaction error respond failed", zap.Error(err))
	}
}

// buildReplies sanitizes urls and drops unchanged or already-posted ones.
func (b *Bot) buildReplies(ctx context.Context, s *discordgo.Session, m *discordgo.MessageCreate, urls []string) []string {
	log := logging.FromContext(ctx)
	seen := make(map[string]bool, len(urls))
	for _, raw := range urls {
		seen[raw] = true
	}
	processed := make(map[string]bool, len(urls))
	var historyLoaded bool

	var replies []string
	for _, raw := range urls {
		if ctx.Err() != nil {
			break
		}
		if processed[raw] {
			continue
		}
		processed[raw] = true
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
		if messageLength(clean) > maxMessageLength {
			log.Warnw("sanitized URL exceeds Discord's message limit", "url", clean)
			recordAction(ctx, "skipped")
			continue
		}
		if seen[clean] {
			log.Debugw("sanitized url already seen", "url", clean)
			recordAction(ctx, "skipped")
			continue
		}
		if !historyLoaded {
			historyLoaded = true
			msgs, err := s.ChannelMessages(m.ChannelID, recentLookback, m.ID, "", "", discordgo.WithContext(ctx))
			if err != nil {
				log.Warnw("could not check recent messages", zap.Error(err))
			}
			for _, prior := range msgs {
				for _, posted := range sanitize.FindURLs(prior.Content) {
					seen[posted] = true
				}
			}
		}
		if seen[clean] {
			log.Debugw("sanitized url already in channel", "url", clean)
			recordAction(ctx, "skipped")
			continue
		}
		seen[clean] = true
		replies = append(replies, clean)
	}
	return replies
}

// splitReplies batches whole URLs within Discord's 2,000 UTF-16-unit limit.
func splitReplies(replies []string) []string {
	var batches []string
	var batch []string
	length := 0
	for _, reply := range replies {
		n := messageLength(reply)
		if n > maxMessageLength {
			continue
		}
		if length+n+len(batch) > maxMessageLength {
			batches = append(batches, strings.Join(batch, "\n"))
			batch = batch[:0]
			length = 0
		}
		batch = append(batch, reply)
		length += n
	}
	if len(batch) > 0 {
		batches = append(batches, strings.Join(batch, "\n"))
	}
	return batches
}

func messageLength(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}
