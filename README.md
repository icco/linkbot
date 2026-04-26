# linkbot

A Discord bot and HTTP API that **sanitizes URLs**.

For now "sanitize" means one thing: when a music streaming link
(Spotify, Apple Music, YouTube Music, Tidal, etc.) is posted, linkbot
asks [Odesli / song.link](https://odesli.co/) for the universal
`song.link` URL and replies with that instead. Future PRs will
broaden sanitization to non-music links (tracking parameters, AMP,
shortener unwrapping, etc.) — the package is sketched for that, but
nothing else is implemented yet.

Stack: **Go**, **chi** (HTTP), **discordgo** (gateway), **log/slog** (JSON logs).

## What's implemented

- Discord bot: detects URLs in messages and replies with sanitized versions.
- HTTP API: `POST /sanitize` and `GET /healthz`.
- Music-link sanitization through Odesli (Spotify, Apple Music, YouTube
  Music, Tidal, Deezer, Amazon Music, SoundCloud, etc. — see
  `lib/sanitize/sanitize.go` for the full host list).

## Not implemented (future work)

The `lib/sanitize` package has the seam for the items below; the actual
rewriting logic lands in follow-up PRs.

- Strip tracking parameters (`utm_*`, `fbclid`, `gclid`, `igshid`, `mc_cid`, …).
- Unwrap Google AMP URLs to the canonical page.
- Follow common URL shorteners (`t.co`, `bit.ly`, `goo.gl`, `tinyurl.com`, …).
- Canonicalize trailing slashes and default ports.
- Per-guild opt-in / opt-out for the Discord bot.
- Slash command for explicit `/sanitize <url>`.

## Features

- Discord bot listens for messages, finds URLs, and replies with sanitized versions.
- HTTP API exposes `POST /sanitize` so any other service can clean a URL.
- Single binary, configured entirely through environment variables.
- Multi-stage Alpine Docker image.

## API

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/sanitize` | Body: `{"url": "..."}`. Returns `{"url", "sanitized", "changed"}`. |
| `GET` | `/healthz` | Liveness probe. |

```bash
curl -sS -X POST http://localhost:8080/sanitize \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://open.spotify.com/track/1jJci4qxiYcOHhQR247rEU"}'
```

```json
{
  "url": "https://open.spotify.com/track/1jJci4qxiYcOHhQR247rEU",
  "sanitized": "https://song.link/s/1jJci4qxiYcOHhQR247rEU",
  "changed": true
}
```

## Environment variables

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `DISCORD_TOKEN` | no | _(empty)_ | Discord bot token. If unset, only the HTTP API runs. |
| `PORT` | no | `8080` | HTTP listen port. |
| `ODESLI_API_KEY` | no | _(empty)_ | Odesli API key. The public endpoint works without one but is rate limited. |
| `ODESLI_USER_COUNTRY` | no | _(empty)_ | ISO 3166-1 alpha-2 country code passed to Odesli (e.g. `US`). |

## Repository layout

```
linkbot/
├── lib/
│   ├── api/        # chi HTTP router and handlers
│   ├── config/     # env-var loading
│   ├── discord/    # discordgo bot wiring
│   ├── odesli/     # song.link API client
│   └── sanitize/   # URL rewriting (music today, more later)
├── Dockerfile
└── main.go
```

## Running

### Local

```bash
export DISCORD_TOKEN=...   # optional
go run .
```

### Docker

```bash
docker build -t linkbot .
docker run --rm -p 8080:8080 -e DISCORD_TOKEN=... linkbot
```

## Discord setup

1. Create an application and bot at <https://discord.com/developers/applications>.
2. Enable the **Message Content** privileged intent.
3. Invite the bot with the `bot` scope and the `Send Messages` and `Read Message History` permissions.
4. Set `DISCORD_TOKEN` and run linkbot.

## Contributing

All work happens in feature branches and lands via PR. PR titles and any
commits that hit `main` must follow [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/);
the `PR title` GitHub Action enforces this on every pull request.
See [`AGENTS.md`](./AGENTS.md) for the full set of conventions used by
both human and AI contributors.
