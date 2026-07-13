#!/usr/bin/env bash
# One-time cleanup (2026-07-09): the London XAG buy breakout filled during an
# IBKR auth flap, so the engine never ran the OCO cancel of the opposite (sell)
# side. This cancels ONLY the orphaned sell-bracket legs — the never-filled sell
# entry and its two children — leaving the long's real protective legs intact.
#
# CANCEL (sell bracket): 1349443975 SELL stop 59.14 (orphan entry)
#                        1349443976 BUY  limit 58.48 (orphan TP)
#                        1349443977 BUY  stop  59.47 (orphan SL)
# KEEP   (long's guard): 1349443973 SELL limit 60.13 (take-profit)
#                        1349443974 SELL stop  59.14 (stop-loss)
set -euo pipefail
GW="https://localhost:5001"; ACCT="DUP723970"

for id in 1349443975 1349443976 1349443977; do
  echo "── Cancelling orphan order $id ──"
  resp=$(curl -sk -X DELETE "$GW/v1/api/iserver/account/$ACCT/order/$id")
  echo "  $resp"
done

echo; echo "── Remaining live XAG orders (expect exactly: SELL limit 60.13 + SELL stop 59.14) ──"
curl -sk "$GW/v1/api/iserver/account/orders" | python3 -c "
import sys,json
for x in json.load(sys.stdin).get('orders',[]):
    if 'XAG' in str(x.get('ticker','')) and x.get('status') in ('Submitted','PreSubmitted'):
        print(' ', x.get('orderId'), x.get('side'), x.get('orderType'), x.get('remainingQuantity'), '@', x.get('price') or x.get('auxPrice'))
"
echo "── Position (should still be LONG 1550) ──"
curl -sk "$GW/v1/api/portfolio/accounts" -o /dev/null
curl -sk "$GW/v1/api/portfolio/$ACCT/positions/0" | python3 -c "
import sys,json
print(' ', [(p.get('contractDesc'), p.get('position')) for p in json.load(sys.stdin) if p.get('position')])
"
