#!/usr/bin/env bash
# monitor.sh — launch a 2-pane tmux session for trade-engine
# Left pane:  live log tail (~/.tradeengine/logs/engine.log)
# Right pane: CLI dashboard (te dashboard)

set -euo pipefail

SESSION="trade-engine"
ENGINE_DIR="$(cd "$(dirname "$0")/.." && pwd)"

if tmux has-session -t "$SESSION" 2>/dev/null; then
  echo "Session '$SESSION' already exists — attaching..."
  tmux attach-session -t "$SESSION"
  exit 0
fi

tmux new-session -s "$SESSION" \; \
  send-keys "cd $ENGINE_DIR && tail -f ~/.tradeengine/logs/engine.log" Enter \; \
  split-window -h \; \
  send-keys "cd $ENGINE_DIR && ./te dashboard" Enter \; \
  select-pane -t 0 \; \
  split-window -v \; \
  send-keys "tail -f /tmp/te-switch.log 2>/dev/null || { echo 'waiting for te-switch.log...'; sleep 2; tail -f /tmp/te-switch.log; }" Enter \; \
  select-pane -t 2
