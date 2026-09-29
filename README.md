# linkbot

A Discord bot and HTTP API that **sanitizes URLs**. See <https://linkbot.natwelch.com> to install in your discord server.

What "sanitize" means today:

- **Music links** (Spotify, Apple Music, YouTube Music, Tidal, Deezer, …) are resolved through
  [Odesli / song.link](https://odesli.co/) so they open on whatever service the reader uses.
- **Tracking params** (`utm_*`, `fbclid`, `gclid`, …) are stripped via host-aware rules ported
  from [timball/Careen](https://github.com/timball/Careen). Hosts without a specific rule
  lose all query parameters and fragments; this can also remove functional parameters.
- **Paywalled hosts** (WSJ, FT, Bloomberg, Washington Post, The Atlantic, The New Yorker, …)
  are rewritten through a randomly chosen [archive.today](https://archive.today/) mirror
  (`archive.fo`, `archive.is`, `archive.li`, `archive.md`, `archive.ph`, `archive.today`)
  so readers without a subscription can still open the link, and so we don't pile load
  onto a single mirror. Already-archived URLs and trusted workspace hosts
  (`admin.cloud.microsoft`) pass through untouched.

## Documentation

For implementation details see the godoc for each package under `lib/`, especially `lib/sanitize`
and `lib/careen`: <https://pkg.go.dev/go.icco.me/linkbot>. The Odesli client lives in its own
repo: <https://pkg.go.dev/github.com/icco/odesli>.

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

`POST /sanitize` accepts one JSON object up to 16 KiB with an absolute HTTP(S) URL.
Invalid input returns `400`, oversized bodies return `413`, and upstream failures return `502`.

## Environment variables

| Variable                | Required | Default   | Description                                                                                                                       |
|-------------------------|----------|-----------|-----------------------------------------------------------------------------------------------------------------------------------|
| `DISCORD_TOKEN`     | no       | _(empty)_ | Discord bot token. Required to start the gateway listener.                              |
| `DISCORD_CLIENT_ID` | no       | _(empty)_ | Discord application/client ID. Enables the invite link on the landing page and registers the `/sanitize` slash command at startup. |
| `PORT`              | no       | `8080`    | HTTP listen port.                                                                       |
| `ODESLI_API_KEY`    | no       | _(empty)_ | Odesli API key. The public endpoint works without one but is rate limited.              |

## Running

Requires Go 1.27.1 or later.

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

The bot only posts when sanitization changes a URL. Unchanged links produce no reply,
including when using `/sanitize`.

## Contributing

See [`AGENTS.md`](./AGENTS.md) for the conventions used by both human and AI contributors,
including the [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/) policy
enforced on every PR.

```sh
go test -race ./...
golangci-lint run # v2.14.0; configuration is in .golangci.yml
go build .
go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
```

Update Go dependencies with `go get -u -t ./...` followed by `go mod tidy`.
Dependabot checks Go modules, Docker images, and GitHub Actions weekly. The landing
page's Web Vitals import and the CI tool versions are pinned and updated separately.

## License

**GPL-3.0.** See [`LICENSE`](./LICENSE).

This is the one repo of mine that isn't MIT, and the reason is `lib/careen`: its host rules and
strategies are a Go port of [timball/Careen](https://github.com/timball/Careen) (©Tim Ball), which
is GPL-3.0. A port of that rule set is a derivative work, so linkbot inherits the licence.

If you want to reuse a piece of this under a permissive licence, the parts that never touched
careen have already been split out — see [icco/odesli](https://github.com/icco/odesli), which is
MIT.
