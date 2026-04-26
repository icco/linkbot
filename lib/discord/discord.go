// Package discord wires the linkbot to Discord through bwmarrin/discordgo.
package discord

import (
	"context"
	"errors"
	"fmt"
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

// Bot listens for messages on Discord and replies with sanitized URLs.
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
