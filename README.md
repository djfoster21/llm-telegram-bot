# llm-telegram-bot

A self-hosted Telegram bot backed by a local LLM (llama.cpp), a self-hosted
SearXNG meta-search instance, and a small data API for structured queries
(weather, crypto prices, currency rates). Everything runs in Docker — no
external LLM provider, no API keys outside Telegram and (optionally) the
search engines SearXNG queries on your behalf.

The bot is built around small group chats: it understands speaker tags,
chimes in with spontaneous takes, reacts with emoji, and auto-summarizes
long conversations so the model keeps context without blowing the token
budget.

## Features

- **Local LLM** — runs any GGUF model via llama.cpp's OpenAI-compatible
  server, with GPU offload (CUDA) when available.
- **Tools the model can call**
  - `search_web` — SearXNG meta-search (DuckDuckGo, Bing, Wikipedia, etc.)
  - `fetch_url` — readability-extracted page text, SSRF-guarded
  - `get_weather` — Open-Meteo current + 2-day forecast
  - `get_crypto_price` — CoinGecko price + 24h change
  - `get_exchange_rate` — Open ER (150+ currencies) plus DolarAPI (all the
    ARS variants: oficial, blue, MEP, CCL, etc.)
- **Group chat support** — allowlist by user ID and/or chat ID,
  per-speaker tagging, optional spontaneous replies, pattern-based emoji
  reactions, auto-summarization of long histories.
- **Streaming UI** — rotating "Thinking…" / "Searching the web…" status
  while the model works; tokens stream in as they arrive.
- **Hot-reloadable config** — system prompt and per-user name overrides
  are re-read on every request.

## Quick start

```sh
git clone <this-repo>
cd llm-telegram-bot
./install.sh
```

The first run of `install.sh` copies `.env.example` to `.env` and exits
so you can fill in `TELEGRAM_BOT_TOKEN` (from `@BotFather`) and
`ALLOWED_USER_IDS`. Re-run it after editing — on the second pass it
generates a fresh `searxng/settings.yml` with a randomly-generated
`secret_key` (kept out of git) and brings up the docker compose stack.

Re-running `install.sh` later is safe — it only generates what's missing.

The `model-init` service downloads the configured GGUF on first run,
then `llama-server` starts and the bot connects to Telegram via long
polling.

## Configuration

All configuration is via `.env`. See `.env.example` for the canonical
list; the most important variables are:

| Variable | Required | Description |
|---|---|---|
| `TELEGRAM_BOT_TOKEN` | yes | Token from `@BotFather`. |
| `ALLOWED_USER_IDS` | yes | Comma-separated Telegram user IDs allowed to DM the bot. Use `@userinfobot` to find yours. |
| `ALLOWED_CHAT_IDS` | no | Group chat IDs where anyone can use the bot. Send `/status@<botname>` in the group to see its ID. |
| `MODEL_URL` / `MODEL_FILE` | yes | GGUF model URL + filename. Defaults to Qwen 2.5 3B Instruct Q4 (~2 GB). |
| `LLAMA_CTX` | no | Context window in tokens. Default 4096. |
| `LLAMA_NGL` | no | GPU layers to offload. Default 24. Set to 0 for CPU-only. |
| `HISTORY_TOKEN_BUDGET` | no | Cap on history tokens forwarded to the model per turn. Default 2500. |

### Per-user name overrides (optional)

Create `config/user-names.json` to override how the bot refers to people:

```json
{ "123456789": "Pibe", "987654321": "Capo" }
```

Hot-reloaded on each message. See `config/user-names.example.json`.

### System prompt

`config/system-prompt.txt` defines the bot's persona, tools, and style.
Edit and the next message picks up the change — no restart needed.

## Bot commands

- `/start` — short intro.
- `/reset` — clears the bot's memory of this chat.
- `/summary` — bot replies with a short recap of the conversation.
- `/status` — shows model name, your user ID, and the chat ID.

## Architecture

Five services in `docker-compose.yml`:

| Service | Purpose |
|---|---|
| `model-init` | One-shot GGUF downloader. Idempotent — exits immediately if the model is already present. |
| `llama-server` | llama.cpp OpenAI-compatible server with `--jinja` for tool calls. |
| `searxng` | Self-hosted meta-search, JSON API enabled, internal-only. |
| `data-api` | Small Go service fronting Open-Meteo / CoinGecko / Open ER / DolarAPI as clean LLM-friendly JSON. |
| `bot` | The Telegram bot itself. |

No service is exposed to the host. The bot reaches the others by their
docker-compose service names on the internal network.

## Hardware notes

The default model (Qwen 2.5 3B Q4) needs ~2 GB of VRAM if you want full
GPU offload, or runs comfortably on CPU. For a 7B Q3/Q4 model on a 2 GB
GPU, expect partial offload (~8 of 29 layers on GPU) and 2–3 tok/s. Raise
`LLAMA_NGL` aggressively only if you have headroom.

## License

MIT. See `LICENSE` (if present).
