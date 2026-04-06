#!/usr/bin/env bash
# te-switch.sh — stop the running engine and restart in the specified mode
#
# Usage: te-switch.sh [orb|orb-forex|regular] [--session london|newyork]
#
# For orb-forex, if --session is omitted the script picks the session
# automatically based on current UTC hour:
#   Before 12:00 UTC → london  (London open 08:00 UTC)
#   12:00 UTC onward → newyork (NY open 13:00 UTC)

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
    ./te run --mode orb --detach
    ;;
  orb-forex)
    # Auto-detect session from UTC hour if not specified.
    if [ -z "$SESSION" ]; then
      HOUR_UTC=$(date -u +%H)
      if [ "$HOUR_UTC" -lt 12 ]; then
        SESSION="london"
      else
        SESSION="newyork"
      fi
    fi
    echo "[te-switch] Starting in Forex ORB mode (session: $SESSION)..."
    ./te run --mode orb-forex --session "$SESSION" --detach
    ;;
  *)
    echo "[te-switch] Starting in regular mode..."
    ./te run --web --web-port 8080 --detach
    ;;
esac
