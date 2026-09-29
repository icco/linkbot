# AGENTS.md

Guidance for coding agents and contributors working on linkbot.

## Conventional Commits

Every commit and PR title **must** strictly follow [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/) (`feat`, `fix`, `docs`, `refactor`, `perf`, `test`, `build`, `ci`, `chore`, `revert`, `style`).
- Imperative, lowercase subject, no trailing period. Example: `feat(api): add sanitize endpoint`.
- Always develop in a branch; PR checks (test, lint, CodeQL, docker build, formatting) must pass before merge.

## Architecture & Layout

- `main.go` — Server entry point and root context shutdown handling.
- `lib/api` — HTTP handlers and Chi v5 router.
- `lib/careen` — URL sanitization, paywall rules, and archive mirrors.
- `lib/sanitize` — URL extraction and routing between Careen and Odesli.
- `lib/discord` — Gateway events, slash commands, and reply deduplication.
- `lib/config` — Environment variable validation and startup configuration.

## Coding Conventions

- **Logging**: Use `go.icco.me/gutil/logging` (Zap + Zapdriver). Loggers travel via `context.Context` (`logging.NewContext` / `logging.FromContext`). Use structured `*w` methods.
- **Errors**: Wrap with short context: `fmt.Errorf("operation: %w", err)`. Use `writeError(r, w, status, err)` in API handlers. Wrap `defer resp.Body.Close()` in a closure checking errors.
- **HTTP Server**: Chi v5 with middleware order: `RequestID` → `ClientIPFromRemoteAddr` → logger injection/request logging → `Recoverer` → `Timeout`. Chi's deprecated `RealIP` trusts arbitrary forwarding headers; use the peer address unless trusted proxies are explicitly configured. Always set explicit HTTP server timeouts. Handlers return JSON via local `writeJSON` / `writeError`.
- **Discord**: Inherit the startup context in event handlers and pass it to every REST call. Acknowledge slash commands before external lookups; unchanged URLs must not produce a public message. Deduplicate exact extracted URLs, not substrings, and fetch history at most once per source message.
- **External APIs**: Separate package per service with functional options (`WithAPIKey`, `WithHTTPClient`). Always take `context.Context` first. Limit all inbound response reads (`io.LimitReader`).
- **Sanitization Invariants**:
  - `sanitize.FindURLs` is the single source of truth for URL extraction.
  - `sanitize.Changed(before, after)` must be used to check URL rewrites (never inline string compare).
  - Paywall routing selects a random `archive.today` mirror per call (`pickArchiveMirror`).
  - Hosts with `noArchive: true` opt out of paywall archiving.
- **Comments**: Keep godoc comments concise (1–3 lines).

## Commands & Verification

```sh
go test -race ./... # Run tests with the race detector
golangci-lint run   # Use v2.14.0 and the checked-in .golangci.yml
go build .         # Build binary
go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
```
