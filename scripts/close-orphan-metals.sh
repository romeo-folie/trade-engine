#!/usr/bin/env bash
# One-time: buy-to-cover the two orphaned metals shorts at IBKR (paper DUP723970).
# Places two MARKET orders and auto-answers IBKR's confirmation prompts, then
# prints remaining positions. Conids fetched live 2026-07-09: XAUUSD=69067924,
# XAGUSD=77124483. Run manually — this executes trades.
set -euo pipefail
GW="https://localhost:5001"
ACCT="DUP723970"

place() { # conid qty label
  local conid=$1 qty=$2 label=$3
  echo "── Closing $label: BUY $qty (conid $conid) at MARKET ──"
  local resp
  resp=$(curl -sk -X POST "$GW/v1/api/iserver/account/$ACCT/orders" \
    -H "Content-Type: application/json" \
    -d "{\"orders\":[{\"conid\":$conid,\"side\":\"BUY\",\"orderType\":\"MKT\",\"quantity\":$qty,\"tif\":\"DAY\"}]}")
  # Walk IBKR's confirmation-prompt chain (size limit warnings etc.).
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    local id
    id=$(echo "$resp" | python3 -c "
import sys,json
d=json.load(sys.stdin)
if isinstance(d,list) and d and 'id' in d[0] and 'order_id' not in d[0]: print(d[0]['id'])" 2>/dev/null)
    [ -z "$id" ] && break
    echo "  confirming prompt $id"
    resp=$(curl -sk -X POST "$GW/v1/api/iserver/reply/$id" -H "Content-Type: application/json" -d '{"confirmed":true}')
  done
  echo "  final: $resp"
}

place 69067924 57  "XAUUSD short"
place 77124483 700 "XAGUSD short"

echo; echo "── Remaining positions ──"
curl -sk "$GW/v1/api/portfolio/accounts" -o /dev/null
curl -sk "$GW/v1/api/portfolio/$ACCT/positions/0" | python3 -m json.tool | grep -E 'contractDesc|"position"' || echo "(flat)"
