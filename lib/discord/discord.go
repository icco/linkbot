// Package discord wires the linkbot to Discord through bwmarrin/discordgo.
package discord

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/icco/linkbot/lib/sanitize"
)

// Bot is a Discord client that listens for messages and replies with
// sanitized versions of any URLs it finds.
type Bot struct {
	session *discordgo.Session
	san     *sanitize.Sanitizer
	log     *slog.Logger
}

// New creates a new Discord bot. It does not open the gateway connection.
func New(token string, san *sanitize.Sanitizer, log *slog.Logger) (*Bot, error) {
	s, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, fmt.Errorf("discordgo: %w", err)
	}
	s.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentsDirectMessages | discordgo.IntentMessageContent

	b := &Bot{session: s, san: san, log: log}
	s.AddHandler(b.onMessage)
	return b, nil
}

// Start opens the gateway connection.
func (b *Bot) Start() error {
	if err := b.session.Open(); err != nil {
		return fmt.Errorf("discord open: %w", err)
	}
	b.log.Info("discord bot connected", "user", b.session.State.User.Username)
	return nil
}

// Close shuts down the gateway connection.
func (b *Bot) Close() error { return b.session.Close() }

func (b *Bot) onMessage(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author == nil || m.Author.Bot {
		return
	}
	urls := sanitize.FindURLs(m.Content)
	if len(urls) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var replies []string
	for _, raw := range urls {
		clean, err := b.san.URL(ctx, raw)
		if err != nil {
			b.log.Warn("sanitize failed", "url", raw, "error", err)
			continue
		}
		if sanitize.Changed(raw, clean) {
			replies = append(replies, clean)
		}
	}
	if len(replies) == 0 {
		return
	}

	reply := strings.Join(replies, "\n")
	if _, err := s.ChannelMessageSendReply(m.ChannelID, reply, m.Reference()); err != nil {
		b.log.Error("discord reply failed", "channel", m.ChannelID, "error", err)
	}
}
