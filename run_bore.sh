#!/usr/bin/env bash
# Self-healing bore tunnel: tunnels local :1275 to a public port on bore.pub.
# Output (stdout/stderr + restart notices) is captured by the launcher to bore.log.
cd "$(dirname "$0")"

: "${BORE_SERVER:=bore.pub}"
: "${BORE_PORT:=29001}"   # pinned public port (stable endpoint)

while true; do
  bore local 1275 --to "$BORE_SERVER" --port "$BORE_PORT"
  echo "[$(date -u +%H:%M:%S)] bore exited rc=$?, restarting in 2s"
  sleep 2
done
