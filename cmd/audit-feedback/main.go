// Command audit-feedback is a one-shot diagnostic tool for the feedback loop.
//
// It calls feedback.GetPerformanceContext for a given ticker, prints the raw
// performance context block, then renders the full analyst stock prompt
// (system + user) using stubbed candles/indicators so that any operator can
// confirm in <60 seconds whether the feedback section actually reaches the
// rendered prompt.
//
// Beyond the basic non-empty check, the audit also queries feedback_insights
// directly for an instrument-scoped row (scope='instrument', scope_key=ticker)
// and reports `instrument_specific: true|false`. When --require-instrument is
// set (default true), the audit exits non-zero if no instrument-scope row
// exists for the supplied ticker — this is the bug we're trying to detect.
//
// Usage:
//
//	DATABASE_URL=... go run ./cmd/audit-feedback -ticker NVDA
//	DATABASE_URL=... go run ./cmd/audit-feedback -ticker IBKR -timeframe 1d
//	DATABASE_URL=... go run ./cmd/audit-feedback -ticker NVDA -require-instrument=false
//
// Issue #314.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/joho/godotenv"

	"github.com/romeo-folie/trade-engine/internal/analysis/prompts"
	"github.com/romeo-folie/trade-engine/internal/feedback"
	"github.com/romeo-folie/trade-engine/internal/store"
)

const (
	connectTimeout = 5 * time.Second
	queryTimeout   = 5 * time.Second
	overallTimeout = 10 * time.Second
)

func main() {
	_ = godotenv.Load()

	ticker := flag.String("ticker", "NVDA", "ticker symbol to audit (must have closed trades for non-empty output)")
	timeframe := flag.String("timeframe", "1d", "timeframe label to render in the prompt")
	strategy := flag.String("strategy", "swing", "comma-separated candidate lenses for the system prompt")
	requireInstrument := flag.Bool("require-instrument", true, "when true, exit non-zero if no instrument-scope feedback_insights row exists for the supplied ticker")
	flag.Parse()

	// Normalise ticker: writers persist trades.ticker in upper-case, so a
	// lower-case query argument would silently miss its instrument-scope row.
	*ticker = strings.ToUpper(strings.TrimSpace(*ticker))
	if *ticker == "" {
		fmt.Fprintln(os.Stderr, "ticker is required")
		os.Exit(2)
	}

	// Overall budget for the whole audit (connect + ping + queries).
	rootCtx, rootCancel := context.WithTimeout(context.Background(), overallTimeout)
	defer rootCancel()

	connectCtx, connectCancel := context.WithTimeout(rootCtx, connectTimeout)
	defer connectCancel()
	if err := store.Connect(connectCtx); err != nil {
		fmt.Fprintf(os.Stderr, "store.Connect: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	pcCtx, pcCancel := context.WithTimeout(rootCtx, queryTimeout)
	defer pcCancel()
	pc, err := feedback.GetPerformanceContext(pcCtx, *ticker)
	if err != nil {
		fmt.Fprintf(os.Stderr, "feedback.GetPerformanceContext(%s): %v\n", *ticker, err)
		os.Exit(1)
	}

	// Stronger ticker-specific check: query feedback_insights directly for
	// scope='instrument', scope_key=ticker. The basic non-empty signal can pass
	// when only model-scope rows exist, masking the absence of per-ticker data.
	instrCtx, instrCancel := context.WithTimeout(rootCtx, queryTimeout)
	defer instrCancel()
	instrRows, err := store.GetFeedbackInsights(instrCtx, "instrument", *ticker, "30d", 1)
	if err != nil {
		fmt.Fprintf(os.Stderr, "store.GetFeedbackInsights(instrument, %s): %v\n", *ticker, err)
		os.Exit(1)
	}
	instrumentSpecific := len(instrRows) > 0

	fmt.Println("=== AUDIT: feedback.GetPerformanceContext ===")
	fmt.Printf("ticker (normalized):  %s\n", *ticker)
	fmt.Printf("non-empty:            %t\n", pc != "")
	fmt.Printf("instrument_specific:  %t\n", instrumentSpecific)
	fmt.Printf("require-instrument:   %t\n", *requireInstrument)
	fmt.Printf("length:               %d bytes\n", len(pc))
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
		RSI:      58.4,
		MACDLine: 0.42, MACDSignal: 0.31, MACDHistogram: 0.11,
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
		*ticker, splitStrategies(*strategy), *timeframe,
		candles, ind, news, portfolio,
		"",  // knowledgeContext
		nil, // similarSetups
		pc,
		false, // momentumPlay
	)

	fmt.Println("=== RENDERED SYSTEM PROMPT ===")
	fmt.Println(system)
	fmt.Println()
	fmt.Println("=== RENDERED USER MESSAGE ===")
	fmt.Println(user)
	fmt.Println()

	// Final pass/fail summary. The audit FAILs when --require-instrument is set
	// (default) and no instrument-scope row exists for the supplied ticker.
	pass := true
	var reason string
	if *requireInstrument && !instrumentSpecific {
		pass = false
		reason = fmt.Sprintf("no instrument-scope feedback_insights row found for ticker=%s (scope='instrument', scope_key=%q)", *ticker, *ticker)
	}

	if pass {
		fmt.Printf("=== AUDIT RESULT: OK (ticker=%s, instrument_specific=%t) ===\n", *ticker, instrumentSpecific)
		return
	}

	fmt.Printf("=== AUDIT RESULT: FAIL — %s ===\n", reason)
	// Exit with status 3 to distinguish a strictness failure from a generic
	// runtime error (1) or flag/usage error (2).
	os.Exit(3)
}

// splitStrategies parses the --strategy flag into the candidate lens set the
// prompt builder now takes. Unrecognised labels are dropped by prompts.Lenses,
// so an operator passing the historical "swing" default still gets a rendered
// prompt (falling back to the default lens) rather than an error.
func splitStrategies(csv string) []string {
	parts := strings.Split(csv, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
