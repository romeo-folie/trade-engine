package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
)

func TestEvaluateRunupGates_NewsTapeConfirmation(t *testing.T) {
	cfg := config.EntryGatesConfig{NewsMinRunupPct: 2.0}

	// Price flat vs prior close — the 16%-win cohort — blocked.
	if reason := evaluateRunupGates("news_sentiment", "BUY", 100.5, 100, cfg); reason == "" {
		t.Fatal("flat tape should block a news BUY")
	} else if !strings.Contains(reason, "tape confirmation") {
		t.Fatalf("unexpected reason: %s", reason)
	}
	// Price down vs prior close — blocked.
	if reason := evaluateRunupGates("news_sentiment", "BUY", 97, 100, cfg); reason == "" {
		t.Fatal("negative reaction should block a news BUY")
	}
	// Tape confirmed (+3%) — passes.
	if reason := evaluateRunupGates("news_sentiment", "BUY", 103, 100, cfg); reason != "" {
		t.Fatalf("confirmed tape should pass, got: %s", reason)
	}
	// Exactly at the threshold — passes (gate blocks strictly below).
	if reason := evaluateRunupGates("news_sentiment", "BUY", 102, 100, cfg); reason != "" {
		t.Fatalf("at-threshold should pass, got: %s", reason)
	}
}

func TestEvaluateRunupGates_MomentumAntiChase(t *testing.T) {
	cfg := config.EntryGatesConfig{MomentumMaxRunupPct: 2.0}

	// Chasing a +5% run-up — blocked.
	if reason := evaluateRunupGates("momentum", "BUY", 105, 100, cfg); reason == "" {
		t.Fatal("chasing momentum BUY should be blocked")
	} else if !strings.Contains(reason, "anti-chase") {
		t.Fatalf("unexpected reason: %s", reason)
	}
	// Not chasing (+1%) — passes.
	if reason := evaluateRunupGates("momentum", "BUY", 101, 100, cfg); reason != "" {
		t.Fatalf("un-chased momentum BUY should pass, got: %s", reason)
	}
	// Exactly at the cap — passes (gate blocks strictly above).
	if reason := evaluateRunupGates("momentum", "BUY", 102, 100, cfg); reason != "" {
		t.Fatalf("at-cap should pass, got: %s", reason)
	}
}

func TestEvaluateRunupGates_ScopeAndFailOpen(t *testing.T) {
	cfg := config.EntryGatesConfig{NewsMinRunupPct: 2.0, MomentumMaxRunupPct: 2.0}

	// SELL signals are never gated.
	if reason := evaluateRunupGates("news_sentiment", "SELL", 100, 100, cfg); reason != "" {
		t.Fatalf("SELL must not be gated, got: %s", reason)
	}
	// Other strategies are never gated.
	if reason := evaluateRunupGates("trend_following", "BUY", 100, 100, cfg); reason != "" {
		t.Fatalf("ungated strategy blocked: %s", reason)
	}
	// Unknown prior close fails open.
	if reason := evaluateRunupGates("news_sentiment", "BUY", 100, 0, cfg); reason != "" {
		t.Fatalf("missing prior close must fail open, got: %s", reason)
	}
	// Zero-value config disables everything.
	if reason := evaluateRunupGates("news_sentiment", "BUY", 100, 100, config.EntryGatesConfig{}); reason != "" {
		t.Fatalf("disabled gates blocked: %s", reason)
	}
	// Momentum gate does not apply the news minimum and vice versa.
	if reason := evaluateRunupGates("momentum", "BUY", 100.5, 100, cfg); reason != "" {
		t.Fatalf("momentum flat tape should pass (min-runup is news-only), got: %s", reason)
	}
}

func TestDeferredPriceHeld(t *testing.T) {
	// Held: within the 1% default tolerance.
	if !deferredPriceHeld(100, 99.5, 0) {
		t.Fatal("price within default tolerance should hold")
	}
	// Dropped past tolerance.
	if deferredPriceHeld(100, 98.5, 1.0) {
		t.Fatal("price below tolerance should not hold")
	}
	// Price above signal always holds.
	if !deferredPriceHeld(100, 102, 1.0) {
		t.Fatal("price above signal should hold")
	}
	// Wider explicit tolerance.
	if !deferredPriceHeld(100, 98.5, 2.0) {
		t.Fatal("price within explicit 2% tolerance should hold")
	}
	// Garbage inputs never hold.
	if deferredPriceHeld(0, 100, 1.0) || deferredPriceHeld(100, 0, 1.0) {
		t.Fatal("non-positive prices must not confirm")
	}
}

func TestSameMarketDay(t *testing.T) {
	et, _ := time.LoadLocation("America/New_York")
	// 23:30 ET vs 00:30 ET next day — different market days even though
	// they may share a UTC day.
	a := time.Date(2026, 6, 1, 23, 30, 0, 0, et)
	b := time.Date(2026, 6, 2, 0, 30, 0, 0, et)
	if sameMarketDay(a, b, et) {
		t.Fatal("ET midnight boundary should split market days")
	}
	// Same ET day expressed in UTC (evening UTC = same ET afternoon).
	c := time.Date(2026, 6, 1, 14, 0, 0, 0, time.UTC) // 10:00 ET
	d := time.Date(2026, 6, 1, 20, 0, 0, 0, time.UTC) // 16:00 ET
	if !sameMarketDay(c, d, et) {
		t.Fatal("same ET session should be the same market day")
	}
	// 01:00 UTC next day is still the prior ET evening.
	f := time.Date(2026, 6, 2, 1, 0, 0, 0, time.UTC) // 21:00 ET Jun 1
	if !sameMarketDay(c, f, et) {
		t.Fatal("UTC rollover before ET midnight is the same market day")
	}
}
