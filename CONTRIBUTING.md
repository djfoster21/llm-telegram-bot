# Contributing

Thanks for considering a contribution. This is a small personal project,
but issues and PRs are welcome — bug reports especially.

## Reporting bugs

Open a GitHub issue with:

- What you ran (env, model, command, message)
- What you expected
- What happened (full bot log if you can — strip the Telegram token first)
- Any relevant snippet of your `.env` (again, never the token)

## Reporting security issues

**Do not open a public issue for security vulnerabilities.** Open a
private security advisory on the GitHub repository, or contact the
maintainer privately through the email address listed on their GitHub
profile. Vulnerabilities to be aware of in this kind of bot:

- Prompt-injection paths through tool calls (`fetch_url`, `search_web`)
- Anything that lets an unauthorized user trigger inference
- SSRF / data exfiltration via the LLM's tool use
- Leakage of the Telegram bot token, internal hostnames, or chat history

## Development setup

```sh
git clone <your-fork>
cd llm-telegram-bot
cp .env.example .env
# Fill in TELEGRAM_BOT_TOKEN and ALLOWED_USER_IDS (use a throwaway bot
# while developing).
docker compose up --build
```

For faster iteration on the bot itself, you can run it outside Docker
once the other services are up:

```sh
docker compose up llama-server searxng data-api
cd bot
LLAMA_BASE_URL=http://localhost:8080 \
SEARXNG_URL=http://localhost:8081 \
DATA_API_URL=http://localhost:8082 \
DB_PATH=./bot.db \
SYSTEM_PROMPT_PATH=../config/system-prompt.txt \
USER_NAMES_PATH=../config/user-names.json \
TELEGRAM_BOT_TOKEN=... ALLOWED_USER_IDS=... \
go run .
```

You'll need to publish the relevant service ports in `docker-compose.yml`
to reach them from the host (they're internal-only by default — that's
deliberate for production, but you can toggle it for dev).

## Code style

This codebase is intentionally minimal. A few conventions:

- **Comments only when the *why* is non-obvious.** Hidden constraints,
  workarounds, subtle invariants — yes. Restating what the code does — no.
- **No backwards-compat shims.** The project is small; when you rename
  something, rename it everywhere.
- **Errors that reach chat must not leak internals.** Use the
  `genericErrorMsg` constant in `bot/internal/bot/bot.go` and log the
  real error with `log.Printf`.
- **Standard Go formatting.** `gofmt -s` and `go vet ./...` should both
  be clean before you commit.

## Adding a new tool

1. Add a `llm.Tool` entry in `Registry.Definitions()` in
   `bot/internal/tools/tools.go` (name, description, JSON-schema params).
2. Add a case to the switch in `Registry.Execute()` that unmarshals the
   arguments and calls your handler.
3. If your tool makes outbound HTTP calls, decide whether the URL is
   model-influenced. If so, copy the SSRF-guarded client pattern from
   `newFetchClient()` instead of using `r.http`.
4. Update the system prompt in `config/system-prompt.txt` if the model
   needs a nudge about when to call your tool.

## Submitting changes

- Branch off `main`, keep PRs focused, one logical change per PR.
- Run `go build ./...` and `go vet ./...` in both `bot/` and `data-api/`.
- A short description of *why* in the PR body matters more than what —
  the diff already shows what changed.
- If your change touches anything security-relevant (auth, SSRF, prompt
  building, the tool registry), say so explicitly so it can be reviewed
  with that lens.

## License

By contributing, you agree that your contributions are licensed under
the same terms as the rest of the project.
