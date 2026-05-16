# Ideas backlog

Things we'd like to do but aren't doing yet. Add freely; trim when done or
when an idea goes stale.

## Hot-reload tunables without container restart

Today, env changes need `docker compose up -d bot` to recreate the container.
That's fast (no rebuild) but it drops any in-flight inference and forces the
prompt cache to warm back up. A true hot-reload path would mean changing a
sampling param applies on the next turn with no container restart at all.

### Sketch

1. Move tunables into a dedicated `config/tunables.env` (or `.toml`) file that
   is volume-mounted into the bot container (read-only), separate from
   secrets in `.env`. The bot parses this file at startup using the same
   helpers in `config.go`.
2. Spawn a small watcher goroutine in `main.go` that polls the file's mtime
   (or uses `fsnotify`) and re-parses on change.
3. Replace `b.cfg *config.Config` with `b.cfgPtr atomic.Pointer[config.Config]`
   and an accessor `b.cfg() *config.Config`. All current `b.cfg.Field` reads
   become `b.cfg().Field`. Concurrent readers are fine because the pointer
   swap is atomic; in-flight inferences keep using their captured snapshot.
4. For LLM sampling specifically (`llmClient.MaxTokens`, etc.), the `Client`
   struct also needs to read from an atomic pointer or be re-set on reload.

### Why it's nice

- Tuning Temperature / TopP during a session no longer requires bouncing the
  container. Faster iteration on persona tweaks and sampling.
- The prompt cache stays warm across config edits — only the actual sampler
  parameters change.
- Streaming a long reply isn't interrupted by a config tweak.

### Why we're not doing it now

- Adds ~50 lines of plumbing and a goroutine for a marginal win — most edits
  during development still come with code changes that need a rebuild
  anyway, so the workflow is the same.
- The atomic-pointer dance is correct but easy to get wrong if someone later
  adds a long-lived reference to `*Config` somewhere.

### When to revisit

Pick this up if we start tuning sampling params on a live bot with users
talking to it, or if cache-warm prompt eval becomes the dominant per-turn
cost again.
