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
#
# Cron runs with a minimal PATH that does not include nvm-managed Node binaries.
# Resolve node via nvm if available, then fall back to common install locations.
NODE_BIN=""
if [ -s "${NVM_DIR:-$HOME/.nvm}/nvm.sh" ]; then
  # shellcheck source=/dev/null
  source "${NVM_DIR:-$HOME/.nvm}/nvm.sh" --no-use
  NODE_BIN="$(command -v node 2>/dev/null || true)"
fi
if [ -z "$NODE_BIN" ]; then
  for candidate in \
    "$HOME/.nvm/versions/node/v22.13.0/bin/node" \
    /usr/local/bin/node \
    /opt/homebrew/bin/node; do
    if [ -x "$candidate" ]; then
      NODE_BIN="$candidate"
      break
    fi
  done
fi

GATEWAY_URL="${IBKR_GATEWAY_URL:-https://localhost:5001}"
CURL_BIN="$(command -v curl 2>/dev/null || echo /usr/bin/curl)"

# needs_ibkr: modes that require a live, authenticated IBKR gateway session.
# Equities-only modes (orb, regular) trade via Alpaca and must NOT be gated on
# IBKR auth — gating them would block a healthy equities launch on an unrelated
# gateway outage.
needs_ibkr() {
  case "$1" in
    orb-forex|orb-newyork) return 0 ;;
    *) return 1 ;;
  esac
}

# gateway_authenticated: ask the gateway's OWN status endpoint — the authoritative
# signal, not the reauth script's self-report — and succeed only when the session
# is both authenticated and connected. Self-signed cert → -k.
gateway_authenticated() {
  local resp
  resp="$("$CURL_BIN" -sk --max-time 8 "$GATEWAY_URL/v1/api/iserver/auth/status" 2>/dev/null)" || return 1
  printf '%s' "$resp" | grep -q '"authenticated":true' || return 1
  printf '%s' "$resp" | grep -q '"connected":true'     || return 1
  return 0
}

# run_reauth: drive the headless SSO login. Returns the script's exit code so the
# caller can decide whether a failure is fatal (IBKR modes) or ignorable (others).
run_reauth() {
  if [ -n "$NODE_BIN" ]; then
    "$NODE_BIN" "$ENGINE_DIR/scripts/reauth-gateway.js"
  else
    echo "[te-switch] WARNING: node not found — cannot run reauth"
    return 1
  fi
}

# tg_notify: best-effort Telegram alert from the launcher's own abort path
# (reauth-gateway.js has its own JS sender for its failures).
tg_notify() {
  [ -n "${TELEGRAM_BOT_TOKEN:-}" ] && [ -n "${TELEGRAM_CHAT_ID:-}" ] || return 0
  "$CURL_BIN" -s --max-time 10 \
    "https://api.telegram.org/bot${TELEGRAM_BOT_TOKEN}/sendMessage" \
    --data-urlencode "chat_id=${TELEGRAM_CHAT_ID}" \
    --data-urlencode "text=$1" >/dev/null 2>&1 || true
}

if needs_ibkr "$MODE"; then
  # Launch-time auth gate. Start an IBKR session ONLY once the gateway itself
  # reports authenticated+connected. This replaces the old "reauth failed →
  # start anyway" path, which launched a detached engine that then failed
  # CheckAuth and exited silently — a dead process masquerading as a session.
  echo "[te-switch] $MODE needs IBKR — authenticating gateway before launch..."
  REAUTH_RC=0
  run_reauth || REAUTH_RC=$?

  # reauth returns 0 only after its own /auth/status poll goes green, but the
  # session can flap in the seconds between that and launch — so independently
  # re-confirm here (~20s) right before starting the engine.
  AUTH_OK=0
  for _ in $(seq 1 10); do
    if gateway_authenticated; then AUTH_OK=1; break; fi
    sleep 2
  done

  if [ "$AUTH_OK" != 1 ]; then
    echo "[te-switch] ERROR: IBKR gateway not authenticated (reauth rc=$REAUTH_RC) — NOT starting $MODE."
    tg_notify "🔐 te-switch: IBKR gateway not authenticated — $MODE session NOT started. Manual login required at $GATEWAY_URL"
    exit 1
  fi
  echo "[te-switch] IBKR gateway authenticated ✓ — launching $MODE."
else
  # Equities-only modes: reauth is best-effort and never blocks the launch.
  echo "[te-switch] $MODE does not require IBKR — best-effort reauth only."
  run_reauth || echo "[te-switch] WARNING: reauth failed — continuing ($MODE does not need IBKR)"
fi

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
