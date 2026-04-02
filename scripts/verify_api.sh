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
    local http_code
    
    # -s: silent, -w: write-out %{http_code} at the end
    body=$(curl -s -w "%{http_code}" "$BASE_URL$url")
    http_code="${body: -3}"
    body="${body:0:${#body}-3}"
    
    if [ "$http_code" != "200" ]; then
        echo "❌ $url returned HTTP $http_code (expected 200)"
        echo "   Body: $body"
        exit 1
    fi

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
