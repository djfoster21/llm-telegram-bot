# Repo guide for AI assistants

This file is a fast onboarding pointer for AI coding assistants working in
this repository. Humans should read `README.md` first.

## What this is

A self-hosted Telegram bot backed by a local LLM (llama.cpp), a SearXNG
meta-search instance, and a small Go "data-api" service that fronts
weather / crypto / FX upstreams. Everything runs in Docker Compose; no
external LLM provider.

## Layout

```
.
├── bot/                       Go module: the Telegram bot
│   ├── main.go
│   └── internal/
│       ├── bot/    bot.go     Telegram handling, streaming, history, summaries
│       ├── config/ config.go  Env-driven config loader
│       ├── llm/    llm.go     llama.cpp OpenAI-compatible client (streaming + tools)
│       ├── store/  store.go   SQLite per-chat history store
│       └── tools/  tools.go   Tool definitions + implementations (search/fetch/weather/crypto/fx)
├── data-api/                  Go module: HTTP service fronting external data sources
│   └── main.go
├── config/
│   ├── system-prompt.txt              System prompt (hot-reloaded)
│   └── user-names.example.json        Per-user display-name overrides (copy to user-names.json)
├── searxng/settings.yml       SearXNG configuration (internal-only)
├── docker-compose.yml         5 services: model-init, llama-server, searxng, data-api, bot
├── .env.example               Canonical env-var list
└── .gitignore
```

The two Go modules are independent; they don't share code, only the wire
protocol (data-api speaks JSON over HTTP, the bot calls it from
`bot/internal/tools`).

## Build & run

Local dev (no docker):

```sh
cd bot      && go build ./...   # builds the bot
cd data-api && go build ./...   # builds the data-api
```

Full stack:

```sh
docker compose up --build
```

There are no unit tests yet. If you add behavior worth testing, place
tests alongside the code (`*_test.go`) and they'll be picked up by
`go test ./...`.

## Conventions

- **Comments are sparse on purpose.** Write a comment only when the *why*
  is non-obvious (hidden constraint, subtle invariant, workaround for a
  known bug). Don't narrate *what* the code does — the names already do.
- **No backwards-compatibility shims.** This is a small project; if you
  change a name or signature, change all the call sites.
- **Errors that hit chat must not leak internals.** Use the
  `genericErrorMsg` constant in `bot/internal/bot/bot.go` for user-facing
  errors; log the real error with `log.Printf`.
- **All tool URLs are user-influenced.** The LLM can be prompt-injected
  into calling `fetch_url` with arbitrary URLs. The `fetch_url` client
  in `bot/internal/tools/tools.go` is SSRF-guarded — preserve that guard
  if you refactor.
- **Speaker labels are user-controlled.** Telegram `FirstName` /
  `Username` flow into the `[Name] message` envelope sent to the model.
  `sanitizeSpeaker` in `bot.go` strips brackets, newlines, control chars
  and caps length. Don't bypass it.

## Important runtime behavior

- **History.** Every non-command message in an allowed chat is archived
  to SQLite (`store.go`). Per turn, the bot trims the archived history
  to fit `HISTORY_TOKEN_BUDGET` (defaults to 2500) before forwarding to
  the model. The oldest portion of very long chats is compressed into a
  single `[CONTEXTO ANTERIOR]` summary message.
- **Inflight lock.** Only one inference at a time per chat
  (`claimInflight`). A user message arriving while inference is running
  gets a polite "still thinking" reply; a background auto-summary in
  flight is *cancelled* so the user isn't stuck behind it.
- **Spontaneous replies + reactions.** In groups, the bot keeps a
  per-chat counter; every `randomThreshold()` messages it generates an
  unprompted "take." Independently, pattern matches on incoming text
  trigger emoji reactions with `reactionProbability`.

## Where to make changes

| You want to… | Edit |
|---|---|
| Change the bot's persona / language / style | `config/system-prompt.txt` |
| Add a new tool the model can call | `bot/internal/tools/tools.go` (definition + execute branch) |
| Tune sampling, max tokens | `bot/internal/llm/llm.go` |
| Add an env var | `bot/internal/config/config.go` and `.env.example` |
| Change auto-summary thresholds | constants at the top of `bot/internal/bot/bot.go` |
| Add a data source (weather/etc.) | `data-api/main.go` plus a tool in `tools.go` calling it |

## Things to be careful about

- **Don't add a new outbound HTTP client without an SSRF guard if the
  URL is influenced by the model or user.** The pattern is in
  `newFetchClient()` — copy it.
- **Never log the Telegram bot token.** The token grants full control of
  the bot.
- **`searxng/settings.yml` is owned by uid 977 (the SearXNG container).**
  You can read it but not write it without `sudo`. To edit, copy to a
  writable path, modify, then `sudo cp` back.
- **`config/user-names.json` and `data/` are gitignored.** They hold
  personal data — don't add them to commits.
