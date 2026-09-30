#!/bin/bash
# Start the exchange (bots on) and the trading screen. Ctrl-C stops both.
# Requires Go 1.24+ and Node.js 20. Does not install anything except frontend
# dependencies, and only when node_modules is missing.

set -eu

root="$(cd "$(dirname "$0")/.." && pwd)"
addr="${ADDR:-127.0.0.1:8080}"
fe_host="${FE_HOST:-127.0.0.1}"
fe_port="${FE_PORT:-5173}"

if ! command -v go >/dev/null 2>&1; then
  echo "dev: go is not on PATH" >&2
  exit 1
fi
if ! command -v npm >/dev/null 2>&1; then
  echo "dev: npm is not on PATH" >&2
  exit 1
fi
if ! command -v curl >/dev/null 2>&1; then
  echo "dev: curl is not on PATH" >&2
  exit 1
fi

if curl -sf "http://${addr}/healthz" >/dev/null 2>&1; then
  echo "dev: something is already serving http://${addr}/healthz" >&2
  echo "dev: stop it, or run the two processes yourself on other ports" >&2
  exit 1
fi

if [ ! -d "$root/frontend/node_modules" ]; then
  echo "dev: installing frontend dependencies"
  (cd "$root/frontend" && npm install)
fi

mkdir -p "$root/backend/sessions"

back_pid=""
front_pid=""

kill_tree() {
  # local: this function recurses, and a global pid would be overwritten by the child call.
  local pid="$1"
  local child
  local children
  if [ -z "$pid" ]; then
    return
  fi
  children="$(pgrep -P "$pid" 2>/dev/null || true)"
  for child in $children; do
    kill_tree "$child"
  done
  kill "$pid" 2>/dev/null || true
}

cleanup() {
  kill_tree "$front_pid"
  kill_tree "$back_pid"
  wait "$front_pid" 2>/dev/null || true
  wait "$back_pid" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

echo "exchange   http://${addr}    (bots on, log in backend/sessions/)"
echo "screen     http://${fe_host}:${fe_port}"
echo "stop       Ctrl-C"

(
  cd "$root/backend"
  exec go run ./cmd/exchanged -addr "$addr" -session-dir "$root/backend/sessions" -bots=true
) &
back_pid=$!

ready=0
i=0
while [ "$i" -lt 80 ]; do
  if curl -sf "http://${addr}/healthz" >/dev/null 2>&1; then
    ready=1
    break
  fi
  if ! kill -0 "$back_pid" 2>/dev/null; then
    echo "dev: the exchange exited before it was ready" >&2
    exit 1
  fi
  i=$((i + 1))
  sleep 0.25
done
if [ "$ready" -ne 1 ]; then
  echo "dev: timed out waiting for http://${addr}/healthz" >&2
  exit 1
fi

(
  cd "$root/frontend"
  exec npm run dev -- --host "$fe_host" --port "$fe_port"
) &
front_pid=$!

# Bash 3.2 has no wait -n. Poll until either process leaves.
while kill -0 "$back_pid" 2>/dev/null && kill -0 "$front_pid" 2>/dev/null; do
  sleep 1
done
