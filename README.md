# linkbot

A Discord bot and HTTP API that **sanitizes URLs**.

Today that means resolving music streaming links (Spotify, Apple Music, YouTube Music, Tidal,
Deezer, etc.) through [Odesli / song.link](https://odesli.co/) so they open on whatever service
the reader uses.

Stack: Go, [chi](https://github.com/go-chi/chi), [discordgo](https://github.com/bwmarrin/discordgo),
`log/slog` JSON logs.

## News-link sanitization

Non-music URLs flow through a host-aware rule engine that strips tracking parameters and unwraps
redirect wrappers. Rule set ported from [timball/Careen](https://github.com/timball/Careen);
hosts currently covered include:

- **Google Workspace** (`docs|drive|sheets|slides|forms|mail|calendar|sites|meet|chat|contacts.google.*`):
  keep `tab`, `gid`, `usp`, `authuser`.
- **Google Search** (`google.<TLD>`): keep `q`, force `udm=14&pws=0` to bypass AI summaries.
- **Amazon** (`amazon.<TLD>`): drop the `/ref=…` path tail plus the entire query.
- **Reddit**, and any unknown host: strip the query string and fragment outright.
- **YouTube** / `youtu.be` / **Twitch**: keep only `v` and/or `t`.
- **Apple News** (`apple.news`): fetch the wrapper page, extract the underlying article URL,
  recurse so the publisher URL is also cleaned.
- **NYTimes**: keep `unlocked_article_code` (gift-link tokens) and drop everything else.
- **Google `search.app`**: follow the first redirect and clean the destination.
- **`admin.cloud.microsoft`**: leave untouched (its query string is real routing state).

## Documentation

For implementation details see the godoc for each package under `lib/`, especially `lib/sanitize`
and `lib/odesli`: <https://pkg.go.dev/github.com/icco/linkbot>.

## API

| Method | Path           | Description                                                           |
|--------|----------------|-----------------------------------------------------------------------|
| `GET`  | `/`            | HTML landing page describing the API and the Discord invite.         |
| `POST` | `/sanitize`    | Body: `{"url": "..."}`. Returns `{"url", "sanitized", "changed"}`.   |
| `GET`  | `/healthcheck` | Liveness probe.                                                       |

```bash
curl -sS -X POST http://localhost:8080/sanitize \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://open.spotify.com/track/1jJci4qxiYcOHhQR247rEU"}'
```

## Environment variables

| Variable            | Required | Default   | Description                                                                |
|---------------------|----------|-----------|----------------------------------------------------------------------------|
| `DISCORD_TOKEN`     | no       | _(empty)_ | Discord bot token. If unset, only the HTTP API runs.                       |
| `DISCORD_CLIENT_ID` | no       | _(empty)_ | Discord application client ID; enables the invite link on the landing page. |
| `PORT`              | no       | `8080`    | HTTP listen port.                                                          |
| `ODESLI_API_KEY`    | no       | _(empty)_ | Odesli API key. The public endpoint works without one but is rate limited. |

## Running

```bash
export DISCORD_TOKEN=...   # optional; HTTP API runs without it
go run .
```

```bash
docker build -t linkbot .
docker run --rm -p 8080:8080 -e DISCORD_TOKEN=... linkbot
```

## Discord setup

1. Create an application and bot at <https://discord.com/developers/applications>.
2. Enable the **Message Content** privileged intent.
3. Invite the bot with the `bot` scope plus the `Send Messages` and `Read Message History`
   permissions.
4. Set `DISCORD_TOKEN` and run linkbot.

## Contributing

See [`AGENTS.md`](./AGENTS.md) for the conventions used by both human and AI contributors,
including the [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/) policy
enforced on every PR.

## Acknowledgements

Thanks to [Tim Ball (`@timball`)](https://github.com/timball) — the news-link rule set in
`lib/sanitize` is a Go port of [timball/Careen](https://github.com/timball/Careen), with the
same host patterns, parameter allowlists, and recursive Apple News / `search.app` handling.
The opinionated paywall-bypass and archive-mirror rules from Careen are deliberately not
ported.
