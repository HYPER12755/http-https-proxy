#!/bin/bash
# Self-healing wrapper for the Go forward proxy on :1275
cd /home/ubuntu/sandbox/proxy
while true; do
  ./proxy
  echo "[`date -u +%H:%M:%S`] proxy exited (rc=$?), restarting in 2s" >&2
  sleep 2
done
