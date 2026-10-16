#!/usr/bin/env bash
# Self-healing forward proxy on :1275 (password protected via HTTP Basic auth).
# Credentials: PROXY_USER (default: proxy) and PROXY_PASS (default: generated
# once and persisted in proxy.credentials, mode 600). Both are exported to the
# binary, so restarts keep the same password.
# Output (stdout/stderr + restart notices) is captured by start.sh -> proxy.log.

PROXY_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$PROXY_DIR"

PROXY_BIN="$PROXY_DIR/proxy"
CRED_FILE="$PROXY_DIR/proxy.credentials"

if [ -z "${PROXY_PASS:-}" ] && [ -f "$CRED_FILE" ]; then
  set -a; . "$CRED_FILE"; set +a
fi
: "${PROXY_USER:=proxy}"; export PROXY_USER

if [ -z "${PROXY_PASS:-}" ]; then
  PROXY_PASS="$(head -c 24 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | cut -c1-20)"
  printf 'PROXY_USER=%s\nPROXY_PASS=%s\n' "$PROXY_USER" "$PROXY_PASS" > "$CRED_FILE"
  chmod 600 "$CRED_FILE"
  echo "[$(date -u +%H:%M:%S)] generated proxy credentials user=$PROXY_USER (stored in proxy.credentials)"
fi
export PROXY_PASS

while true; do
  "$PROXY_BIN"
  echo "[$(date -u +%H:%M:%S)] proxy exited rc=$?, restarting in 2s"
  sleep 2
done
