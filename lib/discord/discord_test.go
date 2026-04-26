// Package discord tests cover the gateway-handshake helpers extracted from
// (*Bot).Start: waitForReady (ready/timeout/ctx) and intentHint (close-4014
// detection on the error chain). These helpers exist precisely so we can
// exercise the racy/edge behavior without spinning up a real discordgo
// session.
package discord

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestWaitForReady_Fires checks that a closed ready channel returns nil
// immediately (the happy path that follows a real gateway READY event).
func TestWaitForReady_Fires(t *testing.T) {
	t.Parallel()

	ready := make(chan struct{})
	close(ready)

	if err := waitForReady(context.Background(), ready, time.Second); err != nil {
		t.Fatalf("waitForReady returned %v, want nil", err)
	}
}

// TestWaitForReady_FiresAfterDelay ensures waitForReady wakes up on the
// channel close even when ready is signaled after the call begins blocking.
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

// TestWaitForReady_Timeout asserts that a ready channel that never closes
// causes waitForReady to return errReadyTimeout, identifiable via errors.Is.
func TestWaitForReady_Timeout(t *testing.T) {
	t.Parallel()

	ready := make(chan struct{})
	err := waitForReady(context.Background(), ready, 10*time.Millisecond)
	if !errors.Is(err, errReadyTimeout) {
		t.Fatalf("waitForReady returned %v, want errReadyTimeout", err)
	}
}

// TestWaitForReady_CtxCanceled asserts that ctx cancellation wins over a
// pending ready/timeout and that ctx.Err() is preserved in the wrapped
// error chain so callers can inspect cancellation vs. deadline.
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

// TestWaitForReady_CtxDeadline checks the deadline-exceeded branch of the
// ctx case: the wrapped error must report context.DeadlineExceeded so
// callers can distinguish it from a manual cancel.
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

// TestIntentHint covers the close-4014 detection helper. The hint must fire
// for direct and wrapped *websocket.CloseError values with code 4014, fall
// back to a portal-root URL when no app ID is known (rather than splicing a
// human-readable placeholder into the path), and return the empty string
// for nil / non-close / wrong-code inputs.
func TestIntentHint(t *testing.T) {
	t.Parallel()

	close4014 := &websocket.CloseError{Code: 4014, Text: "Disallowed intent(s)"}
	close4001 := &websocket.CloseError{Code: 4001, Text: "Unknown opcode"}

	tests := []struct {
		name    string
		err     error
		appID   string
		want    string // substring assertion; empty string means "must be empty"
		wantURL string
	}{
		{
			name:    "nil error",
			err:     nil,
			appID:   "123",
			want:    "",
			wantURL: "",
		},
		{
			name:    "non-close error",
			err:     errors.New("dial tcp: connection refused"),
			appID:   "123",
			want:    "",
			wantURL: "",
		},
		{
			name:    "close 4001",
			err:     close4001,
			appID:   "123",
			want:    "",
			wantURL: "",
		},
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

// TestOnReady_Idempotent verifies that the READY handler closes b.ready
// exactly once even when discordgo re-fires READY after a reconnect.
// Without the sync.Once guard the second invocation would panic on a closed
// channel and crash the gateway listener goroutine.
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
