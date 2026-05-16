#!/usr/bin/env bash
#
# macOS launcher. Starts llama-server natively (Metal) and brings up the
# rest of the stack in Docker via docker-compose.mac.yml. Idempotent —
# safe to re-run.
#
# What it does:
#   1. Ensures .env and searxng/settings.yml exist (via install.sh --no-up).
#   2. Downloads the GGUF model into ./models/ if missing.
#   3. Starts llama-server natively in the background (full Metal offload).
#   4. Waits for the server to load the model.
#   5. docker compose up -d --build (mac override).
#
# Usage:
#   ./mac_start_server.sh
#
# Stop everything:
#   docker compose -f docker-compose.yml -f docker-compose.mac.yml down
#   kill "$(cat .llama-server.pid)" && rm .llama-server.pid

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$REPO_ROOT"

# ---------------------------------------------------------------------------
# Source .env so MODEL_URL / MODEL_FILE / LLAMA_CTX come from the same place
# the bot reads them — keeps /status honest about what's loaded.
#
# Alternative models for the .env (bartowski Qwen2.5-Instruct-GGUF):
#   7B  Q6_K     ~6.2 GB   fastest, still strong quality
#   14B Q4_K_M  ~8.4 GB   default for >= 16 GB unified memory
#   14B Q5_K_M  ~10 GB    higher quality, ~25 tok/s
#   32B Q4_K_M  ~19 GB    very capable, ~10 tok/s on M2 Pro
# ---------------------------------------------------------------------------
if [ -f .env ]; then
  set -a
  # shellcheck disable=SC1091
  . ./.env
  set +a
fi

# LLAMA_NGL in .env is the container value (capped for small GPUs). On Mac
# we want everything on Metal — override unconditionally.
LLAMA_NGL=999
: "${LLAMA_CTX:=4096}"
: "${LLAMA_PORT:=8080}"

LLAMA_PID_FILE=".llama-server.pid"
LLAMA_LOG_FILE="llama-server.log"

require() {
  local cmd="$1" hint="${2:-}"
  if ! command -v "$cmd" >/dev/null 2>&1; then
    echo "Missing required tool: $cmd" >&2
    [ -n "$hint" ] && echo "  → $hint" >&2
    exit 1
  fi
}

require docker
require curl
require llama-server "brew install llama.cpp"

# ---------------------------------------------------------------------------
# 1. .env + searxng/settings.yml via install.sh (it'll exit 1 the first time
#    if .env was just created — the user needs to fill in the token).
# ---------------------------------------------------------------------------
./install.sh --no-up

# ---------------------------------------------------------------------------
# 2. Download model if missing.
# ---------------------------------------------------------------------------
if [ -z "${MODEL_URL:-}" ] || [ -z "${MODEL_FILE:-}" ]; then
  echo "MODEL_URL and MODEL_FILE must be set in .env." >&2
  exit 1
fi
mkdir -p models
if [ ! -f "models/$MODEL_FILE" ]; then
  echo "Downloading $MODEL_FILE ..."
  curl -L --fail --progress-bar -o "models/$MODEL_FILE.tmp" "$MODEL_URL"
  mv "models/$MODEL_FILE.tmp" "models/$MODEL_FILE"
  echo "Model ready."
else
  echo "Model already present: $MODEL_FILE"
fi

# ---------------------------------------------------------------------------
# 3. Start llama-server natively, unless one is already running.
# ---------------------------------------------------------------------------
if [ -f "$LLAMA_PID_FILE" ] && kill -0 "$(cat "$LLAMA_PID_FILE")" 2>/dev/null; then
  echo "llama-server already running (PID $(cat "$LLAMA_PID_FILE"))."
else
  rm -f "$LLAMA_PID_FILE"
  echo "Starting llama-server on 127.0.0.1:$LLAMA_PORT (Metal, ngl=$LLAMA_NGL, ctx=$LLAMA_CTX)..."
  nohup llama-server \
    -m "models/$MODEL_FILE" \
    --host 127.0.0.1 --port "$LLAMA_PORT" \
    -c "$LLAMA_CTX" \
    --jinja -fa on \
    --n-gpu-layers "$LLAMA_NGL" \
    --parallel 1 \
    > "$LLAMA_LOG_FILE" 2>&1 &
  echo $! > "$LLAMA_PID_FILE"
  echo "llama-server PID $(cat "$LLAMA_PID_FILE") — logs: $LLAMA_LOG_FILE"
fi

# ---------------------------------------------------------------------------
# 4. Wait for the model to finish loading (can take 30-60s on first start).
# ---------------------------------------------------------------------------
printf "Waiting for llama-server"
for _ in $(seq 1 180); do
  if curl -sf "http://127.0.0.1:$LLAMA_PORT/health" >/dev/null 2>&1; then
    printf " — ready.\n"
    break
  fi
  if ! kill -0 "$(cat "$LLAMA_PID_FILE")" 2>/dev/null; then
    printf "\n"
    echo "llama-server died during startup. Tail of $LLAMA_LOG_FILE:" >&2
    tail -n 30 "$LLAMA_LOG_FILE" >&2 || true
    rm -f "$LLAMA_PID_FILE"
    exit 1
  fi
  printf "."
  sleep 1
done

# ---------------------------------------------------------------------------
# 5. Bring up the rest of the stack.
# ---------------------------------------------------------------------------
echo "Starting docker compose (mac override)..."
docker compose -f docker-compose.yml -f docker-compose.mac.yml up -d --build

cat <<EOF

All up.

  llama-server   PID $(cat "$LLAMA_PID_FILE") · logs: $LLAMA_LOG_FILE
  docker stack   docker compose -f docker-compose.yml -f docker-compose.mac.yml ps

Stop everything:
  docker compose -f docker-compose.yml -f docker-compose.mac.yml down
  kill "\$(cat $LLAMA_PID_FILE)" && rm $LLAMA_PID_FILE
EOF
