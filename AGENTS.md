# AGENTS.md

Guidance for AI coding agents and human contributors working in this repo.

## Conventional Commits — required everywhere

Every PR title and every commit that lands on `main` **must** follow the
[Conventional Commits 1.0.0](https://www.conventionalcommits.org/en/v1.0.0/) spec.

```
<type>(<optional scope>): <short imperative summary>

<optional body>

<optional footer(s)>
```

### Allowed types

| Type | Use for |
|------|---------|
| `feat` | A new user-visible feature. |
| `fix` | A bug fix. |
| `docs` | Documentation only. |
| `refactor` | Code change that neither fixes a bug nor adds a feature. |
| `perf` | Performance improvement. |
| `test` | Adding or fixing tests. |
| `build` | Build system, Dockerfile, or dependency changes. |
| `ci` | CI configuration changes (`.github/workflows/...`). |
| `chore` | Tooling, housekeeping, no production code change. |
| `revert` | Reverts a previous commit. |
| `style` | Formatting, whitespace, comments — no code behavior change. |

### Rules

- Subject is imperative, present-tense, lowercase, no trailing period, ≤ 72 chars.
- Use a scope when it clarifies, e.g. `feat(discord): reply with sanitized URL`.
- Append `!` after the type/scope (or add a `BREAKING CHANGE:` footer) for breaking changes.
- Body explains the **why**; wrap at 100 chars.
- Reference issues in the footer: `Refs: #12`, `Closes: #34`.

### Examples

```
feat(api): add POST /sanitize endpoint
fix(odesli): handle 429 rate-limit responses
docs: document conventional-commit policy
ci: build Docker image on every PR
refactor(sanitize)!: rename Sanitizer.URL to Sanitizer.Clean

BREAKING CHANGE: callers must update to Clean(ctx, url).
```

### Branches and PRs

- Always work in a feature branch — never commit directly to `main`.
- The PR title is the commit message that lands on `main`, so it must
  itself follow Conventional Commits. Squash-merge is the default.
- All PR checks (test, lint, CodeQL, docker build, yaml/json format,
  conventional-commits) must be green before merge.
- Prefer multiple small commits with focused subjects over one giant
  commit; each commit subject must still be a valid Conventional Commit.

### Reviewing and addressing feedback

- When addressing PR review comments:
  1. Make the fix, push, and reply inline to the original comment with a
     short note that points at the commit SHA.
  2. **Resolve the review thread** in the same step (GitHub's GraphQL
     `resolveReviewThread` mutation — the REST API has no equivalent).
- If a review comment is ambiguous, propose your interpretation in the
  inline reply and offer to redo it differently rather than guessing
  silently.

## Coding conventions

### Style and structure

- Go standard library and well-known packages over hand-rolled code.
- `log/slog` JSON logging — never `fmt.Println`, never `log` from the
  standard library directly.
- Keep functions and packages small and focused. Prefer constructor
  functions (`New`) over zero-value initialization for types with
  invariants.
- One package per directory under `lib/`; the entry point lives in
  `main.go` at the repo root.
- Functions are **never** one-liners. Even trivial setters and option
  helpers get their own `{` / body / `}` lines for readability.
- `gofmt -s` clean. `go vet ./...` clean. `golangci-lint run` clean.

### Comments and godocs

- **Every** function — exported and unexported — gets a godoc comment
  starting with the function name.
- Every exported type, const, and var gets a godoc comment starting with
  its identifier. Unexported package-level identifiers get one when
  intent isn't obvious from the name.
- Every package gets a `// Package <name> ...` comment on the `package`
  line of one file (typically the package's primary file).
- **Keep all comments and godocs concise and short.** One line where
  possible; two or three at most. If you find yourself writing a
  multi-paragraph godoc, the prose probably belongs in a README,
  design doc, or PR description instead.
- Comments explain **why** and call out non-obvious tradeoffs or
  invariants. Skip obvious narration like `// increment i` or
  `// return the result`.
- Use `TODO(<future PR or issue>):` for known follow-ups so they are
  greppable.

### Logging

- Use `github.com/icco/gutil/logging` (Zap + Zapdriver). Loggers
  travel via `context.Context`, not via struct fields:
  - `logging.NewContext(ctx, log, "k", v, ...)` attaches a logger,
    optionally with extra fields.
  - `logging.FromContext(ctx)` retrieves the request/event logger.
  - Use the `*w` methods (`Infow`, `Warnw`, `Errorw`, `Debugw`) for
    structured key/value pairs and `zap.Error(err)` for errors.
- HTTP handlers get a request-scoped logger via `logging.Middleware`
  in `lib/api`, decorated with chi's `request_id`.
- Discord handlers create a per-event context with channel/message/
  author IDs attached before doing work.

### Error handling

- Wrap with `fmt.Errorf("context: %w", err)` so callers can `errors.Is`
  / `errors.As` the underlying cause. Always include a short prefix that
  identifies the operation that failed.
- API responses must always log the underlying error in addition to
  returning it to the client. Use `writeError(r, w, status, err)` in
  `lib/api`, which does both.
- The `error` parameter in helper signatures is preferred over a bare
  `string` so callers cannot accidentally drop a wrapped error.
- `defer resp.Body.Close()` must be wrapped in a closure that logs the
  close error (otherwise `errcheck` complains).

### HTTP server

- Routing via `github.com/go-chi/chi/v5`. Use `chi.NewRouter()` plus
  `middleware.RequestID`, `middleware.RealIP`, the project's
  `loggerMiddleware`, `middleware.Recoverer`, and `middleware.Timeout`
  in that order.
- All endpoints return JSON. Use the package-local `writeJSON` and
  `writeError` helpers; do not call `json.NewEncoder(w).Encode` directly
  in handlers.
- HTTP server timeouts are mandatory: set `ReadHeaderTimeout`,
  `ReadTimeout`, `WriteTimeout`, `IdleTimeout` on every `http.Server`.

### Configuration

- Configuration comes from environment variables, loaded once at
  startup by `lib/config`.
- Validate aggressively in `Load()`: a misconfigured deployment should
  fail loudly at boot, not at first request.
- Default the port to `8080` when `PORT` is unset; reject ports outside
  `1..65535` with a clear error.
- When a value is unlikely to vary across deployments today, prefer a
  package-level `const` over an env var (e.g. `config.UserCountry`).
  Add a comment explaining the future migration path.
- Never commit secrets. `.env` files and credential JSON belong in
  `.gitignore`.

### External APIs

- Each third-party API gets its own package under `lib/` with a
  `Client` type, an `Option` functional-option pattern (`WithAPIKey`,
  `WithBaseURL`, `WithHTTPClient`, etc.), and a `New(...)` constructor.
- Always pass `context.Context` as the first argument to outbound
  requests; build them with `http.NewRequestWithContext`.
- Default to `http.Client{Timeout: 15 * time.Second}` unless the API's
  documented latency justifies otherwise.
- Set a descriptive `User-Agent` (`linkbot/<version> (+repo URL)`).
- Truncate any error body before logging or surfacing it (see
  `odesli.truncate`) so a giant HTML 500 page doesn't blow up logs.

### Concurrency and shutdown

- Use `signal.NotifyContext(context.Background(), os.Interrupt,
  syscall.SIGTERM)` to derive the root cancellation context.
- Long-running goroutines (HTTP server, Discord gateway) must shut down
  via that context with a bounded grace period — currently 10 s.

### Sanitization rules

- `sanitize.FindURLs` is the only place URL extraction happens; trim
  trailing punctuation there, not at call sites.
- `sanitize.Changed(before, after)` is the canonical way to check
  whether sanitization actually rewrote a URL. Don't compare strings
  inline.
- The Discord bot must not reply if the sanitized URL already appears
  in the source message or in the last `recentLookback` channel
  messages, to avoid duplicating other bots' or users' work.
- The careen host rules, paywall regex list, and archive mirror set
  are hard-coded package-level vars in `lib/careen`. Add new entries
  there rather than threading them through env vars; they almost
  never vary per deployment.
- Paywall routing picks a random `archive.today` mirror per call
  (`pickArchiveMirror`) to spread load. Tests that need to assert a
  specific URL must pin the mirror via the package-internal
  `pinMirror(t, ...)` helper.
- A rule with `noArchive: true` (e.g. `admin.cloud.microsoft`) opts a
  trusted host out of paywall-archive routing even if some future
  paywall regex would otherwise match.

## CI / repo layout

- `.github/workflows/test.yml` — `go build`, `go test`.
- `.github/workflows/golangci-lint.yml` — `golangci-lint` with
  `bodyclose,misspell,gosec,goconst,errorlint`.
- `.github/workflows/codeql-analysis.yml` — Go CodeQL on push, PR, weekly.
- `.github/workflows/docker.yml` — multi-arch Docker build; pushes to
  `ghcr.io/icco/linkbot` only on `main`.
- `.github/workflows/pr-title.yml` — Conventional Commit PR title check
  via `amannn/action-semantic-pull-request`.
- `.github/workflows/yaml-json.yml` — `yamllint` (via `reviewdog`) and
  `yq`-based formatting; auto-commits formatting fixes on PRs (excluding
  `.github/workflows/**` to avoid lockouts).
- `.yamllint.yml` disables `line-length`, `document-start`, and
  `truthy` to match other icco repos and to tolerate GitHub Actions
  YAML quirks.
- `Dockerfile` is multi-stage: `golang:1.26-alpine` builder →
  `alpine:3.23` runtime, `CGO_ENABLED=0`, non-root `app` user, no
  binaries in the final image other than `linkbot` and required certs.
