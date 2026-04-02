#!/bin/bash
# scripts/verify_api.sh - Smoke tests for TradeEngine API
#
# Usage: ./scripts/verify_api.sh [BASE_URL]
# Default BASE_URL is http://localhost:8080

BASE_URL=${1:-"http://localhost:8080"}

echo "🔍 Verifying TradeEngine API at $BASE_URL..."

# Helper to check JSON key existence
check_key() {
    local url=$1
    local key=$2
    local body
    body=$(curl -s "$BASE_URL$url")
    if echo "$body" | grep -q "\"$key\""; then
        echo "✅ $url contains '$key'"
    else
        echo "❌ $url missing '$key'"
        echo "   Body: $body"
        exit 1
    fi
}

# 1. Portfolio History
check_key "/api/portfolio/history?days=7" "history"

# 2. Portfolio Performance
check_key "/api/portfolio/performance" "today"
check_key "/api/portfolio/performance" "week"
check_key "/api/portfolio/performance" "month"

# 3. Portfolio Stats
check_key "/api/portfolio/stats" "total_trades"
check_key "/api/portfolio/stats" "win_rate"

# 4. System Status
check_key "/api/system/status" "version"
check_key "/api/system/status" "drawdown"

# 5. Open Positions (Risk)
check_key "/api/node/risk" "open_trades"

echo "✨ All smoke tests passed!"
