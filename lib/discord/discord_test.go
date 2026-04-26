package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

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

// TestSanitizeCommandJSONShape pins down the on-the-wire JSON we send to
// Discord when registering /sanitize. If discordgo or our local schema
// types ever drift, this test fails before we ship a broken slash command.
func TestSanitizeCommandJSONShape(t *testing.T) {
	t.Parallel()

	cmd := sanitizeCommand()
	out, err := json.Marshal([]applicationCommand{cmd})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got []map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(commands) = %d, want 1", len(got))
	}
	c := got[0]
	if c["name"] != sanitizeCommandName {
		t.Errorf("command name = %v, want %q", c["name"], sanitizeCommandName)
	}
	if c["description"] != "Sanitize a URL" {
		t.Errorf("command description = %v", c["description"])
	}
	if c["type"] != float64(1) {
		t.Errorf("command type = %v, want 1", c["type"])
	}

	options, ok := c["options"].([]any)
	if !ok {
		t.Fatalf("options is not an array: %T", c["options"])
	}
	if len(options) != 1 {
		t.Fatalf("len(options) = %d, want 1", len(options))
	}
	opt, ok := options[0].(map[string]any)
	if !ok {
		t.Fatalf("option[0] not an object: %T", options[0])
	}
	if opt["name"] != "url" {
		t.Errorf("option name = %v, want \"url\"", opt["name"])
	}
	if opt["description"] != "URL to sanitize" {
		t.Errorf("option description = %v", opt["description"])
	}
	if opt["type"] != float64(3) {
		t.Errorf("option type = %v, want 3 (STRING)", opt["type"])
	}
	if opt["required"] != true {
		t.Errorf("option required = %v, want true", opt["required"])
	}
}
