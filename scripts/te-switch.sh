#!/usr/bin/env bash
# te-switch.sh — stop the running engine and restart in the specified mode
# Usage: te-switch.sh [orb|regular]

set -euo pipefail

MODE=${1:-regular}
ENGINE_DIR="$(cd "$(dirname "$0")/.." && pwd)"

cd "$ENGINE_DIR"

echo "[te-switch] Stopping any running engine instance..."
./te stop 2>/dev/null || true
sleep 5

if [ "$MODE" = "orb" ]; then
  echo "[te-switch] Starting in ORB mode..."
  ./te run --mode orb --web --web-port 8080 --detach
else
  echo "[te-switch] Starting in regular mode..."
  ./te run --web --web-port 8080 --detach
fi
