package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/gorilla/websocket"
	"github.com/icco/linkbot/lib/sanitize"
	"go.uber.org/zap"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestHandleInteractionOnlyPostsChangedURLs(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{name: "unchanged", raw: "https://example.com/article"},
		{name: "already archived", raw: "https://archive.ph/example"},
		{name: "changed", raw: "https://example.com/article?utm_source=discord", want: "https://example.com/article"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, err := discordgo.New("Bot test")
			if err != nil {
				t.Fatalf("discord session: %v", err)
			}
			var requests []string
			var response discordgo.InteractionResponse
			s.Client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				requests = append(requests, r.Method+" "+r.URL.Path)
				if r.Method == http.MethodPost {
					if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&response); err != nil {
						t.Fatalf("decode interaction response: %v", err)
					}
				}
				return &http.Response{
					StatusCode: http.StatusNoContent,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader("")),
				}, nil
			})}
			b := &Bot{san: sanitize.New(nil)}
			b.handleInteraction(zap.NewNop().Sugar())(s, &discordgo.InteractionCreate{
				Interaction: &discordgo.Interaction{
					ID:    "interaction",
					AppID: "app",
					Token: "token",
					Type:  discordgo.InteractionApplicationCommand,
					Data: discordgo.ApplicationCommandInteractionData{
						Name: sanitizeCommandName,
						Options: []*discordgo.ApplicationCommandInteractionDataOption{
							{Name: "url", Type: discordgo.ApplicationCommandOptionString, Value: tc.raw},
						},
					},
				},
			})

			wantRequests := "POST /api/v9/interactions/interaction/token/callback"
			wantType := discordgo.InteractionResponseChannelMessageWithSource
			var wantFlags discordgo.MessageFlags
			if tc.want == "" {
				wantRequests += "\nDELETE /api/v9/webhooks/app/token/messages/@original"
				wantType = discordgo.InteractionResponseDeferredChannelMessageWithSource
				wantFlags = discordgo.MessageFlagsEphemeral
			}
			if got := strings.Join(requests, "\n"); got != wantRequests {
				t.Errorf("requests = %q, want %q", got, wantRequests)
			}
			if response.Type != wantType {
				t.Errorf("response type = %v, want %v", response.Type, wantType)
			}
			if response.Data == nil {
				t.Fatal("missing response data")
			}
			if response.Data.Content != tc.want || response.Data.Flags != wantFlags {
				t.Errorf("response content = %q, flags = %v; want %q, %v", response.Data.Content, response.Data.Flags, tc.want, wantFlags)
			}
		})
	}
}

func TestHandleMessageUnchangedURLDoesNotPost(t *testing.T) {
	s, err := discordgo.New("Bot test")
	if err != nil {
		t.Fatalf("discord session: %v", err)
	}
	s.Client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatalf("unexpected Discord request: %s %s", r.Method, r.URL)
		return nil, errors.New("unexpected Discord request")
	})}
	b := &Bot{san: sanitize.New(nil)}
	b.handleMessage(zap.NewNop().Sugar())(s, &discordgo.MessageCreate{
		Message: &discordgo.Message{
			ID:        "message",
			ChannelID: "channel",
			Author:    &discordgo.User{ID: "user"},
			Content:   "This is neat https://bughunters.google.com/blog/scaling-memory-safety",
		},
	})
}

// TestWaitForReady_Fires checks that an already-closed ready returns nil.
func TestWaitForReady_Fires(t *testing.T) {
	t.Parallel()

	ready := make(chan struct{})
	close(ready)

	if err := waitForReady(context.Background(), ready, time.Second); err != nil {
		t.Fatalf("waitForReady returned %v, want nil", err)
	}
}

// TestWaitForReady_FiresAfterDelay checks that ready closing mid-call wins.
func TestWaitForReady_FiresAfterDelay(t *testing.T) {
	t.Parallel()

	ready := make(chan struct{})
	go func() {
		time.Sleep(10 * time.Millisecond)
		close(ready)
	}()

	if err := waitForReady(context.Background(), ready, time.Second); err != nil {
		t.Fatalf("waitForReady returned %v, want nil", err)
	}
}

// TestWaitForReady_Timeout checks that timeout returns errReadyTimeout.
func TestWaitForReady_Timeout(t *testing.T) {
	t.Parallel()

	ready := make(chan struct{})
	err := waitForReady(context.Background(), ready, 10*time.Millisecond)
	if !errors.Is(err, errReadyTimeout) {
		t.Fatalf("waitForReady returned %v, want errReadyTimeout", err)
	}
}

// TestWaitForReady_CtxCanceled checks that ctx cancellation wraps ctx.Err.
func TestWaitForReady_CtxCanceled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ready := make(chan struct{})
	err := waitForReady(ctx, ready, time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waitForReady returned %v, want chain containing context.Canceled", err)
	}
	if !strings.Contains(err.Error(), "discord ready wait") {
		t.Fatalf("error %q missing %q prefix", err.Error(), "discord ready wait")
	}
}

// TestWaitForReady_CtxDeadline checks that deadline wraps DeadlineExceeded.
func TestWaitForReady_CtxDeadline(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()

	ready := make(chan struct{})
	err := waitForReady(ctx, ready, time.Second)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitForReady returned %v, want chain containing context.DeadlineExceeded", err)
	}
}

// TestIntentHint covers the close-4014 detection helper across nil,
// non-close, wrong-code, direct, wrapped, and missing-app-ID inputs.
func TestIntentHint(t *testing.T) {
	t.Parallel()

	close4014 := &websocket.CloseError{Code: 4014, Text: "Disallowed intent(s)"}
	close4001 := &websocket.CloseError{Code: 4001, Text: "Unknown opcode"}

	tests := []struct {
		name    string
		err     error
		appID   string
		want    string
		wantURL string
	}{
		{name: "nil error", err: nil, appID: "123"},
		{name: "non-close error", err: errors.New("dial tcp: connection refused"), appID: "123"},
		{name: "close 4001", err: close4001, appID: "123"},
		{
			name:    "close 4014 direct with app id",
			err:     close4014,
			appID:   "1234567890",
			want:    "gateway rejected privileged intent(s) (close 4014)",
			wantURL: "https://discord.com/developers/applications/1234567890/bot",
		},
		{
			name:    "close 4014 wrapped with app id",
			err:     fmt.Errorf("discord open: %w", close4014),
			appID:   "1234567890",
			want:    "gateway rejected privileged intent(s) (close 4014)",
			wantURL: "https://discord.com/developers/applications/1234567890/bot",
		},
		{
			name:    "close 4014 without app id falls back to portal root",
			err:     close4014,
			appID:   "",
			want:    "for your application at https://discord.com/developers/applications",
			wantURL: "https://discord.com/developers/applications",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := intentHint(tc.err, tc.appID)
			if tc.want == "" {
				if got != "" {
					t.Fatalf("intentHint(%v, %q) = %q, want empty", tc.err, tc.appID, got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("intentHint(%v, %q) = %q, want substring %q", tc.err, tc.appID, got, tc.want)
			}
			if !strings.Contains(got, tc.wantURL) {
				t.Fatalf("intentHint(%v, %q) = %q, want URL substring %q", tc.err, tc.appID, got, tc.wantURL)
			}
			if strings.Contains(got, " /bot") || strings.Contains(got, "applications/your application") {
				t.Fatalf("intentHint(%v, %q) = %q, must not interpolate a placeholder into the URL path", tc.err, tc.appID, got)
			}
		})
	}
}

// TestOnReady_Idempotent checks that repeat READY events don't re-close ready.
func TestOnReady_Idempotent(t *testing.T) {
	t.Parallel()

	b := &Bot{ready: make(chan struct{})}

	b.onReady(nil, nil)
	b.onReady(nil, nil)
	b.onReady(nil, nil)

	select {
	case <-b.ready:
	case <-time.After(time.Second):
		t.Fatalf("ready channel was not closed by onReady")
	}
}

// TestSanitizeCommandShape checks the /sanitize command we hand to
// discordgo: name, type, and a required string `url` option.
func TestSanitizeCommandShape(t *testing.T) {
	t.Parallel()

	cmd := sanitizeCommand()
	if cmd.Name != sanitizeCommandName {
		t.Errorf("Name = %q, want %q", cmd.Name, sanitizeCommandName)
	}
	if cmd.Type != discordgo.ChatApplicationCommand {
		t.Errorf("Type = %v, want ChatApplicationCommand", cmd.Type)
	}
	if cmd.Description == "" {
		t.Errorf("Description must not be empty")
	}
	if len(cmd.Options) != 1 {
		t.Fatalf("len(Options) = %d, want 1", len(cmd.Options))
	}
	opt := cmd.Options[0]
	if opt.Name != "url" {
		t.Errorf("option Name = %q, want \"url\"", opt.Name)
	}
	if opt.Type != discordgo.ApplicationCommandOptionString {
		t.Errorf("option Type = %v, want String", opt.Type)
	}
	if !opt.Required {
		t.Errorf("option Required = false, want true")
	}
}
