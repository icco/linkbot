// Package logctx propagates a structured slog.Logger through context.Context.
//
// This mirrors the convention from icco/gutil's logging package but uses the
// standard library's log/slog instead of zap.
package logctx

import (
	"context"
	"log/slog"
)

// ctxKey is the unexported context.Context key under which the logger is
// stored. Using an empty struct type prevents collisions with keys from other
// packages, per the context package guidelines.
type ctxKey struct{}

// New returns a copy of ctx that carries log.
func New(ctx context.Context, log *slog.Logger) context.Context {
	if log == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, log)
}

// With returns a copy of ctx that carries the logger from ctx decorated with
// the given attributes. If no logger is in ctx, slog.Default() is decorated.
func With(ctx context.Context, args ...any) context.Context {
	return New(ctx, From(ctx).With(args...))
}

// From returns the logger stored in ctx. If none is present it falls back to
// slog.Default() so handlers can always log without a nil check.
func From(ctx context.Context) *slog.Logger {
	if ctx == nil {
		return slog.Default()
	}
	if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}
