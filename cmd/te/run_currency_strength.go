package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/store"
	"github.com/romeo-folie/trade-engine/internal/strategy/currencystrength"
)

const currencyStrengthEntryGrace = 15 * time.Minute

// sessionNotifier delivers currency-strength session notifications (e.g. Telegram).
// Structurally satisfied by *orbTelegramNotifier, so the currency-strength runner
// reuses the same Telegram adapter the ORB scheduler uses without importing orb.
type sessionNotifier interface {
	Notify(ctx context.Context, msg string) error
}

type currencyStrengthRunState struct {
	Date           string  `json:"date"`
	Symbol         string  `json:"symbol"`
	Side           string  `json:"side"`
	Quantity       float64 `json:"quantity"`
	EntryOrderID   string  `json:"entry_order_id"`
	EntryStatus    string  `json:"entry_status"`
	EntryFillPrice float64 `json:"entry_fill_price,omitempty"`
	EntryFilledQty float64 `json:"entry_filled_qty,omitempty"`
	EnteredAt      string  `json:"entered_at"`
	Strongest      string  `json:"strongest,omitempty"`
	Weakest        string  `json:"weakest,omitempty"`
	StrengthGap    float64 `json:"strength_gap,omitempty"`
	ExitOrderID    string  `json:"exit_order_id,omitempty"`
	ExitStatus     string  `json:"exit_status,omitempty"`
	ExitFillPrice  float64 `json:"exit_fill_price,omitempty"`
	ExitFilledQty  float64 `json:"exit_filled_qty,omitempty"`
	ExitedAt       string  `json:"exited_at,omitempty"`
}

// currencyStrengthJournal is the strategy's durable audit seam. Production
// writes to PostgreSQL; tests may omit it or supply a lightweight recorder.
type currencyStrengthJournal interface {
	Start(context.Context, store.CurrencyStrengthRun) error
	MarkEvaluating(context.Context, time.Time) error
	RecordNoSignal(context.Context, time.Time, string) error
	RecordEntry(context.Context, store.CurrencyStrengthRun) error
	RecordExit(context.Context, store.CurrencyStrengthRun) error
	RecordFailure(context.Context, time.Time, string) error
}

type dbCurrencyStrengthJournal struct{}

func (dbCurrencyStrengthJournal) Start(ctx context.Context, run store.CurrencyStrengthRun) error {
	return store.StartCurrencyStrengthRun(ctx, store.GetDB(), run)
}

func (dbCurrencyStrengthJournal) MarkEvaluating(ctx context.Context, date time.Time) error {
	return store.MarkCurrencyStrengthEvaluating(ctx, store.GetDB(), date)
}

func (dbCurrencyStrengthJournal) RecordNoSignal(ctx context.Context, date time.Time, reason string) error {
	return store.RecordCurrencyStrengthNoSignal(ctx, store.GetDB(), date, reason)
}

func (dbCurrencyStrengthJournal) RecordEntry(ctx context.Context, run store.CurrencyStrengthRun) error {
	return store.RecordCurrencyStrengthEntry(ctx, store.GetDB(), run)
}

func (dbCurrencyStrengthJournal) RecordExit(ctx context.Context, run store.CurrencyStrengthRun) error {
	return store.RecordCurrencyStrengthExit(ctx, store.GetDB(), run)
}

func (dbCurrencyStrengthJournal) RecordFailure(ctx context.Context, date time.Time, message string) error {
	return store.RecordCurrencyStrengthFailure(ctx, store.GetDB(), date, message)
}

type currencyStrengthPaperRunner struct {
	cfg       config.CurrencyStrengthConfig
	market    data.MarketDataProvider
	broker    execution.Executor
	statePath string
	now       func() time.Time
	notifier  sessionNotifier // optional; nil disables Telegram notifications
	journal   currencyStrengthJournal

	state              *currencyStrengthRunState
	decisionDoneFor    string
	sessionRecordedFor string
	entryRecorded      bool
}

func validateCurrencyStrengthPaperConfig(cfg *config.Config, dryRun bool) error {
	if !cfg.CurrencyStrength.Enabled {
		return fmt.Errorf("currency_strength.enabled=true is required")
	}
	if cfg.CurrencyStrength.Mode != "paper" {
		return fmt.Errorf("currency_strength.mode=paper is required for IBKR paper orders")
	}
	if dryRun || cfg.Engine.DryRun {
		return fmt.Errorf("currency_strength paper mode cannot run with dry_run enabled because no IBKR paper orders would be placed")
	}
	if !cfg.Markets.IBKR.Enabled {
		return fmt.Errorf("currency_strength requires markets.ibkr.enabled=true")
	}
	if cfg.Markets.IBKR.AccountID == "" {
		return fmt.Errorf("currency_strength requires markets.ibkr.account_id to be set")
	}
	if !isIBKRPaperAccountID(cfg.Markets.IBKR.AccountID) {
		return fmt.Errorf("currency_strength paper mode requires an IBKR paper account id starting with DU; got %q", cfg.Markets.IBKR.AccountID)
	}
	return nil
}

func newCurrencyStrengthPaperRunner(cfg config.CurrencyStrengthConfig, market data.MarketDataProvider, broker execution.Executor) *currencyStrengthPaperRunner {
	return &currencyStrengthPaperRunner{
		cfg:       cfg,
		market:    market,
		broker:    broker,
		statePath: defaultCurrencyStrengthStatePath(),
		now:       time.Now,
	}
}

// SetNotifier wires a notification sink (e.g. Telegram) for session-start and
// entry messages. Call before Run. If not called, notifications are no-ops.
func (r *currencyStrengthPaperRunner) SetNotifier(n sessionNotifier) {
	r.notifier = n
}

func (r *currencyStrengthPaperRunner) SetJournal(j currencyStrengthJournal) {
	r.journal = j
}

// notify sends a message through the configured notifier. No-op when unset;
// a delivery error is logged and swallowed so notifications never abort the runner.
func (r *currencyStrengthPaperRunner) notify(ctx context.Context, msg string) {
	if r.notifier == nil {
		return
	}
	if err := r.notifier.Notify(ctx, msg); err != nil {
		logger.Warn("currency-strength: notify failed", "err", err)
	}
}

// buildCurrencyStrengthSessionStartMessage renders the session-start Telegram
// listing for the FX-major basket — the currency-strength analogue of the ORB
// "Scan Complete" message. Lists the pairs ranked for strength, the subset
// eligible for orders, and the decision/exit clock so the operator knows when a
// paper order can appear.
func buildCurrencyStrengthSessionStartMessage(cfg config.CurrencyStrengthConfig) string {
	tz := cfg.Timezone
	if tz == "" {
		tz = "UTC"
	}
	var sb strings.Builder
	sb.WriteString("💱 FX Currency-Strength — session start\n")
	fmt.Fprintf(&sb, "Ranking basket: %s\n", strings.Join(cfg.RankInstruments, ", "))
	fmt.Fprintf(&sb, "Tradeable: %s\n", strings.Join(cfg.TradeInstruments, ", "))
	fmt.Fprintf(&sb, "Strength window: %s–%s %s · decision ~%s · exit %s",
		cfg.StrengthStart, cfg.StrengthEnd, tz, cfg.StrengthEnd, cfg.ExitTime)
	return sb.String()
}

// buildCurrencyStrengthEntryMessage announces a placed paper entry — the
// currency-strength analogue of the ORB "Brackets Placed" message. Reports the
// routed pair, side/size, and the strong/weak ranking that produced the signal.
func buildCurrencyStrengthEntryMessage(sig currencystrength.Signal, units int, orderID, status string) string {
	var sb strings.Builder
	sb.WriteString("✅ FX Currency-Strength — order placed\n")
	fmt.Fprintf(&sb, "%s %s · %d units (market)\n", strings.ToUpper(sig.Side), sig.Symbol, units)
	fmt.Fprintf(&sb, "Strongest %s · Weakest %s · gap %.4f\n", sig.Strong, sig.Weak, sig.Gap)
	fmt.Fprintf(&sb, "Order %s · %s", orderID, status)
	return sb.String()
}

// buildCurrencyStrengthNoOrderMessage explains why no paper order was placed on a
// decision day, so a silent session is distinguishable from an inactive one.
func buildCurrencyStrengthNoOrderMessage(reason string) string {
	if reason == "" {
		reason = "no qualifying signal"
	}
	return "💤 FX Currency-Strength — no order today\nReason: " + reason
}

// buildCurrencyStrengthExitMessage announces the flattening order placed at the
// configured exit time (or on a stale carried-over position).
func buildCurrencyStrengthExitMessage(symbol string, side execution.OrderSide, qty float64, orderID, status string) string {
	var sb strings.Builder
	sb.WriteString("🏁 FX Currency-Strength — position flattened\n")
	fmt.Fprintf(&sb, "%s %s · %.0f units (market)\n", strings.ToUpper(string(side)), symbol, qty)
	fmt.Fprintf(&sb, "Order %s · %s", orderID, status)
	return sb.String()
}

func shouldRunCurrencyStrengthForForexSession(session string) bool {
	switch strings.ToLower(strings.TrimSpace(session)) {
	case "", "london", "both":
		return true
	default:
		return false
	}
}

func shouldRunCurrencyStrengthPaperOrders(mode string) bool {
	return strings.EqualFold(strings.TrimSpace(mode), "paper")
}

func (r *currencyStrengthPaperRunner) Run(ctx context.Context) error {
	loc, err := time.LoadLocation(r.cfg.Timezone)
	if err != nil {
		return fmt.Errorf("currency-strength timezone %q: %w", r.cfg.Timezone, err)
	}
	startHM, err := parseCurrencyStrengthHHMM(r.cfg.StrengthStart)
	if err != nil {
		return fmt.Errorf("currency_strength.strength_start: %w", err)
	}
	endHM, err := parseCurrencyStrengthHHMM(r.cfg.StrengthEnd)
	if err != nil {
		return fmt.Errorf("currency_strength.strength_end: %w", err)
	}
	exitHM, err := parseCurrencyStrengthHHMM(r.cfg.ExitTime)
	if err != nil {
		return fmt.Errorf("currency_strength.exit_time: %w", err)
	}

	// Announce the FX-major basket once at session start — the currency-strength
	// analogue of the ORB "Scan Complete" listing. Fires per process launch, which
	// (via te-switch) is once per London session.
	r.notify(ctx, buildCurrencyStrengthSessionStartMessage(r.cfg))

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		now := r.now().In(loc)
		day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
		strengthStart := applyCurrencyStrengthClock(day, startHM)
		strengthEnd := applyCurrencyStrengthClock(day, endHM)
		exitTime := applyCurrencyStrengthClock(day, exitHM)
		dateKey := day.Format("2006-01-02")

		if r.sessionRecordedFor != dateKey {
			if err := r.startJournal(ctx, day); err != nil {
				return err
			}
			r.sessionRecordedFor = dateKey
		}

		if r.state != nil && !r.entryRecorded {
			if err := r.syncActiveEntryJournal(ctx); err != nil {
				// The position sidecar remains authoritative while the DB is
				// unavailable. Retry on the next tick without interrupting the
				// broker-managed position or submitting another order.
				logger.Warn("currency-strength: entry history sync failed; will retry", "err", err)
			}
		}

		if currencyStrengthShouldExitState(r.state, dateKey, now, exitTime) {
			if err := r.exitActive(ctx); err != nil {
				return err
			}
		}

		if r.state == nil && !now.Before(strengthEnd) && now.Before(exitTime) {
			if r.decisionDoneFor != dateKey {
				if now.Sub(strengthEnd) > currencyStrengthEntryGrace {
					logger.Info("currency-strength: entry window expired; no paper order placed",
						"date", dateKey, "strength_end", strengthEnd.Format(time.RFC3339), "grace", currencyStrengthEntryGrace.String())
					r.decisionDoneFor = dateKey
					if err := r.recordNoSignal(ctx, day, "entry window expired before evaluation"); err != nil {
						return err
					}
					r.notify(ctx, buildCurrencyStrengthNoOrderMessage("entry window expired before evaluation"))
				} else if err := r.evaluateAndEnter(ctx, dateKey, strengthStart, strengthEnd); err != nil {
					r.recordFailure(ctx, day, err)
					return err
				}
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (r *currencyStrengthPaperRunner) evaluateAndEnter(ctx context.Context, dateKey string, strengthStart, strengthEnd time.Time) error {
	sessionDate := time.Date(
		strengthStart.Year(), strengthStart.Month(), strengthStart.Day(),
		0, 0, 0, 0, strengthStart.Location(),
	)
	if r.journal != nil {
		if err := r.journal.MarkEvaluating(ctx, sessionDate); err != nil {
			return fmt.Errorf("currency-strength record evaluation start: %w", err)
		}
	}

	pairs, err := currencystrength.PairsFromSymbols(r.cfg.RankInstruments)
	if err != nil {
		return err
	}
	tradeSet := make(map[string]bool, len(r.cfg.TradeInstruments))
	for _, symbol := range r.cfg.TradeInstruments {
		tradeSet[strings.ToUpper(strings.TrimSpace(symbol))] = true
	}

	if existing, err := r.configuredPosition(ctx); err != nil {
		logger.Warn("currency-strength: skipping entry after position check failure", "date", dateKey, "err", err)
		r.decisionDoneFor = dateKey
		reason := "position check failed: " + err.Error()
		if journalErr := r.recordNoSignal(ctx, sessionDate, reason); journalErr != nil {
			return journalErr
		}
		r.notify(ctx, buildCurrencyStrengthNoOrderMessage(reason))
		return nil
	} else if existing != nil {
		logger.Info("currency-strength: skipping entry because configured IBKR position already exists without runner state",
			"date", dateKey, "ticker", existing.Ticker, "quantity", existing.Quantity)
		r.decisionDoneFor = dateKey
		reason := fmt.Sprintf("existing %s position (%.0f units) held outside runner state",
			existing.Ticker, existing.Quantity)
		if err := r.recordNoSignal(ctx, sessionDate, reason); err != nil {
			return err
		}
		r.notify(ctx, buildCurrencyStrengthNoOrderMessage(reason))
		return nil
	}

	windows := make(map[string]currencystrength.Window, len(pairs))
	for _, pair := range pairs {
		start, end, err := r.fetchWindow(ctx, pair.Symbol, strengthStart, strengthEnd)
		if err != nil {
			return err
		}
		windows[pair.Symbol] = currencystrength.Window{Start: start, End: end}
	}

	decision := currencystrength.Evaluate(pairs, windows, currencystrength.Config{
		MinGap:           r.cfg.MinGap,
		BroadMin:         r.cfg.BroadMin,
		ConfirmMinMove:   r.cfg.ConfirmMinMove,
		TradeInstruments: tradeSet,
	})
	r.decisionDoneFor = dateKey
	if decision.Signal == nil {
		logger.Info("currency-strength: no paper order", "date", dateKey, "reason", decision.SkipReason)
		if err := r.recordNoSignal(ctx, sessionDate, decision.SkipReason); err != nil {
			return err
		}
		r.notify(ctx, buildCurrencyStrengthNoOrderMessage(decision.SkipReason))
		return nil
	}

	side := execution.OrderSideBuy
	if decision.Signal.Side == "sell" {
		side = execution.OrderSideSell
	}
	tradeID := fmt.Sprintf("currency-strength-%s-%s", dateKey, strings.ReplaceAll(decision.Signal.Symbol, "/", ""))
	result, err := r.broker.PlaceOrder(ctx, execution.OrderRequest{
		Ticker:      decision.Signal.Symbol,
		Side:        side,
		Type:        execution.OrderTypeMarket,
		Quantity:    float64(r.cfg.Units),
		TimeInForce: "day",
		SecType:     "CASH",
		TradeID:     tradeID,
	})
	if err != nil {
		return fmt.Errorf("currency-strength place paper entry %s %s: %w", decision.Signal.Symbol, decision.Signal.Side, err)
	}

	enteredAt := r.now().UTC()
	r.state = &currencyStrengthRunState{
		Date:           dateKey,
		Symbol:         decision.Signal.Symbol,
		Side:           decision.Signal.Side,
		Quantity:       float64(r.cfg.Units),
		EntryOrderID:   result.BrokerOrderID,
		EntryStatus:    result.Status,
		EntryFillPrice: result.FilledPrice,
		EntryFilledQty: result.FilledQty,
		EnteredAt:      enteredAt.Format(time.RFC3339),
		Strongest:      decision.Signal.Strong,
		Weakest:        decision.Signal.Weak,
		StrengthGap:    decision.Signal.Gap,
	}
	if err := r.saveState(); err != nil {
		return err
	}
	if err := r.syncActiveEntryJournal(ctx); err != nil {
		// Do not stop after the broker has accepted an order. The saved sidecar
		// lets this process (or a restart) retry the history write safely.
		logger.Warn("currency-strength: entry history sync failed; will retry", "err", err)
	}
	logger.Info("currency-strength: IBKR paper entry submitted",
		"date", dateKey, "symbol", decision.Signal.Symbol, "side", decision.Signal.Side,
		"quantity", r.cfg.Units, "order_id", result.BrokerOrderID, "status", result.Status,
		"strong", decision.Signal.Strong, "weak", decision.Signal.Weak, "gap", decision.Signal.Gap)
	r.notify(ctx, buildCurrencyStrengthEntryMessage(*decision.Signal, r.cfg.Units, result.BrokerOrderID, result.Status))
	return nil
}

func (r *currencyStrengthPaperRunner) exitActive(ctx context.Context) error {
	if r.state == nil {
		return nil
	}
	// A prior iteration already submitted the flatten order and persisted its
	// acknowledgement in the sidecar. Retry only the DB journal write; never
	// place a second exit.
	if r.state.ExitOrderID != "" {
		return r.finalizeExitedState(ctx)
	}
	qty := math.Abs(r.state.Quantity)
	exitSide := execution.OrderSideSell
	if r.state.Side == "sell" {
		exitSide = execution.OrderSideBuy
	}
	if pos, err := r.positionFor(ctx, r.state.Symbol); err != nil {
		return err
	} else if pos != nil {
		qty = math.Abs(pos.Quantity)
		if pos.Quantity > 0 {
			exitSide = execution.OrderSideSell
		} else if pos.Quantity < 0 {
			exitSide = execution.OrderSideBuy
		} else {
			return r.clearState()
		}
	}
	if qty <= 0 {
		return r.clearState()
	}

	result, err := r.broker.PlaceOrder(ctx, execution.OrderRequest{
		Ticker:      r.state.Symbol,
		Side:        exitSide,
		Type:        execution.OrderTypeMarket,
		Quantity:    qty,
		TimeInForce: "day",
		SecType:     "CASH",
		TradeID:     fmt.Sprintf("currency-strength-exit-%s-%s", r.state.Date, strings.ReplaceAll(r.state.Symbol, "/", "")),
	})
	if err != nil {
		return fmt.Errorf("currency-strength place paper exit %s qty %.2f: %w", r.state.Symbol, qty, err)
	}
	exitedAt := r.now().UTC()
	r.state.ExitOrderID = result.BrokerOrderID
	r.state.ExitStatus = result.Status
	r.state.ExitFillPrice = result.FilledPrice
	r.state.ExitFilledQty = result.FilledQty
	r.state.ExitedAt = exitedAt.Format(time.RFC3339)
	if err := r.saveState(); err != nil {
		// Keep the in-memory exit acknowledgement so this process still cannot
		// submit a duplicate. The journal write below provides a second durable
		// recovery path if the sidecar filesystem is temporarily unavailable.
		logger.Warn("currency-strength: failed to persist exit acknowledgement sidecar", "err", err)
	}
	logger.Info("currency-strength: IBKR paper exit submitted",
		"date", r.state.Date, "symbol", r.state.Symbol, "side", exitSide,
		"quantity", qty, "order_id", result.BrokerOrderID, "status", result.Status)
	r.notify(ctx, buildCurrencyStrengthExitMessage(r.state.Symbol, exitSide, qty, result.BrokerOrderID, result.Status))
	return r.finalizeExitedState(ctx)
}

func (r *currencyStrengthPaperRunner) syncActiveEntryJournal(ctx context.Context) error {
	if r.journal == nil || r.state == nil {
		r.entryRecorded = true
		return nil
	}
	sessionDate, err := time.Parse("2006-01-02", r.state.Date)
	if err != nil {
		return fmt.Errorf("parse state date %q: %w", r.state.Date, err)
	}
	if err := r.journal.Start(ctx, store.CurrencyStrengthRun{
		SessionDate:      sessionDate,
		Timezone:         r.cfg.Timezone,
		StrengthStart:    r.cfg.StrengthStart,
		StrengthEnd:      r.cfg.StrengthEnd,
		ExitTime:         r.cfg.ExitTime,
		RankInstruments:  r.cfg.RankInstruments,
		TradeInstruments: r.cfg.TradeInstruments,
		Units:            r.cfg.Units,
	}); err != nil {
		return fmt.Errorf("record session for active entry: %w", err)
	}
	enteredAt, _ := time.Parse(time.RFC3339, r.state.EnteredAt)
	if err := r.journal.RecordEntry(ctx, store.CurrencyStrengthRun{
		SessionDate:       sessionDate,
		StrongestCurrency: r.state.Strongest,
		WeakestCurrency:   r.state.Weakest,
		StrengthGap:       r.state.StrengthGap,
		SignalSymbol:      r.state.Symbol,
		SignalSide:        r.state.Side,
		EntryOrderID:      r.state.EntryOrderID,
		EntryStatus:       r.state.EntryStatus,
		EntryFillPrice:    r.state.EntryFillPrice,
		EntryFilledQty:    r.state.EntryFilledQty,
		EnteredAt:         optionalTime(enteredAt),
	}); err != nil {
		return fmt.Errorf("record active entry: %w", err)
	}
	r.entryRecorded = true
	return nil
}

func (r *currencyStrengthPaperRunner) finalizeExitedState(ctx context.Context) error {
	if r.state == nil || r.state.ExitOrderID == "" {
		return nil
	}
	if r.journal != nil {
		if !r.entryRecorded {
			if err := r.syncActiveEntryJournal(ctx); err != nil {
				logger.Warn("currency-strength: entry history sync before exit failed; will retry", "err", err)
				return nil
			}
		}
		sessionDate, err := time.Parse("2006-01-02", r.state.Date)
		if err != nil {
			return fmt.Errorf("currency-strength parse state date %q: %w", r.state.Date, err)
		}
		exitedAt, _ := time.Parse(time.RFC3339, r.state.ExitedAt)
		var realizedPnL *float64
		if r.state.EntryFillPrice > 0 && r.state.ExitFillPrice > 0 {
			pnl := (r.state.ExitFillPrice - r.state.EntryFillPrice) * math.Abs(r.state.Quantity)
			if r.state.Side == "sell" {
				pnl = -pnl
			}
			realizedPnL = &pnl
		}
		if err := r.journal.RecordExit(ctx, store.CurrencyStrengthRun{
			SessionDate:   sessionDate,
			ExitOrderID:   r.state.ExitOrderID,
			ExitStatus:    r.state.ExitStatus,
			ExitFillPrice: r.state.ExitFillPrice,
			ExitFilledQty: r.state.ExitFilledQty,
			ExitedAt:      optionalTime(exitedAt),
			RealizedPnL:   realizedPnL,
		}); err != nil {
			logger.Warn("currency-strength: exit history sync failed; will retry", "err", err)
			return nil
		}
	}
	return r.clearState()
}

func optionalTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func (r *currencyStrengthPaperRunner) startJournal(ctx context.Context, sessionDate time.Time) error {
	if r.journal == nil {
		return nil
	}
	if err := r.journal.Start(ctx, store.CurrencyStrengthRun{
		SessionDate:      sessionDate,
		Timezone:         r.cfg.Timezone,
		StrengthStart:    r.cfg.StrengthStart,
		StrengthEnd:      r.cfg.StrengthEnd,
		ExitTime:         r.cfg.ExitTime,
		RankInstruments:  r.cfg.RankInstruments,
		TradeInstruments: r.cfg.TradeInstruments,
		Units:            r.cfg.Units,
	}); err != nil {
		return fmt.Errorf("currency-strength record session: %w", err)
	}
	return nil
}

func (r *currencyStrengthPaperRunner) recordNoSignal(
	ctx context.Context,
	sessionDate time.Time,
	reason string,
) error {
	if r.journal == nil {
		return nil
	}
	if err := r.journal.RecordNoSignal(ctx, sessionDate, reason); err != nil {
		return fmt.Errorf("currency-strength record no-signal outcome: %w", err)
	}
	return nil
}

func (r *currencyStrengthPaperRunner) recordFailure(
	ctx context.Context,
	sessionDate time.Time,
	runErr error,
) {
	if r.journal == nil || runErr == nil {
		return
	}
	if err := r.journal.RecordFailure(ctx, sessionDate, runErr.Error()); err != nil {
		logger.Warn("currency-strength: failed to record run failure", "err", err)
	}
}

func (r *currencyStrengthPaperRunner) fetchWindow(ctx context.Context, symbol string, strengthStart, strengthEnd time.Time) (float64, float64, error) {
	candles, err := r.market.FetchCandles(ctx, symbol, data.Timeframe1Min, strengthStart.Add(-2*time.Hour).UTC(), strengthEnd.Add(2*time.Minute).UTC())
	if err != nil {
		return 0, 0, fmt.Errorf("currency-strength fetch %s: %w", symbol, err)
	}
	start, okStart := closeAtOrBefore(candles, strengthStart.UTC())
	end, okEnd := closeAtOrBefore(candles, strengthEnd.UTC())
	if !okStart || !okEnd || start <= 0 || end <= 0 {
		return 0, 0, fmt.Errorf("currency-strength missing start/end price for %s at %s/%s", symbol, strengthStart.Format(time.RFC3339), strengthEnd.Format(time.RFC3339))
	}
	return start, end, nil
}

func closeAtOrBefore(candles []data.Candle, target time.Time) (float64, bool) {
	var out float64
	ok := false
	for _, c := range candles {
		if !c.Timestamp.After(target) {
			out = c.Close
			ok = true
		}
	}
	return out, ok
}

func (r *currencyStrengthPaperRunner) configuredPosition(ctx context.Context) (*execution.Position, error) {
	positions, err := r.broker.GetPositions(ctx)
	if err != nil {
		return nil, fmt.Errorf("currency-strength check positions: %w", err)
	}
	tradeSet := make(map[string]bool, len(r.cfg.TradeInstruments))
	for _, symbol := range r.cfg.TradeInstruments {
		tradeSet[strings.ToUpper(strings.TrimSpace(symbol))] = true
	}
	for i := range positions {
		if tradeSet[positions[i].Ticker] && math.Abs(positions[i].Quantity) > 0 {
			return &positions[i], nil
		}
	}
	return nil, nil
}

func (r *currencyStrengthPaperRunner) positionFor(ctx context.Context, symbol string) (*execution.Position, error) {
	positions, err := r.broker.GetPositions(ctx)
	if err != nil {
		return nil, fmt.Errorf("currency-strength check exit position: %w", err)
	}
	for i := range positions {
		if positions[i].Ticker == symbol && math.Abs(positions[i].Quantity) > 0 {
			return &positions[i], nil
		}
	}
	return nil, nil
}

func (r *currencyStrengthPaperRunner) loadState() error {
	b, err := os.ReadFile(r.statePath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("currency-strength read state: %w", err)
	}
	var state currencyStrengthRunState
	if err := json.Unmarshal(b, &state); err != nil {
		return fmt.Errorf("currency-strength decode state: %w", err)
	}
	if state.Symbol != "" && state.Quantity > 0 {
		r.state = &state
	}
	return nil
}

func (r *currencyStrengthPaperRunner) saveState() error {
	if r.state == nil {
		return r.clearState()
	}
	if err := os.MkdirAll(filepath.Dir(r.statePath), 0750); err != nil {
		return fmt.Errorf("currency-strength create state dir: %w", err)
	}
	b, err := json.MarshalIndent(r.state, "", "  ")
	if err != nil {
		return fmt.Errorf("currency-strength encode state: %w", err)
	}
	return os.WriteFile(r.statePath, b, 0600)
}

func (r *currencyStrengthPaperRunner) clearState() error {
	r.state = nil
	if err := os.Remove(r.statePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("currency-strength remove state: %w", err)
	}
	return nil
}

func defaultCurrencyStrengthStatePath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "tradeengine-currency-strength-state.json")
	}
	return filepath.Join(home, ".tradeengine", "currency_strength_state.json")
}

func isIBKRPaperAccountID(accountID string) bool {
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(accountID)), "DU")
}

func currencyStrengthShouldExitState(state *currencyStrengthRunState, dateKey string, now, exitTime time.Time) bool {
	if state == nil {
		return false
	}
	return state.Date != dateKey || !now.Before(exitTime)
}

type currencyStrengthClock struct {
	hour   int
	minute int
}

func parseCurrencyStrengthHHMM(raw string) (currencyStrengthClock, error) {
	t, err := time.Parse("15:04", raw)
	if err != nil {
		return currencyStrengthClock{}, err
	}
	return currencyStrengthClock{hour: t.Hour(), minute: t.Minute()}, nil
}

func applyCurrencyStrengthClock(day time.Time, hm currencyStrengthClock) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day(), hm.hour, hm.minute, 0, 0, day.Location())
}
