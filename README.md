# linkbot

A Discord bot and HTTP API that **sanitizes URLs**.

Today that means resolving music streaming links (Spotify, Apple Music, YouTube Music, Tidal,
Deezer, etc.) through [Odesli / song.link](https://odesli.co/) so they open on whatever service
the reader uses.

Stack: Go, [chi](https://github.com/go-chi/chi), [discordgo](https://github.com/bwmarrin/discordgo),
[icco/gutil](https://github.com/icco/gutil) (zap-based JSON logs),
[otelhttp](https://pkg.go.dev/go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp)
+ Prometheus exporter (`/metrics`).

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
| `DISCORD_TOKEN`         | no       | _(empty)_ | Discord bot token. Required for the gateway connection — Discord does not allow OAuth2 client credentials to authorize a gateway. |
| `DISCORD_CLIENT_ID`     | no       | _(empty)_ | Discord application client ID; enables the invite link on the landing page and identifies the app for slash command registration. |
| `DISCORD_CLIENT_SECRET` | no       | _(empty)_ | Discord application OAuth2 client secret. When set together with `DISCORD_CLIENT_ID`, linkbot registers a `/sanitize` slash command at startup. |
| `PORT`                  | no       | `8080`    | HTTP listen port.                                                                                                                 |
| `ODESLI_API_KEY`        | no       | _(empty)_ | Odesli API key. The public endpoint works without one but is rate limited.                                                        |

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
5. Optional: set `DISCORD_CLIENT_ID` and `DISCORD_CLIENT_SECRET` to enable the `/sanitize`
   slash command. Discord's client-credentials grant implicitly authorizes the
   `applications.commands.update` scope used to register application commands; the bot token
   is still required for the gateway connection alongside it.

## Contributing

See [`AGENTS.md`](./AGENTS.md) for the conventions used by both human and AI contributors,
including the [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/) policy
enforced on every PR.
