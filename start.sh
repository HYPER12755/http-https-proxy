#!/usr/bin/env bash
#
# start.sh — bootstrap + run the forward proxy (:1275) behind a public
#             bore tunnel (bore.pub:29001). Both sides are self-healing
#             and launched detached (survive this script exiting).
#
# Usage:
#   ./start.sh            # (re)install missing bits, start loops, verify
#   ./start.sh stop       # stop the proxy + bore loops
#   ./start.sh status     # show running status + tunnel endpoint
#
# bore is auto-installed via the best available package manager if missing
#   (order: already-in-PATH -> npm -> cargo -> direct GitHub binary).
#
set -euo pipefail

PROXY_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$PROXY_DIR"

BORE_SERVER="${BORE_SERVER:-bore.pub}"
BORE_PORT="${BORE_PORT:-29001}"        # pinned public port (stable endpoint)
PROXY_LOG="$PROXY_DIR/proxy.log"
BORE_LOG="$PROXY_DIR/bore.log"

log()  { printf '%s\n' "$*"; }
have() { command -v "$1" >/dev/null 2>&1; }
is_bore_ok() { have bore && bore --version >/dev/null 2>&1; }

# ---------------------------------------------------------------------------
# ensure bore is available (auto-install if missing)
# ---------------------------------------------------------------------------
ensure_bore() {
  if is_bore_ok; then
    log "bore present: $(command -v bore)"; return 0
  fi
  if have npm; then
    log "installing bore via npm (npm install -g bore-cli)..."
    if npm install -g bore-cli 2>/dev/null && is_bore_ok; then
      log "bore installed via npm"; return 0
    fi
  fi
  if have cargo; then
    log "installing bore via cargo (cargo install --locked bore-cli)..."
    if cargo install --locked bore-cli 2>/dev/null && is_bore_ok; then
      log "bore installed via cargo"; return 0
    fi
  fi
  log "installing bore via direct download (official release)..."
  install_bore_binary
}

install_bore_binary() {
  local arch artifact url tmp
  arch="$(uname -m)"
  case "$arch" in
    x86_64|amd64) artifact="bore-v0.6.0-x86_64-unknown-linux-musl.tar.gz" ;;
    aarch64|arm64) artifact="bore-v0.6.0-aarch64-unknown-linux-musl.tar.gz" ;;
    armv7l) artifact="bore-v0.6.0-armv7-unknown-linux-musl.tar.gz" ;;
    *) log "ERROR: unsupported arch '$arch' for bore binary"; return 1 ;;
  esac
  url="https://github.com/ekzhang/bore/releases/download/v0.6.0/${artifact}"
  tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' RETURN
  curl -fsSL "$url" -o "$tmp/bore.tar.gz"
  tar -xzf "$tmp/bore.tar.gz" -C "$tmp"
  sudo mkdir -p /usr/local/bin
  sudo cp "$tmp/bore" /usr/local/bin/bore
  sudo chmod +x /usr/local/bin/bore
  is_bore_ok && log "bore installed via direct download -> /usr/local/bin/bore" \
    || { log "ERROR: bore binary failed to run"; return 1; }
}

# ---------------------------------------------------------------------------
# ensure the proxy binary is built
# ---------------------------------------------------------------------------
ensure_proxy() {
  if [ -x "$PROXY_DIR/proxy" ]; then
    log "proxy binary present"
  else
    log "building proxy from source (go build)..."
    go build -o "$PROXY_DIR/proxy" .
  fi
}

# kill a loop + its child (each loop is its own process group via setsid)
_kill_group() {
  local pid="$1"
  [ -z "$pid" ] && return 0
  kill -9 -"$pid" 2>/dev/null || kill -9 "$pid" 2>/dev/null || true
}

stop_loops() {
  # via pidfiles (primary)
  for pf in proxy.pid bore.pid; do
    if [ -f "$PROXY_DIR/$pf" ]; then
      _kill_group "$(cat "$PROXY_DIR/$pf" 2>/dev/null || true)"
      rm -f "$PROXY_DIR/$pf"
    fi
  done
  # fallbacks for strays (basename patterns; exclude haproxy)
  pkill -9 -f '[/]run_proxy\.sh' 2>/dev/null || true
  pkill -9 -f '[/]run_bore\.sh'   2>/dev/null || true
  pkill -9 -f 'bore local 1275'   2>/dev/null || true
  pkill -9 -f '[^a-zA-Z0-9_]proxy$' 2>/dev/null || true
  sleep 2
}

# launch a loop detached; record its pid (and process-group leader)
launch() {
  local script="$1" logfile="$2" pidfile="$3"
  nohup setsid "$PROXY_DIR/$script" </dev/null >>"$PROXY_DIR/$logfile" 2>&1 &
  local pid=$!
  echo "$pid" > "$PROXY_DIR/$pidfile"
  log "$script -> pid $pid (logs: $logfile)"
}

wait_http() { # port url max
  local url="$1" max="$2" i
  for i in $(seq 1 "$max"); do
    curl -fsS -m 2 "$url" >/dev/null 2>&1 && return 0
    sleep 1
  done
  return 1
}

# ---------------------------------------------------------------------------
cmd_start() {
  ensure_bore
  ensure_proxy
  stop_loops

  log "starting proxy loop on :1275"
  launch run_proxy.sh proxy.log proxy.pid
  wait_http "http://localhost:1275/health" 20 || log "WARN: proxy not responding on :1275"

  log "starting bore tunnel -> $BORE_SERVER:$BORE_PORT"
  launch run_bore.sh bore.log bore.pid
  for i in $(seq 1 30); do
    grep -q 'listening at' "$BORE_LOG" 2>/dev/null && break
    sleep 1
  done

  log "=== STATUS ==="
  if curl -fsS -m 3 http://localhost:1275/health >/dev/null 2>&1; then
    log "proxy :1275            -> UP (health=OK)"
  else
    log "proxy :1275            -> DOWN"; tail -n 8 "$PROXY_LOG"
  fi
  if grep -q 'listening at' "$BORE_LOG" 2>/dev/null; then
    log "tunnel $BORE_SERVER:$BORE_PORT -> UP"
    grep -oE 'bore\.pub:[0-9]+' "$BORE_LOG" | tail -1
  else
    log "tunnel $BORE_SERVER:$BORE_PORT -> starting/blocked"; tail -n 8 "$BORE_LOG"
  fi
}

cmd_stop() {
  stop_loops
  log "stopped proxy + bore loops"
}

cmd_status() {
  curl -fsS -m 3 http://localhost:1275/health >/dev/null 2>&1 \
    && log "proxy :1275            -> UP" || log "proxy :1275            -> down"
  if [ -f "$BORE_LOG" ] && grep -q 'listening at' "$BORE_LOG"; then
    log "tunnel                  -> UP"; grep -oE 'bore\.pub:[0-9]+' "$BORE_LOG" | tail -1
  else
    log "tunnel                  -> down/not-logged"
  fi
}

case "${1:-start}" in
  start)  cmd_start ;;
  stop)   cmd_stop ;;
  status) cmd_status ;;
  *) log "unknown command: $1 (start|stop|status)"; exit 2 ;;
esac
