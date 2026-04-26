// Package discord wires the linkbot to Discord through bwmarrin/discordgo.
package discord

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/icco/linkbot/lib/logctx"
	"github.com/icco/linkbot/lib/sanitize"
)

// recentLookback is how many prior messages we examine to decide whether a
// sanitized URL has already been posted in the channel.
const recentLookback = 20

// Bot listens for messages on Discord and replies with sanitized URLs.
type Bot struct {
	session *discordgo.Session
	san     *sanitize.Sanitizer
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

	b := &Bot{session: s, san: san}
	s.AddHandler(b.handleMessage(base))
	return b, nil
}

// Start opens the gateway connection.
func (b *Bot) Start(ctx context.Context) error {
	if err := b.session.Open(); err != nil {
		return fmt.Errorf("discord open: %w", err)
	}
	logctx.From(ctx).Info("discord bot connected", "user", b.session.State.User.Username)
	return nil
}

// Close shuts down the gateway connection.
func (b *Bot) Close() error {
	return b.session.Close()
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
