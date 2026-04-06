#!/usr/bin/env bash
# te-switch.sh — stop the running engine and restart in the specified mode
# Usage: te-switch.sh [orb|orb-forex|regular] [--session london|newyork]

set -euo pipefail

MODE=${1:-regular}
SESSION=""
if [ "${2:-}" = "--session" ] && [ -n "${3:-}" ]; then
  SESSION="$3"
fi

ENGINE_DIR="$(cd "$(dirname "$0")/.." && pwd)"

cd "$ENGINE_DIR"

echo "[te-switch] Stopping any running engine instance..."
./te stop 2>/dev/null || true
sleep 5

case "$MODE" in
  orb)
    echo "[te-switch] Starting in ORB mode (equities)..."
    ./te run --mode orb --web --web-port 8080 --detach
    ;;
  orb-forex)
    if [ -n "$SESSION" ]; then
      echo "[te-switch] Starting in Forex ORB mode (session: $SESSION)..."
      ./te run --mode orb-forex --session "$SESSION" --web --web-port 8080 --detach
    else
      echo "[te-switch] Starting in Forex ORB mode..."
      ./te run --mode orb-forex --web --web-port 8080 --detach
    fi
    ;;
  *)
    echo "[te-switch] Starting in regular mode..."
    ./te run --web --web-port 8080 --detach
    ;;
esac
