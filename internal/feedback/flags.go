package feedback

import (
	"context"
	"fmt"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// checkFlags inspects the 30d window insights and raises feedback flags for any
// threshold violations. Duplicate unresolved flags for the same (date, scope,
// scopeKey, metric) combination are suppressed.
func checkFlags(
	ctx context.Context,
	bus *events.Bus,
	cfg config.FeedbackThresholds,
	date time.Time,
	insights []store.FeedbackInsight,
) error {
	// Build a lookup: (scope, scopeKey) → metric → insight for window "30d".
	type key struct{ scope, scopeKey string }
	byKey := make(map[key]map[string]store.FeedbackInsight)
	for _, ins := range insights {
		if ins.Window != "30d" {
			continue
		}
		k := key{ins.Scope, ins.ScopeKey}
		if byKey[k] == nil {
			byKey[k] = make(map[string]store.FeedbackInsight)
		}
		byKey[k][ins.Metric] = ins
	}

	// Helper: look up a specific metric for a scope+key.
	getMetric := func(scope, scopeKey, metric string) (store.FeedbackInsight, bool) {
		if m, ok := byKey[key{scope, scopeKey}]; ok {
			if ins, ok := m[metric]; ok {
				return ins, true
			}
		}
		return store.FeedbackInsight{}, false
	}

	// ── Model win-rate check ─────────────────────────────────────────────────
	for k, metrics := range byKey {
		if k.scope != "model" {
			continue
		}
		winIns, hasWin := metrics["win_rate"]
		if !hasWin {
			continue
		}
		tcIns, hasTc := metrics["trade_count"]
		tradeCount := 0
		if hasTc {
			tradeCount = int(tcIns.Value)
		}

		if winIns.Value < cfg.ModelWinRateMin && tradeCount >= cfg.ModelMinTrades {
			dup, err := store.HasUnresolvedFlag(ctx, date, "model", k.scopeKey, "win_rate")
			if err != nil {
				return fmt.Errorf("feedback: HasUnresolvedFlag model win_rate %s: %w", k.scopeKey, err)
			}
			if dup {
				continue
			}
			msg := fmt.Sprintf(
				"%s win rate dropped to %.0f%% over last %d trades (threshold: %.0f%%)",
				k.scopeKey, winIns.Value*100, tradeCount, cfg.ModelWinRateMin*100,
			)
			if err := insertAndPublish(ctx, bus, store.FeedbackFlag{
				Date:        date,
				Scope:       "model",
				ScopeKey:    k.scopeKey,
				Severity:    "warning",
				Message:     msg,
				Metric:      "win_rate",
				MetricValue: winIns.Value,
				Threshold:   cfg.ModelWinRateMin,
			}); err != nil {
				return err
			}
		}
	}

	// ── Instrument win-rate check ────────────────────────────────────────────
	for k, metrics := range byKey {
		if k.scope != "instrument" {
			continue
		}
		winIns, hasWin := metrics["win_rate"]
		if !hasWin {
			continue
		}
		tcIns, hasTc := metrics["trade_count"]
		tradeCount := 0
		if hasTc {
			tradeCount = int(tcIns.Value)
		}

		if winIns.Value < cfg.InstrumentWinRateMin && tradeCount >= cfg.InstrumentMinTrades {
			dup, err := store.HasUnresolvedFlag(ctx, date, "instrument", k.scopeKey, "win_rate")
			if err != nil {
				return fmt.Errorf("feedback: HasUnresolvedFlag instrument win_rate %s: %w", k.scopeKey, err)
			}
			if dup {
				continue
			}
			msg := fmt.Sprintf(
				"%s win rate dropped to %.0f%% over last %d trades (threshold: %.0f%%)",
				k.scopeKey, winIns.Value*100, tradeCount, cfg.InstrumentWinRateMin*100,
			)
			if err := insertAndPublish(ctx, bus, store.FeedbackFlag{
				Date:        date,
				Scope:       "instrument",
				ScopeKey:    k.scopeKey,
				Severity:    "warning",
				Message:     msg,
				Metric:      "win_rate",
				MetricValue: winIns.Value,
				Threshold:   cfg.InstrumentWinRateMin,
			}); err != nil {
				return err
			}
		}
	}

	// ── ORB session win-rate check ───────────────────────────────────────────
	for k, metrics := range byKey {
		if k.scope != "orb_session" {
			continue
		}
		winIns, hasWin := metrics["win_rate"]
		if !hasWin {
			continue
		}
		tcIns, hasTc := metrics["trade_count"]
		tradeCount := 0
		if hasTc {
			tradeCount = int(tcIns.Value)
		}

		if winIns.Value < cfg.InstrumentWinRateMin && tradeCount >= cfg.InstrumentMinTrades {
			dup, err := store.HasUnresolvedFlag(ctx, date, "orb_session", k.scopeKey, "win_rate")
			if err != nil {
				return fmt.Errorf("feedback: HasUnresolvedFlag orb_session win_rate %s: %w", k.scopeKey, err)
			}
			if dup {
				continue
			}
			msg := fmt.Sprintf(
				"%s win rate dropped to %.0f%% over last %d trades (threshold: %.0f%%)",
				k.scopeKey, winIns.Value*100, tradeCount, cfg.InstrumentWinRateMin*100,
			)
			if err := insertAndPublish(ctx, bus, store.FeedbackFlag{
				Date:        date,
				Scope:       "orb_session",
				ScopeKey:    k.scopeKey,
				Severity:    "warning",
				Message:     msg,
				Metric:      "win_rate",
				MetricValue: winIns.Value,
				Threshold:   cfg.InstrumentWinRateMin,
			}); err != nil {
				return err
			}
		}
	}

	// ── Arbiter invoked rate ────────────────────────────────────────────────
	if arbIns, ok := getMetric("system", "arbiter", "arbiter_invoked_rate"); ok {
		if arbIns.Value > cfg.ArbiterInvokedRateMax {
			dup, err := store.HasUnresolvedFlag(ctx, date, "system", "arbiter", "arbiter_invoked_rate")
			if err != nil {
				return fmt.Errorf("feedback: HasUnresolvedFlag arbiter_invoked_rate: %w", err)
			}
			if !dup {
				msg := fmt.Sprintf(
					"Arbiter invoked rate at %.0f%% (threshold: %.0f%%)",
					arbIns.Value*100, cfg.ArbiterInvokedRateMax*100,
				)
				if err := insertAndPublish(ctx, bus, store.FeedbackFlag{
					Date:        date,
					Scope:       "system",
					ScopeKey:    "arbiter",
					Severity:    "info",
					Message:     msg,
					Metric:      "arbiter_invoked_rate",
					MetricValue: arbIns.Value,
					Threshold:   cfg.ArbiterInvokedRateMax,
				}); err != nil {
					return err
				}
			}
		}
	}

	return nil
}

// insertAndPublish inserts a FeedbackFlag and publishes a FeedbackFlagCreated event.
func insertAndPublish(ctx context.Context, bus *events.Bus, flag store.FeedbackFlag) error {
	id, err := store.InsertFeedbackFlag(ctx, flag)
	if err != nil {
		return fmt.Errorf("feedback: InsertFeedbackFlag scope=%s key=%s: %w", flag.Scope, flag.ScopeKey, err)
	}
	if bus != nil {
		bus.Publish(events.FeedbackFlagCreated, events.FeedbackFlagCreatedPayload{
			FlagID:   id,
			Scope:    flag.Scope,
			ScopeKey: flag.ScopeKey,
			Severity: flag.Severity,
			Message:  flag.Message,
		})
	}
	return nil
}
