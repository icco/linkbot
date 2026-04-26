// Package discord wires the linkbot to Discord through bwmarrin/discordgo.
package discord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/gorilla/websocket"
	"github.com/icco/linkbot/lib/logctx"
	"github.com/icco/linkbot/lib/sanitize"
)

// recentLookback is how many prior messages we examine to decide whether a
// sanitized URL has already been posted in the channel.
const recentLookback = 20

// closeDisallowedIntents is the gateway close code returned when the bot
// identifies with a privileged intent that has not been enabled in the
// Developer Portal. See
// https://discord.com/developers/docs/topics/opcodes-and-status-codes#gateway-gateway-close-event-codes.
const closeDisallowedIntents = 4014

// readyTimeout bounds how long Start waits for the gateway READY event after
// Open() returns. discordgo's Open() has been observed to return nil before
// READY fully populates session state, so we wait briefly so the success log
// line carries a real username. If READY never arrives we log a warning and
// return nil instead of crashing: the bot may still recover via discordgo's
// reconnect logic, and failing here would only put us in a docker
// `restart: always` hot-loop.
const readyTimeout = 10 * time.Second

// errReadyTimeout signals that the gateway READY event did not arrive within
// readyTimeout. It is unexported because callers should react to it with a
// warning log, not by failing Start.
var errReadyTimeout = errors.New("discord ready event not received before timeout")

// Bot listens for messages on Discord and replies with sanitized URLs.
type Bot struct {
	session   *discordgo.Session
	san       *sanitize.Sanitizer
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
		ready:   make(chan struct{}),
	}
	s.AddHandler(b.onReady)
	s.AddHandler(b.handleMessage(base))
	return b, nil
}

// onReady fires when discordgo dispatches a READY (or RESUMED-then-READY)
// event. It signals Start that session state has been populated. We guard the
// channel close with sync.Once because reconnects can deliver multiple READY
// events and closing an already-closed channel panics.
func (b *Bot) onReady(_ *discordgo.Session, _ *discordgo.Ready) {
	b.readyOnce.Do(func() {
		close(b.ready)
	})
}

// Start opens the gateway connection. It then waits up to readyTimeout for
// the READY handshake so the success-path log carries a populated user. On
// timeout it logs a warning and returns nil rather than crashing the
// process; on a 4014 ("Disallowed intent(s)") close from Open() it wraps the
// error with a hint pointing the operator at the privileged-intent toggle in
// the Discord Developer Portal.
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

// applicationID returns the bot's Discord application ID from session state,
// or an empty string when the gateway never populated it (e.g. Open() failed
// before READY). It is defensive about nil pointers because the whole point
// of this helper is to be called on the unhappy path.
func (b *Bot) applicationID() string {
	if b == nil || b.session == nil || b.session.State == nil {
		return ""
	}
	if app := b.session.State.Application; app != nil {
		return app.ID
	}
	return ""
}

// waitForReady blocks until ready is closed, ctx is canceled, or timeout
// elapses. The timeout path returns errReadyTimeout so callers can react via
// errors.Is; the ctx path wraps ctx.Err() so callers can inspect cancellation
// vs. deadline.
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

// intentHint inspects err for a gorilla/websocket close 4014 ("Disallowed
// intent(s)") anywhere in its chain and returns a human-readable hint
// pointing the operator at the privileged-intent toggle in the Discord
// Developer Portal. It returns the empty string for any other error so
// callers can branch on a simple non-empty check. appID is best-effort: when
// non-empty we deep-link to that application's bot page; otherwise we link
// to the portal root rather than inventing an ID.
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
