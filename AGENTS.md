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
- All PR checks (test, lint, CodeQL, docker build) must be green before merge.

## Coding conventions

- Go standard library and well-known packages over hand-rolled code.
- `log/slog` JSON logging.
- Comments explain **why**, not what. Skip obvious narration.
- Keep functions and packages small and focused.
- Prefer env-var configuration; never commit secrets.
