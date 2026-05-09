// Command audit-feedback is a one-shot diagnostic tool for the feedback loop.
//
// It calls feedback.GetPerformanceContext for a given ticker, prints the raw
// performance context block, then renders the full analyst stock prompt
// (system + user) using stubbed candles/indicators so that any operator can
// confirm in <60 seconds whether the feedback section actually reaches the
// rendered prompt.
//
// Usage:
//
//	DATABASE_URL=... go run ./cmd/audit-feedback -ticker NVDA
//	DATABASE_URL=... go run ./cmd/audit-feedback -ticker IBKR -timeframe 1d
//
// Issue #314.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/joho/godotenv"

	"github.com/romeo-folie/trade-engine/internal/analysis/prompts"
	"github.com/romeo-folie/trade-engine/internal/feedback"
	"github.com/romeo-folie/trade-engine/internal/store"
)

func main() {
	_ = godotenv.Load()

	ticker := flag.String("ticker", "NVDA", "ticker symbol to audit (must have closed trades for non-empty output)")
	timeframe := flag.String("timeframe", "1d", "timeframe label to render in the prompt")
	strategy := flag.String("strategy", "swing", "strategy label for the system prompt")
	flag.Parse()

	ctx := context.Background()
	if err := store.Connect(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "store.Connect: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	pc, err := feedback.GetPerformanceContext(ctx, *ticker)
	if err != nil {
		fmt.Fprintf(os.Stderr, "feedback.GetPerformanceContext(%s): %v\n", *ticker, err)
		os.Exit(1)
	}

	fmt.Println("=== AUDIT: feedback.GetPerformanceContext ===")
	fmt.Printf("ticker:    %s\n", *ticker)
	fmt.Printf("non-empty: %t\n", pc != "")
	fmt.Printf("length:    %d bytes\n", len(pc))
	fmt.Println("--- raw performance context ---")
	if pc == "" {
		fmt.Println("(empty)")
	} else {
		fmt.Print(pc)
		if pc[len(pc)-1] != '\n' {
			fmt.Println()
		}
	}
	fmt.Println("--- end raw performance context ---")
	fmt.Println()

	// Build a synthetic but plausible stock prompt so the renderer runs end-to-end.
	now := time.Now().UTC().Truncate(time.Hour)
	candles := []prompts.Candle{
		{Timestamp: now.Add(-1 * time.Hour), Open: 100.10, High: 101.40, Low: 99.80, Close: 101.05, Volume: 1_200_000, VWAP: 100.55},
		{Timestamp: now.Add(-2 * time.Hour), Open: 99.50, High: 100.30, Low: 99.10, Close: 100.10, Volume: 1_080_000, VWAP: 99.85},
		{Timestamp: now.Add(-3 * time.Hour), Open: 99.00, High: 99.70, Low: 98.60, Close: 99.50, Volume: 980_000, VWAP: 99.20},
	}
	ind := prompts.Indicators{
		EMA20: 100.10, EMA50: 99.40, EMA200: 95.20,
		SMA20: 100.05, SMA50: 99.10, SMA200: 94.80,
		RSI:           58.4,
		MACDLine:      0.42, MACDSignal: 0.31, MACDHistogram: 0.11,
		BBUpper: 102.30, BBMiddle: 100.10, BBLower: 97.90,
		ADX: 22.1, ATR: 1.35, OBV: 145_000_000,
		StochK: 62.0, StochD: 58.0,
		VWAP: 100.55, VolSMA20: 1_050_000, VolRatio: 1.14,
	}
	news := []prompts.NewsItem{
		{Headline: "(audit stub) no live news fetched", Source: "audit-feedback", PublishedAt: now.Add(-3 * time.Hour), Sentiment: "neutral"},
	}
	portfolio := prompts.Portfolio{CashAvailable: 0}

	system, user := prompts.BuildStockPrompt(
		*ticker, *strategy, *timeframe,
		candles, ind, news, portfolio,
		"", // knowledgeContext
		nil, // similarSetups
		pc,
		false, // momentumPlay
	)

	fmt.Println("=== RENDERED SYSTEM PROMPT ===")
	fmt.Println(system)
	fmt.Println()
	fmt.Println("=== RENDERED USER MESSAGE ===")
	fmt.Println(user)
}
