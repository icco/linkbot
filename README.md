# linkbot

A Discord bot and HTTP API that **sanitizes URLs**. See <https://linkbot.natwelch.com> to install in your discord server.

What "sanitize" means today:

- **Music links** (Spotify, Apple Music, YouTube Music, Tidal, Deezer, …) are resolved through
  [Odesli / song.link](https://odesli.co/) so they open on whatever service the reader uses.
- **Tracking params** (`utm_*`, `fbclid`, `gclid`, …) are stripped via host-aware rules ported
  from [timball/Careen](https://github.com/timball/Careen).
- **Paywalled hosts** (WSJ, FT, Bloomberg, Washington Post, The Atlantic, The New Yorker, …)
  are rewritten through a randomly chosen [archive.today](https://archive.today/) mirror
  (`archive.fo`, `archive.is`, `archive.li`, `archive.md`, `archive.ph`, `archive.today`)
  so readers without a subscription can still open the link, and so we don't pile load
  onto a single mirror. Already-archived URLs and trusted workspace hosts
  (`admin.cloud.microsoft`) pass through untouched.

## Documentation

For implementation details see the godoc for each package under `lib/`, especially `lib/sanitize`
and `lib/odesli`: <https://pkg.go.dev/github.com/icco/linkbot>.

## API

| Method | Path           | Description                                                                                                                |
|--------|----------------|----------------------------------------------------------------------------------------------------------------------------|
| `GET`  | `/`            | HTML landing page describing the API and the Discord invite.                                                               |
| `POST` | `/sanitize`    | Body: `{"url": "..."}`. Returns `{"url", "sanitized", "changed"}`.                                                         |
| `GET`  | `/healthcheck` | Liveness probe.                                                                                                            |
| `GET`  | `/metrics`     | OTel HTTP semconv metrics (e.g. `http_server_request_duration_seconds`) in [Prometheus exposition format](https://prometheus.io/docs/instrumenting/exposition_formats/). |

```bash
curl -sS -X POST http://localhost:8080/sanitize \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://open.spotify.com/track/1jJci4qxiYcOHhQR247rEU"}'
```

## Environment variables

| Variable                | Required | Default   | Description                                                                                                                       |
|-------------------------|----------|-----------|-----------------------------------------------------------------------------------------------------------------------------------|
| `DISCORD_TOKEN`     | no       | _(empty)_ | Discord bot token. Required to start the gateway listener.                              |
| `DISCORD_CLIENT_ID` | no       | _(empty)_ | Discord application/client ID. Enables the invite link on the landing page and registers the `/sanitize` slash command at startup. |
| `PORT`              | no       | `8080`    | HTTP listen port.                                                                       |
| `ODESLI_API_KEY`    | no       | _(empty)_ | Odesli API key. The public endpoint works without one but is rate limited.              |

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
5. Optional: set `DISCORD_CLIENT_ID` to register the `/sanitize` slash command at startup
   (via discordgo's `ApplicationCommandBulkOverwrite`, authenticated by the bot token).

## Contributing

See [`AGENTS.md`](./AGENTS.md) for the conventions used by both human and AI contributors,
including the [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/) policy
enforced on every PR.
