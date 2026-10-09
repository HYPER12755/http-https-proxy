#!/usr/bin/env bash
# Self-healing forward proxy on :1275.
# Output (stdout/stderr + restart notices) is captured by start.sh -> proxy.log.
cd "$(dirname "$0")"

PROXY_BIN="$(cd "$(dirname "$0")" && pwd)/proxy"

while true; do
  "$PROXY_BIN"
  echo "[$(date -u +%H:%M:%S)] proxy exited rc=$?, restarting in 2s"
  sleep 2
done
