#!/usr/bin/env bash
# te-switch.sh — stop the running engine and restart in the specified mode
#
# Usage: te-switch.sh [orb|orb-forex|orb-newyork|regular] [--session london|newyork]
#
# For orb-forex, if --session is omitted the script picks the session
# automatically based on current UTC hour:
#   Before 12:00 UTC → london  (London open 08:00 UTC)
#   12:00 UTC onward → newyork (NY open 13:00 UTC)
#
# orb-newyork runs equities ORB (Alpaca) and forex NY ORB (IBKR) concurrently
# in a single process. Starts 30 min before NY open (13:30 UTC).
#
# IBKR re-authentication runs before every mode switch so the gateway is
# always fresh regardless of when the switch is triggered.

set -euo pipefail

MODE=${1:-regular}
SESSION=""
if [ "${2:-}" = "--session" ] && [ -n "${3:-}" ]; then
  SESSION="$3"
fi

ENGINE_DIR="$(cd "$(dirname "$0")/.." && pwd)"

cd "$ENGINE_DIR"

# Load credentials from .env if present (needed for reauth and the engine).
if [ -f "$ENGINE_DIR/.env" ]; then
  set -a
  # shellcheck source=/dev/null
  source "$ENGINE_DIR/.env"
  set +a
fi

# Re-authenticate the IBKR gateway before every restart. The script fast-paths
# immediately if the gateway is already authenticated, so this is cheap when
# auth is healthy.
echo "[te-switch] Running IBKR gateway re-authentication..."
node "$ENGINE_DIR/scripts/reauth-gateway.js" || echo "[te-switch] WARNING: reauth failed — engine will start anyway"

echo "[te-switch] Stopping any running engine instance..."
./te stop 2>/dev/null || true
sleep 5

case "$MODE" in
  orb)
    echo "[te-switch] Starting in ORB mode (equities)..."
    ./te run --mode orb --detach
    ;;
  orb-newyork)
    echo "[te-switch] Starting in ORB-NewYork mode (equities + forex)..."
    ./te run --mode orb-newyork --detach
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
