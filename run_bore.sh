#!/bin/bash
# Self-healing wrapper for the bore tunnel to bore.pub
cd /home/ubuntu/sandbox/proxy
while true; do
  : > /tmp/bore.log
  bore local 1275 --to bore.pub --port 29001 2>&1 | tee -a /tmp/bore.log
  echo "[`date -u +%H:%M:%S`] bore exited, restarting in 2s" >&2
  sleep 2
done
