package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestServeListenFailure(t *testing.T) {
	var lc net.ListenConfig
	listener, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Error(err)
		}
	})
	srv := &http.Server{Addr: listener.Addr().String(), ReadHeaderTimeout: time.Second}
	err = serve(t.Context(), srv)
	var opErr *net.OpError
	if !errors.As(err, &opErr) || !strings.Contains(err.Error(), "http serve") {
		t.Fatalf("serve = %v, want wrapped listen error", err)
	}
}

func TestServeCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	srv := &http.Server{Addr: "127.0.0.1:0", ReadHeaderTimeout: time.Second}
	if err := serve(ctx, srv); err != nil {
		t.Fatalf("serve = %v", err)
	}
}
