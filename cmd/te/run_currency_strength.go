package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
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
const currencyStrengthTradeRecoveryInterval = time.Minute
const currencyStrengthPortfolioPropagationGrace = 2 * time.Minute

// sessionNotifier delivers currency-strength session notifications (e.g. Telegram).
// Structurally satisfied by *orbTelegramNotifier, so the currency-strength runner
// reuses the same Telegram adapter the ORB scheduler uses without importing orb.
type sessionNotifier interface {
	Notify(ctx context.Context, msg string) error
}

type currencyStrengthRunState struct {
	AccountID         string                      `json:"account_id"`
	Date              string                      `json:"date"`
	Symbol            string                      `json:"symbol"`
	Side              string                      `json:"side"`
	Quantity          float64                     `json:"quantity"`
	EntryOrderID      string                      `json:"entry_order_id"`
	EntryStatus       string                      `json:"entry_status"`
	EntryFillPrice    float64                     `json:"entry_fill_price,omitempty"`
	EntryFilledQty    float64                     `json:"entry_filled_qty,omitempty"`
	EnteredAt         string                      `json:"entered_at"`
	Strongest         string                      `json:"strongest,omitempty"`
	Weakest           string                      `json:"weakest,omitempty"`
	StrengthGap       float64                     `json:"strength_gap,omitempty"`
	ExitOrderID       string                      `json:"exit_order_id,omitempty"`
	ExitTradeID       string                      `json:"exit_trade_id,omitempty"`
	ExitAttempt       int                         `json:"exit_attempt,omitempty"`
	ExitStatus        string                      `json:"exit_status,omitempty"`
	ExitFillPrice     float64                     `json:"exit_fill_price,omitempty"`
	ExitFilledQty     float64                     `json:"exit_filled_qty,omitempty"`
	ExitRequestedQty  float64                     `json:"exit_requested_qty,omitempty"`
	ExitFillObserved  string                      `json:"exit_fill_observed_at,omitempty"`
	ExitFillTimeQty   float64                     `json:"exit_fill_time_qty,omitempty"`
	ExitedAt          string                      `json:"exited_at,omitempty"`
	ExitRetry         bool                        `json:"exit_retry,omitempty"`
	ExitConfirmed     bool                        `json:"exit_confirmed_notified,omitempty"`
	PendingExitAudits []currencyStrengthExitAudit `json:"pending_exit_audits,omitempty"`
}

type currencyStrengthExitAudit struct {
	OrderID   string  `json:"order_id"`
	Status    string  `json:"status"`
	FillPrice float64 `json:"fill_price,omitempty"`
	FilledQty float64 `json:"filled_qty,omitempty"`
	ExitedAt  string  `json:"exited_at,omitempty"`
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
	Get(context.Context, time.Time) (*store.CurrencyStrengthRun, error)
	GetActive(context.Context, time.Time) (*store.CurrencyStrengthRun, error)
}

type dbCurrencyStrengthJournal struct {
	accountID string
}

func (d dbCurrencyStrengthJournal) Start(ctx context.Context, run store.CurrencyStrengthRun) error {
	run.AccountID = d.accountID
	return store.StartCurrencyStrengthRun(ctx, store.GetDB(), run)
}

func (d dbCurrencyStrengthJournal) MarkEvaluating(ctx context.Context, date time.Time) error {
	return store.MarkCurrencyStrengthEvaluating(ctx, store.GetDB(), d.accountID, date)
}

func (d dbCurrencyStrengthJournal) RecordNoSignal(ctx context.Context, date time.Time, reason string) error {
	return store.RecordCurrencyStrengthNoSignal(ctx, store.GetDB(), d.accountID, date, reason)
}

func (d dbCurrencyStrengthJournal) RecordEntry(ctx context.Context, run store.CurrencyStrengthRun) error {
	run.AccountID = d.accountID
	return store.RecordCurrencyStrengthEntry(ctx, store.GetDB(), run)
}

func (d dbCurrencyStrengthJournal) RecordExit(ctx context.Context, run store.CurrencyStrengthRun) error {
	run.AccountID = d.accountID
	return store.RecordCurrencyStrengthExit(ctx, store.GetDB(), run)
}

func (d dbCurrencyStrengthJournal) RecordFailure(ctx context.Context, date time.Time, message string) error {
	return store.RecordCurrencyStrengthFailure(ctx, store.GetDB(), d.accountID, date, message)
}

func (d dbCurrencyStrengthJournal) Get(ctx context.Context, date time.Time) (*store.CurrencyStrengthRun, error) {
	return store.GetCurrencyStrengthRunByDate(ctx, store.GetDB(), d.accountID, date)
}

func (d dbCurrencyStrengthJournal) GetActive(ctx context.Context, date time.Time) (*store.CurrencyStrengthRun, error) {
	return store.GetActiveCurrencyStrengthRun(ctx, store.GetDB(), d.accountID, date)
}

type currencyStrengthPaperRunner struct {
	cfg       config.CurrencyStrengthConfig
	accountID string
	market    data.MarketDataProvider
	broker    execution.Executor
	statePath string
	// legacyStatePath is read only when the account-scoped sidecar does not
	// exist. It supports one-time migration from the pre-account-scoping file.
	legacyStatePath   string
	claimedLegacyPath string
	now               func() time.Time
	notifier          sessionNotifier // optional; nil disables Telegram notifications
	journal           currencyStrengthJournal

	state              *currencyStrengthRunState
	decisionDoneFor    string
	sessionRecordedFor string
	entryRecorded      bool
	nextTradeRecovery  time.Time
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

func newCurrencyStrengthPaperRunner(
	cfg config.CurrencyStrengthConfig,
	accountID string,
	market data.MarketDataProvider,
	broker execution.Executor,
) *currencyStrengthPaperRunner {
	return &currencyStrengthPaperRunner{
		cfg:             cfg,
		accountID:       normalizeCurrencyStrengthAccountID(accountID),
		market:          market,
		broker:          broker,
		statePath:       defaultCurrencyStrengthStatePath(accountID),
		legacyStatePath: defaultLegacyCurrencyStrengthStatePath(),
		now:             time.Now,
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

func loadCurrencyStrengthRecovery(r *currencyStrengthPaperRunner) error {
	if err := r.loadState(); err != nil {
		if r.journal == nil {
			return err
		}
		// PostgreSQL is an independent recovery source. A corrupt/unreadable
		// sidecar must not prevent it from restoring an active obligation.
		logger.Warn("currency-strength: sidecar unavailable; falling back to journal recovery", "err", err)
		r.state = nil
	}
	return nil
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

// buildCurrencyStrengthExitMessage announces submission without claiming that
// the broker has already flattened the position.
func buildCurrencyStrengthExitMessage(symbol string, side execution.OrderSide, qty float64, orderID, status string) string {
	var sb strings.Builder
	sb.WriteString("⏳ FX Currency-Strength — exit submitted\n")
	fmt.Fprintf(&sb, "%s %s · %.0f units (market)\n", strings.ToUpper(string(side)), symbol, qty)
	fmt.Fprintf(&sb, "Order %s · %s", orderID, status)
	return sb.String()
}

func buildCurrencyStrengthFlattenedMessage(symbol, orderID string, qty, price float64) string {
	var sb strings.Builder
	sb.WriteString("🏁 FX Currency-Strength — position flattened\n")
	fmt.Fprintf(&sb, "%s · %.0f units confirmed closed\n", symbol, qty)
	fmt.Fprintf(&sb, "Order %s", orderID)
	if price > 0 {
		fmt.Fprintf(&sb, " · fill %.5f", price)
	}
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
				// Broker-only recovery can become visible after startup, so a
				// journal outage must never stop reconciliation even when no
				// sidecar obligation has been restored yet. Leave the marker
				// unset, retry next tick, and suppress new entries until the
				// daily audit row is available.
				logger.Warn("currency-strength: session history unavailable; continuing broker recovery", "err", err)
			} else {
				if err := r.restoreJournalState(ctx, day); err != nil {
					logger.Warn("currency-strength: history restore failed; continuing broker recovery", "err", err)
				} else {
					r.sessionRecordedFor = dateKey
				}
			}
		}

		if r.state == nil {
			if _, err := r.recoverEntryObligation(ctx); err != nil {
				logger.Warn("currency-strength: broker entry recovery failed; will retry", "err", err)
			}
		}

		if r.state != nil {
			if err := r.mergeActiveStateFromJournal(ctx); err != nil {
				logger.Warn("currency-strength: active-state history merge failed; using broker recovery", "err", err)
			}
			if r.state != nil && r.state.ExitOrderID == "" {
				if err := r.reconcileActiveEntry(ctx); err != nil {
					logger.Warn("currency-strength: entry reconciliation failed; will retry", "err", err)
				}
			}
			if r.state != nil && !r.entryRecorded {
				if err := r.syncActiveEntryJournal(ctx); err != nil {
					// The position sidecar remains authoritative while the DB is
					// unavailable. Retry on the next tick without interrupting the
					// broker-managed position or submitting another order.
					logger.Warn("currency-strength: entry history sync failed; will retry", "err", err)
				}
			}
		}

		if r.state != nil && (r.state.ExitOrderID != "" ||
			currencyStrengthShouldExitState(r.state, dateKey, now, exitTime)) {
			if err := r.exitActive(ctx); err != nil {
				return err
			}
		}

		if r.state == nil && r.journal != nil && r.sessionRecordedFor == dateKey &&
			!now.Before(strengthEnd) && now.Before(exitTime) {
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

func (r *currencyStrengthPaperRunner) recoverEntryObligation(ctx context.Context) (bool, error) {
	if recovered, err := r.recoverWorkingEntry(ctx); recovered || err != nil {
		return recovered, err
	}
	now := r.currencyStrengthNow()
	if !r.nextTradeRecovery.IsZero() && now.Before(r.nextTradeRecovery) {
		return false, nil
	}
	r.nextTradeRecovery = now.Add(currencyStrengthTradeRecoveryInterval)
	records, err := r.currencyStrengthTradeRecords(ctx)
	if err != nil {
		return false, err
	}
	if len(records) == 0 {
		return false, nil
	}
	positions, err := r.broker.GetPositions(ctx)
	if err != nil {
		return false, fmt.Errorf("list positions for filled-entry recovery: %w", err)
	}
	sort.Slice(records, func(i, j int) bool {
		return records[i].FilledAt.Before(records[j].FilledAt)
	})

	var recovered *currencyStrengthRunState
	seenEntries := make(map[string]bool)
	for i, entry := range records {
		dateKey, symbol, ok := parseCurrencyStrengthEntryTradeID(entry.TradeID)
		if !ok || seenEntries[entry.TradeID] || !r.currencyStrengthTradeable(symbol) {
			continue
		}
		seenEntries[entry.TradeID] = true
		entrySide := strings.ToLower(entry.Side)
		entrySign := currencyStrengthSideSign(entrySide)
		if entrySign == 0 {
			continue
		}
		var entryQty, entryNotional float64
		for j := i; j < len(records); j++ {
			fill := records[j]
			if fill.TradeID == entry.TradeID {
				entryQty += fill.Qty
				entryNotional += fill.Price * fill.Qty
			}
		}
		var netQty float64
		closed, contaminated := false, false
		for j := i; j < len(records); j++ {
			fill := records[j]
			if !currencyStrengthRecordMatchesSymbol(fill, symbol) {
				continue
			}
			netQty += currencyStrengthSideSign(fill.Side) * fill.Qty
			if entryQty > 0 && math.Abs(netQty) > entryQty+1e-6 {
				contaminated = true
			}
			if fill.TradeID != entry.TradeID && math.Abs(netQty) < 1e-6 {
				closed = true
				break
			}
		}
		if closed || contaminated || entryQty <= 0 || entryNotional <= 0 ||
			math.Abs(netQty) > entryQty+1e-6 || netQty*entrySign <= 0 {
			continue
		}
		for j := range positions {
			pos := positions[j]
			if normalizeCurrencyStrengthTicker(pos.Ticker) != normalizeCurrencyStrengthTicker(symbol) ||
				!currencyStrengthQuantitiesMatch(pos.Quantity, netQty) {
				continue
			}
			entryStatus, statusErr := r.recoveredEntryStatus(ctx, entry.OrderID, entryQty)
			if statusErr != nil {
				return false, statusErr
			}
			candidate := &currencyStrengthRunState{
				AccountID:      r.accountID,
				Date:           dateKey,
				Symbol:         symbol,
				Side:           entrySide,
				Quantity:       math.Abs(netQty),
				EntryOrderID:   entry.OrderID,
				EntryStatus:    entryStatus,
				EntryFillPrice: entryNotional / entryQty,
				EntryFilledQty: entryQty,
				EnteredAt:      entry.FilledAt.UTC().Format(time.RFC3339),
			}
			if recovered == nil || candidate.EnteredAt > recovered.EnteredAt {
				recovered = candidate
			}
		}
	}
	if recovered == nil {
		return false, nil
	}
	r.state = recovered
	if err := r.persistActiveEntryRecovery(ctx); err != nil {
		logger.Warn("currency-strength: recovered filled entry remains non-durable", "err", err)
	}
	logger.Warn("currency-strength: recovered strategy-owned filled entry",
		"symbol", recovered.Symbol, "order_id", recovered.EntryOrderID)
	return true, nil
}

func (r *currencyStrengthPaperRunner) recoveredEntryStatus(
	ctx context.Context,
	orderID string,
	filledQty float64,
) (string, error) {
	result, err := r.broker.GetOrderStatus(ctx, orderID)
	if err != nil {
		if !errors.Is(err, execution.ErrOrderNotFound) {
			return "", fmt.Errorf("currency-strength recover entry status %s: %w", orderID, err)
		}
		if filledQty+1e-6 >= float64(r.cfg.Units) {
			return "filled", nil
		}
		return "cancelled", nil
	}
	status := normalizeCurrencyStrengthOrderStatus(result.Status)
	if status == "pending" && filledQty > 0 {
		status = "partial"
	}
	return status, nil
}

func (r *currencyStrengthPaperRunner) recoverWorkingEntry(ctx context.Context) (bool, error) {
	lister, ok := r.broker.(execution.WorkingOrdersLister)
	if !ok {
		return false, nil
	}
	orders, err := lister.GetWorkingOrders(ctx)
	if err != nil {
		return false, fmt.Errorf("list working entries for recovery: %w", err)
	}
	for _, order := range orders {
		dateKey, symbol, owned := parseCurrencyStrengthEntryTradeID(order.TradeID)
		if !owned || !r.currencyStrengthTradeable(symbol) ||
			normalizeCurrencyStrengthTicker(order.Ticker) != normalizeCurrencyStrengthTicker(symbol) {
			continue
		}
		r.state = &currencyStrengthRunState{
			AccountID:    r.accountID,
			Date:         dateKey,
			Symbol:       symbol,
			Side:         strings.ToLower(order.Side),
			Quantity:     float64(r.cfg.Units),
			EntryOrderID: order.OrderID,
			EntryStatus:  order.Status,
			EnteredAt:    r.currencyStrengthNow().UTC().Format(time.RFC3339),
		}
		if err := r.persistActiveEntryRecovery(ctx); err != nil {
			logger.Warn("currency-strength: recovered working entry remains non-durable", "err", err)
		}
		logger.Warn("currency-strength: recovered strategy-owned working entry",
			"symbol", symbol, "order_id", order.OrderID, "trade_id", order.TradeID)
		return true, nil
	}
	return false, nil
}

func (r *currencyStrengthPaperRunner) currencyStrengthTradeRecords(
	ctx context.Context,
) ([]execution.IBKRTradeRecord, error) {
	type tradeRecordFetcher interface {
		FetchTradeRecords(context.Context, int) ([]execution.IBKRTradeRecord, error)
	}
	if fetcher, ok := r.broker.(tradeRecordFetcher); ok {
		records, err := fetcher.FetchTradeRecords(ctx, 7)
		if err != nil {
			return nil, fmt.Errorf("list strategy trade records: %w", err)
		}
		return records, nil
	}
	type recentFillFetcher interface {
		RecentFills(context.Context) ([]execution.BrokerOrder, error)
	}
	fetcher, ok := r.broker.(recentFillFetcher)
	if !ok {
		return nil, nil
	}
	fills, err := fetcher.RecentFills(ctx)
	if err != nil {
		return nil, fmt.Errorf("list recent strategy fills: %w", err)
	}
	records := make([]execution.IBKRTradeRecord, 0, len(fills))
	for _, fill := range fills {
		records = append(records, execution.IBKRTradeRecord{
			OrderID: fill.BrokerOrderID, TradeID: fill.TradeID, Symbol: fill.Symbol,
			Side: fill.Side, Qty: fill.FilledQty, Price: fill.FilledAvgPx,
			FilledAt: fill.FilledAt,
		})
	}
	return records, nil
}

func (r *currencyStrengthPaperRunner) currencyStrengthTradeable(symbol string) bool {
	want := normalizeCurrencyStrengthTicker(symbol)
	for _, configured := range r.cfg.TradeInstruments {
		if normalizeCurrencyStrengthTicker(configured) == want {
			return true
		}
	}
	return false
}

func currencyStrengthSideSign(side string) float64 {
	if strings.EqualFold(side, "buy") {
		return 1
	}
	if strings.EqualFold(side, "sell") {
		return -1
	}
	return 0
}

func currencyStrengthQuantitiesMatch(positionQty, expectedQty float64) bool {
	tolerance := math.Max(1e-6, math.Abs(expectedQty)*1e-8)
	return math.Abs(positionQty-expectedQty) <= tolerance
}

func parseCurrencyStrengthEntryTradeID(tradeID string) (dateKey, symbol string, ok bool) {
	const prefix = "currency-strength-"
	if !strings.HasPrefix(tradeID, prefix) || strings.HasPrefix(tradeID, prefix+"exit-") {
		return "", "", false
	}
	rest := strings.TrimPrefix(tradeID, prefix)
	if len(rest) < len("2006-01-02-X") || rest[10] != '-' {
		return "", "", false
	}
	dateKey, compact := rest[:10], rest[11:]
	if _, err := time.Parse("2006-01-02", dateKey); err != nil || compact == "" {
		return "", "", false
	}
	if len(compact) != 6 {
		return "", "", false
	}
	return dateKey, compact[:3] + "/" + compact[3:], true
}

func parseCurrencyStrengthExitTradeID(tradeID string) (dateKey, symbol string, ok bool) {
	const prefix = "currency-strength-exit-"
	if !strings.HasPrefix(tradeID, prefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(tradeID, prefix)
	if len(rest) < len("2006-01-02-XXXXXX") || rest[10] != '-' {
		return "", "", false
	}
	dateKey, compact := rest[:10], rest[11:]
	if _, err := time.Parse("2006-01-02", dateKey); err != nil || len(compact) < 6 {
		return "", "", false
	}
	compact = compact[:6]
	return dateKey, compact[:3] + "/" + compact[3:], true
}

func currencyStrengthRecordMatchesSymbol(record execution.IBKRTradeRecord, symbol string) bool {
	if normalizeCurrencyStrengthTicker(record.Symbol) == normalizeCurrencyStrengthTicker(symbol) {
		return true
	}
	if _, ownedSymbol, ok := parseCurrencyStrengthEntryTradeID(record.TradeID); ok {
		return normalizeCurrencyStrengthTicker(ownedSymbol) == normalizeCurrencyStrengthTicker(symbol)
	}
	if _, ownedSymbol, ok := parseCurrencyStrengthExitTradeID(record.TradeID); ok {
		return normalizeCurrencyStrengthTicker(ownedSymbol) == normalizeCurrencyStrengthTicker(symbol)
	}
	return false
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
	if adopted, adoptErr := r.adoptWorkingEntry(ctx, dateKey, *decision.Signal, tradeID); adoptErr != nil {
		return adoptErr
	} else if adopted {
		return nil
	}
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
		AccountID:      r.accountID,
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
	if err := r.persistActiveEntryRecovery(ctx); err != nil {
		return err
	}
	logger.Info("currency-strength: IBKR paper entry submitted",
		"date", dateKey, "symbol", decision.Signal.Symbol, "side", decision.Signal.Side,
		"quantity", r.cfg.Units, "order_id", result.BrokerOrderID, "status", result.Status,
		"strong", decision.Signal.Strong, "weak", decision.Signal.Weak, "gap", decision.Signal.Gap)
	r.notify(ctx, buildCurrencyStrengthEntryMessage(*decision.Signal, r.cfg.Units, result.BrokerOrderID, result.Status))
	return nil
}

func (r *currencyStrengthPaperRunner) adoptWorkingEntry(
	ctx context.Context,
	dateKey string,
	signal currencystrength.Signal,
	tradeID string,
) (bool, error) {
	lister, ok := r.broker.(execution.WorkingOrdersLister)
	if !ok {
		return false, nil
	}
	orders, err := lister.GetWorkingOrders(ctx)
	if err != nil {
		return false, fmt.Errorf("currency-strength list working entries: %w", err)
	}
	wantTicker := normalizeCurrencyStrengthTicker(signal.Symbol)
	for _, order := range orders {
		if order.TradeID != tradeID ||
			normalizeCurrencyStrengthTicker(order.Ticker) != wantTicker ||
			!strings.EqualFold(order.Side, signal.Side) {
			continue
		}
		r.state = &currencyStrengthRunState{
			AccountID:    r.accountID,
			Date:         dateKey,
			Symbol:       signal.Symbol,
			Side:         signal.Side,
			Quantity:     float64(r.cfg.Units),
			EntryOrderID: order.OrderID,
			EntryStatus:  order.Status,
			EnteredAt:    r.now().UTC().Format(time.RFC3339),
			Strongest:    signal.Strong,
			Weakest:      signal.Weak,
			StrengthGap:  signal.Gap,
		}
		if persistErr := r.persistActiveEntryRecovery(ctx); persistErr != nil {
			// In-memory ownership plus deterministic broker discovery still
			// prevents a duplicate during this process and after restart.
			logger.Warn("currency-strength: adopted entry recovery remains non-durable", "err", persistErr)
		}
		logger.Warn("currency-strength: adopted strategy-owned working entry during recovery",
			"symbol", signal.Symbol, "order_id", order.OrderID, "trade_id", tradeID)
		return true, nil
	}
	return false, nil
}

func (r *currencyStrengthPaperRunner) persistActiveEntryRecovery(ctx context.Context) error {
	sidecarErr := r.saveState()
	journalErr := r.syncActiveEntryJournal(ctx)
	switch {
	case sidecarErr != nil && (journalErr != nil || r.journal == nil):
		return fmt.Errorf(
			"currency-strength entry accepted but no recovery source persisted (sidecar: %v; journal: %v)",
			sidecarErr, journalErr,
		)
	case sidecarErr != nil:
		logger.Warn("currency-strength: entry sidecar unavailable; durable journal will drive recovery", "err", sidecarErr)
	case journalErr != nil:
		// Do not stop after the broker has accepted an order. The saved sidecar
		// lets this process (or a restart) retry the history write safely.
		logger.Warn("currency-strength: entry history sync failed; will retry", "err", journalErr)
	}
	return nil
}

func (r *currencyStrengthPaperRunner) exitActive(ctx context.Context) error {
	if r.state == nil {
		return nil
	}
	if err := r.syncPendingExitAudit(ctx); err != nil {
		// Audit retry is independent of broker-truth risk management. Retain
		// the durable snapshot and continue flattening any live exposure.
		logger.Warn("currency-strength: prior exit audit sync failed; continuing broker exit", "err", err)
	}
	// A prior iteration already submitted the flatten order and persisted its
	// acknowledgement in the sidecar. Retry only the DB journal write; never
	// place a second exit.
	if r.state.ExitOrderID != "" {
		return r.finalizeExitedState(ctx)
	}
	if adopted, err := r.adoptFilledExit(ctx); err != nil {
		return err
	} else if adopted {
		return r.finalizeExitedState(ctx)
	}
	exitSide := execution.OrderSideSell
	if r.state.Side == "sell" {
		exitSide = execution.OrderSideBuy
	}
	pos, err := r.positionFor(ctx, r.state.Symbol)
	if err != nil {
		return err
	}
	if pos == nil {
		// Never submit a blind opposite-side market order. The entry may have
		// remained unfilled, or a prior unrecorded flatten may already have made
		// the broker flat.
		entryStatus := normalizeCurrencyStrengthOrderStatus(r.state.EntryStatus)
		if (entryStatus == "pending" || entryStatus == "partial") &&
			r.state.EntryOrderID != "" {
			settled, settleErr := r.cancelAndSettleEntry(ctx)
			if settleErr != nil {
				return settleErr
			}
			if !settled {
				return nil
			}
			// Close the cancel/fill race before discarding recovery state.
			// A market entry can fill between the first position snapshot and
			// cancellation acknowledgement.
			var positionErr error
			pos, positionErr = r.positionFor(ctx, r.state.Symbol)
			if positionErr != nil {
				return positionErr
			}
			if pos == nil {
				entryStatus = normalizeCurrencyStrengthOrderStatus(r.state.EntryStatus)
				if entryStatus == "filled" || r.state.EntryFilledQty > 0 {
					logger.Warn("currency-strength: terminal entry has fills but broker position is not visible; retaining recovery state",
						"order_id", r.state.EntryOrderID, "status", entryStatus,
						"filled_qty", r.state.EntryFilledQty)
					return nil
				}
				r.entryRecorded = false
				if err := r.syncActiveEntryJournal(ctx); err != nil {
					logger.Warn("currency-strength: cancelled entry history sync failed; clearing broker-flat state", "err", err)
				}
				return r.clearState()
			}
			r.state.EntryStatus = "filled"
			r.state.EntryFillPrice = pos.AvgCostBasis
			r.state.EntryFilledQty = math.Abs(pos.Quantity)
			r.state.Quantity = math.Abs(pos.Quantity)
			r.entryRecorded = false
			if err := r.saveState(); err != nil {
				return err
			}
			if err := r.syncActiveEntryJournal(ctx); err != nil {
				logger.Warn("currency-strength: filled entry history sync failed; continuing broker exit", "err", err)
			}
		}
		if pos == nil {
			r.state.ExitOrderID = "broker-flat"
			r.state.ExitStatus = "filled"
			r.state.ExitRequestedQty = 0
			r.state.ExitFillObserved = ""
			r.state.ExitFillTimeQty = 0
			r.state.ExitedAt = r.now().UTC().Format(time.RFC3339)
			if err := r.saveState(); err != nil {
				logger.Warn("currency-strength: failed to persist broker-flat recovery", "err", err)
			}
			return r.finalizeExitedState(ctx)
		}
	}
	entryStatus := normalizeCurrencyStrengthOrderStatus(r.state.EntryStatus)
	if (entryStatus == "pending" || entryStatus == "partial") &&
		r.state.EntryOrderID != "" {
		// Freeze the entry quantity before flattening so an unfilled remainder
		// cannot execute after the exit and recreate exposure.
		settled, err := r.cancelAndSettleEntry(ctx)
		if err != nil {
			return err
		}
		if !settled {
			return nil
		}
		// Cancellation acceptance is not cancellation completion at IBKR.
		// Refresh broker truth only after the order reaches a terminal state.
		pos, err = r.positionFor(ctx, r.state.Symbol)
		if err != nil {
			return err
		}
		if pos == nil {
			r.state.ExitOrderID = "broker-flat"
			r.state.ExitStatus = "filled"
			r.state.ExitRequestedQty = 0
			r.state.ExitFillObserved = ""
			r.state.ExitFillTimeQty = 0
			r.state.ExitedAt = r.now().UTC().Format(time.RFC3339)
			if err := r.saveState(); err != nil {
				logger.Warn("currency-strength: failed to persist broker-flat recovery", "err", err)
			}
			return r.finalizeExitedState(ctx)
		}
		r.state.EntryStatus = "filled"
		r.state.EntryFillPrice = pos.AvgCostBasis
		r.state.EntryFilledQty = math.Abs(pos.Quantity)
		r.state.Quantity = math.Abs(pos.Quantity)
		r.entryRecorded = false
		if err := r.saveState(); err != nil {
			return fmt.Errorf("persist finalized entry quantity: %w", err)
		}
		if err := r.syncActiveEntryJournal(ctx); err != nil {
			logger.Warn("currency-strength: finalized entry history sync failed; continuing broker exit", "err", err)
		}
	}
	qty := math.Abs(pos.Quantity)
	if pos.Quantity > 0 {
		exitSide = execution.OrderSideSell
	} else if pos.Quantity < 0 {
		exitSide = execution.OrderSideBuy
	}
	if qty <= 0 {
		return r.clearState()
	}

	if adopted, err := r.adoptWorkingExit(ctx, exitSide); err != nil {
		return err
	} else if adopted {
		return r.finalizeExitedState(ctx)
	}

	r.state.ExitRequestedQty = qty
	r.state.ExitFillObserved = ""
	r.state.ExitFillTimeQty = 0
	exitTradeID, err := r.reserveExitTradeID()
	if err != nil {
		return err
	}
	result, err := r.broker.PlaceOrder(ctx, execution.OrderRequest{
		Ticker:      r.state.Symbol,
		Side:        exitSide,
		Type:        execution.OrderTypeMarket,
		Quantity:    qty,
		TimeInForce: "day",
		SecType:     "CASH",
		TradeID:     exitTradeID,
	})
	if err != nil {
		return fmt.Errorf("currency-strength place paper exit %s qty %.2f: %w", r.state.Symbol, qty, err)
	}
	exitedAt := r.now().UTC()
	r.state.ExitOrderID = result.BrokerOrderID
	r.state.ExitStatus = result.Status
	r.state.ExitFillPrice = result.FilledPrice
	r.state.ExitFilledQty = result.FilledQty
	if normalizeCurrencyStrengthOrderStatus(result.Status) == "filled" {
		r.state.ExitFillTimeQty = result.FilledQty
	}
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

func (r *currencyStrengthPaperRunner) adoptFilledExit(ctx context.Context) (bool, error) {
	if r.state == nil {
		return false, nil
	}
	if r.state.ExitRetry && r.state.ExitTradeID == "" {
		return false, nil
	}
	records, err := r.currencyStrengthTradeRecords(ctx)
	if err != nil {
		return false, fmt.Errorf("currency-strength recover filled exit: %w", err)
	}
	wantRef := r.exitTradeID()
	baseRef := r.baseExitTradeID()
	exact := r.state.ExitTradeID != ""
	var orderID, discoveredRef string
	var filledQty, fillNotional float64
	var filledAt time.Time
	for _, record := range records {
		if !currencyStrengthExitReferenceMatches(record.TradeID, wantRef, baseRef, exact) {
			continue
		}
		if (orderID != "" && record.OrderID != orderID) ||
			(discoveredRef != "" && record.TradeID != discoveredRef) {
			return false, fmt.Errorf(
				"currency-strength exit prefix %q maps to multiple broker orders",
				baseRef,
			)
		}
		orderID = record.OrderID
		discoveredRef = record.TradeID
		filledQty += record.Qty
		fillNotional += record.Price * record.Qty
		if record.FilledAt.After(filledAt) {
			filledAt = record.FilledAt
		}
	}
	if orderID == "" || filledQty <= 0 {
		return false, nil
	}
	result, statusErr := r.broker.GetOrderStatus(ctx, orderID)
	status := ""
	if statusErr != nil {
		if !errors.Is(statusErr, execution.ErrOrderNotFound) {
			return false, fmt.Errorf("currency-strength recover exit status %s: %w", orderID, statusErr)
		}
		requested := r.state.ExitRequestedQty
		if requested <= 0 {
			requested = r.state.Quantity
		}
		if requested > 0 && filledQty+1e-6 < requested {
			status = "cancelled"
		} else {
			status = "filled"
		}
	} else {
		status = normalizeCurrencyStrengthOrderStatus(result.Status)
		if status == "pending" && filledQty > 0 {
			status = "partial"
		}
	}
	r.state.ExitTradeID = discoveredRef
	r.state.ExitOrderID = orderID
	r.state.ExitStatus = status
	r.state.ExitFilledQty = filledQty
	r.state.ExitFillPrice = fillNotional / filledQty
	r.state.ExitFillTimeQty = filledQty
	if r.state.ExitRequestedQty <= 0 {
		r.state.ExitRequestedQty = r.state.Quantity
	}
	if filledAt.IsZero() {
		filledAt = r.currencyStrengthNow().UTC()
	}
	r.state.ExitedAt = filledAt.UTC().Format(time.RFC3339)
	if err := r.saveState(); err != nil {
		logger.Warn("currency-strength: failed to persist recovered filled exit", "err", err)
	}
	logger.Warn("currency-strength: recovered strategy-owned filled exit",
		"symbol", r.state.Symbol, "order_id", orderID,
		"trade_id", discoveredRef, "status", status, "filled_qty", filledQty)
	return true, nil
}

func (r *currencyStrengthPaperRunner) adoptWorkingExit(
	ctx context.Context,
	exitSide execution.OrderSide,
) (bool, error) {
	if r.state.ExitRetry && r.state.ExitTradeID == "" {
		return false, nil
	}
	lister, ok := r.broker.(execution.WorkingOrdersLister)
	if !ok {
		return false, nil
	}
	orders, err := lister.GetWorkingOrders(ctx)
	if err != nil {
		return false, fmt.Errorf("currency-strength list working exits: %w", err)
	}
	wantTicker := normalizeCurrencyStrengthTicker(r.state.Symbol)
	wantTradeID := r.exitTradeID()
	baseTradeID := r.baseExitTradeID()
	for _, order := range orders {
		if normalizeCurrencyStrengthTicker(order.Ticker) != wantTicker ||
			!strings.EqualFold(order.Side, string(exitSide)) ||
			!currencyStrengthExitReferenceMatches(order.TradeID, wantTradeID, baseTradeID, r.state.ExitTradeID != "") {
			continue
		}
		r.state.ExitOrderID = order.OrderID
		r.state.ExitStatus = "pending"
		r.state.ExitFillTimeQty = 0
		if r.state.ExitRequestedQty <= 0 {
			r.state.ExitRequestedQty = r.state.Quantity
		}
		r.state.ExitedAt = r.now().UTC().Format(time.RFC3339)
		if err := r.saveState(); err != nil {
			logger.Warn("currency-strength: failed to persist adopted working exit", "err", err)
		}
		logger.Warn("currency-strength: adopted broker working exit during recovery",
			"symbol", r.state.Symbol, "order_id", order.OrderID)
		return true, nil
	}
	return false, nil
}

func (r *currencyStrengthPaperRunner) exitTradeID() string {
	if r.state.ExitTradeID != "" {
		return r.state.ExitTradeID
	}
	return r.baseExitTradeID()
}

func (r *currencyStrengthPaperRunner) baseExitTradeID() string {
	return fmt.Sprintf("currency-strength-exit-%s-%s",
		r.state.Date, strings.ReplaceAll(r.state.Symbol, "/", ""))
}

func currencyStrengthExitReferenceMatches(orderRef, wantRef, baseRef string, exact bool) bool {
	if exact {
		return orderRef == wantRef
	}
	return orderRef == baseRef || strings.HasPrefix(orderRef, baseRef+"-")
}

func (r *currencyStrengthPaperRunner) reserveExitTradeID() (string, error) {
	if r.state.ExitTradeID != "" {
		return r.state.ExitTradeID, nil
	}
	r.state.ExitAttempt++
	r.state.ExitTradeID = fmt.Sprintf("%s-%d-%d",
		r.exitTradeID(), r.now().UTC().UnixNano(), r.state.ExitAttempt)
	if err := r.saveState(); err != nil {
		// The strategy-specific prefix remains discoverable through IBKR
		// working orders and journal-restored state. Filesystem durability must
		// not block exposure reduction.
		logger.Warn("currency-strength: exit ownership sidecar unavailable; continuing with broker-discoverable reference", "err", err)
	}
	return r.state.ExitTradeID, nil
}

// cancelAndSettleEntry requests cancellation and observes the order until the
// broker reports a terminal state. A successful cancellation HTTP response is
// only an acknowledgement at IBKR, so callers must defer flatten sizing until
// this returns settled=true on a later reconciliation tick.
func (r *currencyStrengthPaperRunner) cancelAndSettleEntry(ctx context.Context) (bool, error) {
	if err := r.broker.CancelOrder(ctx, r.state.EntryOrderID); err != nil &&
		!errors.Is(err, execution.ErrOrderNotFound) {
		return false, fmt.Errorf("cancel currency-strength entry remainder: %w", err)
	}
	result, err := r.broker.GetOrderStatus(ctx, r.state.EntryOrderID)
	if err != nil {
		if errors.Is(err, execution.ErrOrderNotFound) {
			// A purged order is no longer working. Broker position truth,
			// refreshed by the caller, determines whether anything filled.
			return true, nil
		}
		return false, fmt.Errorf("confirm currency-strength entry cancellation: %w", err)
	}
	r.state.EntryStatus = normalizeCurrencyStrengthOrderStatus(result.Status)
	if result.FilledPrice > 0 {
		r.state.EntryFillPrice = result.FilledPrice
	}
	if result.FilledQty > 0 {
		r.state.EntryFilledQty = result.FilledQty
	}
	r.entryRecorded = false
	if err := r.saveState(); err != nil {
		return false, fmt.Errorf("persist currency-strength entry cancellation status: %w", err)
	}
	switch r.state.EntryStatus {
	case "filled", "cancelled", "rejected":
		return true, nil
	default:
		return false, nil
	}
}

func normalizeCurrencyStrengthTicker(raw string) string {
	replacer := strings.NewReplacer("/", "", ".", "", "-", "", "_", "", " ", "")
	return strings.ToUpper(replacer.Replace(strings.TrimSpace(raw)))
}

func normalizeCurrencyStrengthOrderStatus(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "filled":
		return "filled"
	case "partial", "partially_filled":
		return "partial"
	case "cancelled", "canceled":
		return "cancelled"
	case "rejected":
		return "rejected"
	case "pending":
		return "pending"
	default:
		return execution.NormalizeOrderStatus(raw)
	}
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

func (r *currencyStrengthPaperRunner) reconcileActiveEntry(ctx context.Context) error {
	if r.state == nil || r.state.ExitOrderID != "" || r.state.EntryOrderID == "" {
		return nil
	}

	status := normalizeCurrencyStrengthOrderStatus(r.state.EntryStatus)
	if status == "filled" {
		// Older sidecars and immediate IBKR acknowledgements may lack fill
		// economics. Broker position truth can safely enrich them.
		if r.state.EntryFillPrice <= 0 || r.state.EntryFilledQty <= 0 {
			pos, err := r.positionFor(ctx, r.state.Symbol)
			if err != nil {
				return err
			}
			if pos != nil {
				r.state.EntryFillPrice = pos.AvgCostBasis
				r.state.EntryFilledQty = math.Abs(pos.Quantity)
				r.state.Quantity = math.Abs(pos.Quantity)
				r.entryRecorded = false
				if err := r.saveState(); err != nil {
					return err
				}
			}
		}
		return nil
	}

	result, err := r.broker.GetOrderStatus(ctx, r.state.EntryOrderID)
	if err != nil {
		pos, posErr := r.positionFor(ctx, r.state.Symbol)
		if posErr != nil {
			return fmt.Errorf("entry order status: %w (position check: %v)", err, posErr)
		}
		if pos != nil {
			r.state.EntryStatus = "filled"
			r.state.EntryFillPrice = pos.AvgCostBasis
			r.state.EntryFilledQty = math.Abs(pos.Quantity)
			r.state.Quantity = math.Abs(pos.Quantity)
			r.entryRecorded = false
			return r.saveState()
		}
		if !errors.Is(err, execution.ErrOrderNotFound) {
			return fmt.Errorf("entry order status: %w", err)
		}
		result = execution.OrderResult{
			BrokerOrderID: r.state.EntryOrderID,
			Status:        "rejected",
		}
	}

	r.state.EntryStatus = normalizeCurrencyStrengthOrderStatus(result.Status)
	if result.FilledPrice > 0 {
		r.state.EntryFillPrice = result.FilledPrice
	}
	if result.FilledQty > 0 {
		r.state.EntryFilledQty = result.FilledQty
	}
	r.entryRecorded = false
	if err := r.saveState(); err != nil {
		return err
	}

	switch r.state.EntryStatus {
	case "cancelled", "rejected":
		r.decisionDoneFor = r.state.Date
		pos, posErr := r.positionFor(ctx, r.state.Symbol)
		if posErr != nil {
			return posErr
		}
		if pos != nil {
			// A cancelled/rejected order can still contain an earlier partial
			// fill. Preserve that broker-truth exposure so the scheduled exit
			// remains responsible for flattening it.
			r.state.EntryStatus = "filled"
			r.state.EntryFillPrice = pos.AvgCostBasis
			r.state.EntryFilledQty = math.Abs(pos.Quantity)
			r.state.Quantity = math.Abs(pos.Quantity)
			if err := r.saveState(); err != nil {
				return err
			}
			if err := r.syncActiveEntryJournal(ctx); err != nil {
				logger.Warn("currency-strength: partially-filled cancelled entry history sync failed; retaining exposure", "err", err)
			}
			return nil
		}
		if r.state.EntryFilledQty > 0 {
			// IBKR can expose terminal order fills before the corresponding
			// position appears. Keep recovery state until broker position truth
			// catches up instead of orphaning delayed exposure.
			if err := r.saveState(); err != nil {
				return err
			}
			if err := r.syncActiveEntryJournal(ctx); err != nil {
				logger.Warn("currency-strength: terminal entry fill history sync failed; retaining recovery state", "err", err)
			}
			return nil
		}
		if err := r.syncActiveEntryJournal(ctx); err != nil {
			return err
		}
		return r.clearState()
	default:
		return nil
	}
}

func (r *currencyStrengthPaperRunner) restoreJournalState(
	ctx context.Context,
	sessionDate time.Time,
) error {
	if r.journal == nil {
		return nil
	}
	run, err := r.journal.Get(ctx, sessionDate)
	if err != nil || run == nil {
		if err != nil {
			return err
		}
	} else {
		r.restoreCurrencyStrengthRun(*run)
	}

	if r.state == nil {
		active, activeErr := r.journal.GetActive(ctx, sessionDate)
		if activeErr != nil {
			return activeErr
		}
		if active != nil {
			r.restoreCurrencyStrengthRun(*active)
		}
	}
	return nil
}

func (r *currencyStrengthPaperRunner) restoreCurrencyStrengthRun(run store.CurrencyStrengthRun) {
	switch run.Status {
	case "scheduled", "evaluating", "failed":
		return
	case "no_signal", "entry_submitted", "entered", "entry_failed",
		"exit_submitted", "exit_failed", "exited":
		runDate := run.SessionDate.Format("2006-01-02")
		if runDate > r.decisionDoneFor {
			r.decisionDoneFor = runDate
		}
	}

	if r.state == nil {
		switch run.Status {
		case "entry_submitted", "entered", "exit_submitted", "exit_failed":
			r.state = currencyStrengthStateFromRun(run)
			r.entryRecorded = true
		case "entry_failed":
			if run.EntryFilledQty > 0 {
				r.state = currencyStrengthStateFromRun(run)
				r.entryRecorded = true
			}
		}
		return
	}
	if r.state.Date == run.SessionDate.Format("2006-01-02") {
		journalCoversEntry := currencyStrengthJournalCoversEntry(r.state, run)
		mergeCurrencyStrengthRunIntoState(r.state, run)
		r.entryRecorded = currencyStrengthRunHasEntry(run.Status) && journalCoversEntry
	}
}

func (r *currencyStrengthPaperRunner) mergeActiveStateFromJournal(ctx context.Context) error {
	if r.journal == nil || r.state == nil {
		return nil
	}
	sessionDate, err := time.Parse("2006-01-02", r.state.Date)
	if err != nil {
		return err
	}
	run, err := r.journal.Get(ctx, sessionDate)
	if err != nil || run == nil {
		return err
	}
	journalCoversEntry := currencyStrengthJournalCoversEntry(r.state, *run)
	mergeCurrencyStrengthRunIntoState(r.state, *run)
	r.entryRecorded = currencyStrengthRunHasEntry(run.Status) && journalCoversEntry
	return nil
}

func currencyStrengthRunHasEntry(status string) bool {
	switch status {
	case "entry_submitted", "entered", "entry_failed",
		"exit_submitted", "exit_failed", "exited":
		return true
	default:
		return false
	}
}

func currencyStrengthStateFromRun(run store.CurrencyStrengthRun) *currencyStrengthRunState {
	state := &currencyStrengthRunState{
		AccountID:      run.AccountID,
		Date:           run.SessionDate.Format("2006-01-02"),
		Symbol:         run.SignalSymbol,
		Side:           run.SignalSide,
		Quantity:       float64(run.Units),
		EntryOrderID:   run.EntryOrderID,
		EntryStatus:    run.EntryStatus,
		EntryFillPrice: run.EntryFillPrice,
		EntryFilledQty: run.EntryFilledQty,
		Strongest:      run.StrongestCurrency,
		Weakest:        run.WeakestCurrency,
		StrengthGap:    run.StrengthGap,
		ExitOrderID:    run.ExitOrderID,
		ExitStatus:     run.ExitStatus,
		ExitFillPrice:  run.ExitFillPrice,
		ExitFilledQty:  run.ExitFilledQty,
	}
	if run.EnteredAt != nil {
		state.EnteredAt = run.EnteredAt.UTC().Format(time.RFC3339)
	}
	if run.ExitedAt != nil {
		state.ExitedAt = run.ExitedAt.UTC().Format(time.RFC3339)
	}
	if state.EntryFilledQty > 0 {
		state.Quantity = state.EntryFilledQty
	}
	if strings.EqualFold(run.ExitStatus, "residual") {
		// The recorded order is terminal; its fill left broker exposure.
		// Restore the strategy's obligation to submit another flatten, not the
		// old order as though it were still working.
		state.ExitOrderID = ""
		state.ExitStatus = ""
		state.ExitFillPrice = 0
		state.ExitFilledQty = 0
		state.ExitedAt = ""
		state.ExitRetry = true
	}
	return state
}

func mergeCurrencyStrengthRunIntoState(
	state *currencyStrengthRunState,
	run store.CurrencyStrengthRun,
) {
	if state == nil {
		return
	}
	if state.EntryOrderID == "" {
		state.EntryOrderID = run.EntryOrderID
	}
	localEntryRank := currencyStrengthEntryStatusRank(state.EntryStatus)
	remoteEntryRank := currencyStrengthEntryStatusRank(run.EntryStatus)
	if shouldMergeCurrencyStrengthEntryStatus(state.EntryStatus, run.EntryStatus) {
		state.EntryStatus = run.EntryStatus
	}
	if run.EntryFillPrice > 0 &&
		(state.EntryFillPrice <= 0 ||
			(run.EntryFilledQty >= state.EntryFilledQty &&
				(remoteEntryRank > localEntryRank ||
					run.EntryFilledQty > state.EntryFilledQty))) {
		state.EntryFillPrice = run.EntryFillPrice
	}
	if run.EntryFilledQty > state.EntryFilledQty {
		state.EntryFilledQty = run.EntryFilledQty
		state.Quantity = run.EntryFilledQty
	}
	if run.ExitOrderID != "" &&
		(run.Status == "exit_submitted" || run.Status == "exited") &&
		!state.ExitRetry {
		state.ExitOrderID = run.ExitOrderID
		state.ExitStatus = run.ExitStatus
		state.ExitFillPrice = run.ExitFillPrice
		state.ExitFilledQty = run.ExitFilledQty
		if run.ExitedAt != nil {
			state.ExitedAt = run.ExitedAt.UTC().Format(time.RFC3339)
		}
	}
}

func shouldMergeCurrencyStrengthEntryStatus(local, remote string) bool {
	if remote == "" {
		return false
	}
	return currencyStrengthEntryStatusRank(remote) >= currencyStrengthEntryStatusRank(local)
}

func currencyStrengthEntryStatusRank(status string) int {
	switch normalizeCurrencyStrengthOrderStatus(status) {
	case "pending":
		return 1
	case "partial":
		return 2
	case "cancelled", "rejected":
		return 3
	case "filled":
		return 4
	default:
		return 0
	}
}

func currencyStrengthJournalCoversEntry(
	state *currencyStrengthRunState,
	run store.CurrencyStrengthRun,
) bool {
	if state == nil || !currencyStrengthRunHasEntry(run.Status) {
		return false
	}
	if currencyStrengthEntryStatusRank(run.EntryStatus) <
		currencyStrengthEntryStatusRank(state.EntryStatus) {
		return false
	}
	if run.EntryFilledQty < state.EntryFilledQty {
		return false
	}
	if state.EntryFillPrice > 0 {
		if run.EntryFillPrice <= 0 ||
			math.Abs(run.EntryFillPrice-state.EntryFillPrice) > 1e-9 {
			return false
		}
	}
	return true
}

func (r *currencyStrengthPaperRunner) finalizeExitedState(ctx context.Context) error {
	if r.state == nil || r.state.ExitOrderID == "" {
		return nil
	}

	status := normalizeCurrencyStrengthOrderStatus(r.state.ExitStatus)
	if r.state.ExitOrderID != "broker-flat" && status != "filled" {
		result, err := r.broker.GetOrderStatus(ctx, r.state.ExitOrderID)
		if err != nil {
			pos, posErr := r.positionFor(ctx, r.state.Symbol)
			if posErr != nil {
				return fmt.Errorf("exit order status: %w (position check: %v)", err, posErr)
			}
			if errors.Is(err, execution.ErrOrderNotFound) {
				if pos == nil {
					status = "filled"
				} else {
					status = "cancelled"
				}
				r.state.ExitStatus = status
			} else {
				if journalErr := r.recordExitJournal(ctx); journalErr != nil {
					logger.Warn("currency-strength: pending exit history sync failed; will retry", "err", journalErr)
				}
				return nil
			}
		} else {
			status = normalizeCurrencyStrengthOrderStatus(result.Status)
			r.state.ExitStatus = status
			if result.FilledPrice > 0 {
				r.state.ExitFillPrice = result.FilledPrice
			}
			if result.FilledQty > 0 {
				r.state.ExitFilledQty = result.FilledQty
			}
		}
		if err := r.saveState(); err != nil {
			logger.Warn("currency-strength: failed to persist reconciled exit state", "err", err)
		}
	}

	pos, err := r.positionFor(ctx, r.state.Symbol)
	if err != nil {
		return err
	}
	if r.state.ExitFilledQty > r.state.ExitFillTimeQty+1e-6 {
		ready, readyErr := r.ensureExitFillTime(ctx)
		if readyErr != nil {
			logger.Warn("currency-strength: exit fill timestamp recovery failed; will retry",
				"order_id", r.state.ExitOrderID, "err", readyErr)
			return nil
		}
		if !ready {
			return nil
		}
	}
	switch status {
	case "pending", "partial":
		if pos == nil {
			settled, settleErr := r.cancelFlatWorkingExit(ctx)
			if settleErr != nil {
				return settleErr
			}
			if !settled {
				return nil
			}
			status = normalizeCurrencyStrengthOrderStatus(r.state.ExitStatus)
		} else {
			if err := r.recordExitJournal(ctx); err != nil {
				logger.Warn("currency-strength: pending exit history sync failed; will retry", "err", err)
			}
			return nil
		}
	case "cancelled", "rejected":
		if pos == nil {
			r.state.ExitStatus = "filled"
			status = "filled"
		} else {
			r.capturePendingExitAudit()
			if err := r.syncPendingExitAudit(ctx); err != nil {
				logger.Warn("currency-strength: failed exit history sync; will retry", "err", err)
			}
			r.state.ExitOrderID = ""
			r.state.ExitTradeID = ""
			r.state.ExitStatus = ""
			r.state.ExitFillPrice = 0
			r.state.ExitFilledQty = 0
			r.state.ExitRequestedQty = 0
			r.state.ExitFillObserved = ""
			r.state.ExitFillTimeQty = 0
			r.state.ExitedAt = ""
			r.state.ExitRetry = true
			if err := r.saveState(); err != nil {
				return fmt.Errorf("currency-strength persist exit retry state: %w", err)
			}
			return nil
		}
	case "filled":
		if pos != nil {
			provenPartial := r.state.ExitRequestedQty > 0 &&
				r.state.ExitFilledQty+1e-6 < r.state.ExitRequestedQty
			if !provenPartial {
				now := r.currencyStrengthNow().UTC()
				observedAt, _ := time.Parse(time.RFC3339, r.state.ExitFillObserved)
				if observedAt.IsZero() {
					r.state.ExitFillObserved = now.Format(time.RFC3339)
					if err := r.saveState(); err != nil {
						return fmt.Errorf("persist currency-strength fill propagation guard: %w", err)
					}
					logger.Info("currency-strength: filled exit awaiting portfolio propagation",
						"order_id", r.state.ExitOrderID, "position_qty", pos.Quantity)
					return nil
				}
				if now.Sub(observedAt) < currencyStrengthPortfolioPropagationGrace {
					return nil
				}
			}
			// The order is terminal but residual exposure remains. Journal the
			// partial outcome, then release this order ID so the next tick
			// submits a new flatten for broker-truth remaining quantity.
			r.state.ExitStatus = "residual"
			r.capturePendingExitAudit()
			if err := r.syncPendingExitAudit(ctx); err != nil {
				logger.Warn("currency-strength: residual exit history sync failed; will retry", "err", err)
			}
			r.state.ExitOrderID = ""
			r.state.ExitTradeID = ""
			r.state.ExitStatus = ""
			r.state.ExitFillPrice = 0
			r.state.ExitFilledQty = 0
			r.state.ExitRequestedQty = 0
			r.state.ExitFillObserved = ""
			r.state.ExitFillTimeQty = 0
			r.state.ExitedAt = ""
			r.state.ExitRetry = true
			if err := r.saveState(); err != nil {
				return fmt.Errorf("currency-strength persist residual exit retry: %w", err)
			}
			return nil
		}
	}

	if status != "filled" {
		return nil
	}
	ready, readyErr := r.ensureExitFillTime(ctx)
	if readyErr != nil {
		logger.Warn("currency-strength: exit fill timestamp recovery failed; will retry",
			"order_id", r.state.ExitOrderID, "err", readyErr)
		return nil
	}
	if !ready {
		return nil
	}
	if err := r.syncPendingExitAudit(ctx); err != nil {
		logger.Warn("currency-strength: prior exit audit sync failed; retaining terminal state", "err", err)
		return nil
	}
	if err := r.recordExitJournal(ctx); err != nil {
		logger.Warn("currency-strength: terminal exit history sync failed; will retry", "err", err)
		return nil
	}
	if !r.state.ExitConfirmed {
		r.notify(ctx, buildCurrencyStrengthFlattenedMessage(
			r.state.Symbol, r.state.ExitOrderID, r.state.ExitFilledQty, r.state.ExitFillPrice,
		))
		r.state.ExitConfirmed = true
		if err := r.saveState(); err != nil {
			logger.Warn("currency-strength: failed to persist terminal notification state", "err", err)
			return nil
		}
	}
	return r.clearState()
}

func (r *currencyStrengthPaperRunner) ensureExitFillTime(ctx context.Context) (bool, error) {
	if r.state == nil || r.state.ExitFilledQty <= 0 ||
		r.state.ExitOrderID == "broker-flat" ||
		r.state.ExitFillTimeQty+1e-6 >= r.state.ExitFilledQty {
		return true, nil
	}
	records, err := r.currencyStrengthTradeRecords(ctx)
	if err != nil {
		return false, err
	}
	var filledAt time.Time
	var filledQty float64
	for _, record := range records {
		if record.OrderID == r.state.ExitOrderID {
			filledQty += record.Qty
			if record.FilledAt.After(filledAt) {
				filledAt = record.FilledAt
			}
		}
	}
	if filledAt.IsZero() || filledQty+1e-6 < r.state.ExitFilledQty {
		logger.Info("currency-strength: filled exit awaiting execution timestamp",
			"order_id", r.state.ExitOrderID,
			"status_filled_qty", r.state.ExitFilledQty,
			"timestamped_filled_qty", filledQty)
		return false, nil
	}
	r.state.ExitedAt = filledAt.UTC().Format(time.RFC3339)
	r.state.ExitFillTimeQty = filledQty
	if err := r.saveState(); err != nil {
		// Keep broker reconciliation live. The timestamp remains in memory and
		// the journal write provides an independent durable destination.
		logger.Warn("currency-strength: failed to persist recovered exit fill timestamp",
			"order_id", r.state.ExitOrderID, "err", err)
	}
	return true, nil
}

func (r *currencyStrengthPaperRunner) cancelFlatWorkingExit(ctx context.Context) (bool, error) {
	if err := r.broker.CancelOrder(ctx, r.state.ExitOrderID); err != nil &&
		!errors.Is(err, execution.ErrOrderNotFound) {
		return false, fmt.Errorf("cancel flat-account currency-strength exit remainder: %w", err)
	}
	result, err := r.broker.GetOrderStatus(ctx, r.state.ExitOrderID)
	if err != nil {
		if !errors.Is(err, execution.ErrOrderNotFound) {
			return false, fmt.Errorf("confirm flat-account currency-strength exit cancellation: %w", err)
		}
		r.state.ExitStatus = "filled"
		return true, nil
	}
	status := normalizeCurrencyStrengthOrderStatus(result.Status)
	r.state.ExitStatus = status
	if result.FilledPrice > 0 {
		r.state.ExitFillPrice = result.FilledPrice
	}
	if result.FilledQty > 0 {
		r.state.ExitFilledQty = result.FilledQty
	}
	switch status {
	case "pending", "partial":
		if err := r.saveState(); err != nil {
			logger.Warn("currency-strength: failed to persist working flat-account exit", "err", err)
		}
		return false, nil
	case "filled":
		return true, nil
	case "cancelled", "rejected":
		r.capturePendingExitAudit()
		if err := r.syncPendingExitAudit(ctx); err != nil {
			logger.Warn("currency-strength: cancelled flat-account exit audit sync failed; will retry", "err", err)
		}
		r.state.ExitOrderID = "broker-flat"
		r.state.ExitTradeID = ""
		r.state.ExitStatus = "filled"
		r.state.ExitFillPrice = 0
		r.state.ExitFilledQty = 0
		r.state.ExitRequestedQty = 0
		r.state.ExitFillObserved = ""
		r.state.ExitFillTimeQty = 0
		r.state.ExitedAt = r.now().UTC().Format(time.RFC3339)
		if err := r.saveState(); err != nil {
			logger.Warn("currency-strength: failed to persist broker-flat exit recovery", "err", err)
		}
		return true, nil
	default:
		return false, nil
	}
}

func (r *currencyStrengthPaperRunner) recordExitJournal(ctx context.Context) error {
	if r.journal == nil {
		return nil
	}
	audit := currencyStrengthExitAudit{
		OrderID:   r.state.ExitOrderID,
		Status:    r.state.ExitStatus,
		FillPrice: r.state.ExitFillPrice,
		FilledQty: r.state.ExitFilledQty,
		ExitedAt:  r.state.ExitedAt,
	}
	realizedPnL, err := r.realizedPnLUSD(ctx, audit)
	if err != nil {
		return err
	}
	return r.writeExitJournal(ctx, audit, realizedPnL)
}

func (r *currencyStrengthPaperRunner) capturePendingExitAudit() {
	r.state.PendingExitAudits = append(r.state.PendingExitAudits, currencyStrengthExitAudit{
		OrderID:   r.state.ExitOrderID,
		Status:    r.state.ExitStatus,
		FillPrice: r.state.ExitFillPrice,
		FilledQty: r.state.ExitFilledQty,
		ExitedAt:  r.state.ExitedAt,
	})
}

func (r *currencyStrengthPaperRunner) syncPendingExitAudit(ctx context.Context) error {
	if r.state == nil || len(r.state.PendingExitAudits) == 0 {
		return nil
	}
	for len(r.state.PendingExitAudits) > 0 {
		audit := r.state.PendingExitAudits[0]
		realizedPnL, err := r.realizedPnLUSD(ctx, audit)
		if err != nil {
			return err
		}
		if err := r.writeExitJournal(ctx, audit, realizedPnL); err != nil {
			return err
		}
		r.state.PendingExitAudits = r.state.PendingExitAudits[1:]
		if err := r.saveState(); err != nil {
			r.state.PendingExitAudits = append(
				[]currencyStrengthExitAudit{audit},
				r.state.PendingExitAudits...,
			)
			return fmt.Errorf("persist currency-strength exit audit dequeue: %w", err)
		}
	}
	return nil
}

func (r *currencyStrengthPaperRunner) realizedPnLUSD(
	ctx context.Context,
	audit currencyStrengthExitAudit,
) (*float64, error) {
	if normalizeCurrencyStrengthOrderStatus(r.state.EntryStatus) != "filled" ||
		audit.FillPrice <= 0 || audit.FilledQty <= 0 || r.state.EntryFillPrice <= 0 {
		return nil, nil
	}
	parts := strings.Split(strings.ToUpper(r.state.Symbol), "/")
	if len(parts) != 2 || len(parts[0]) != 3 || len(parts[1]) != 3 {
		return nil, fmt.Errorf("currency-strength convert P&L for invalid pair %q", r.state.Symbol)
	}
	pnl := (audit.FillPrice - r.state.EntryFillPrice) * audit.FilledQty
	if r.state.Side == "sell" {
		pnl = -pnl
	}
	base, quote := parts[0], parts[1]
	switch {
	case quote == "USD":
		return &pnl, nil
	case base == "USD":
		pnl /= audit.FillPrice
		return &pnl, nil
	}

	exitedAt, parseErr := time.Parse(time.RFC3339, audit.ExitedAt)
	if parseErr != nil {
		return nil, fmt.Errorf(
			"currency-strength convert %s P&L: invalid exit timestamp %q: %w",
			quote, audit.ExitedAt, parseErr,
		)
	}
	rate, err := r.currencyConversionRateAt(ctx, quote+"/USD", exitedAt)
	if err == nil {
		pnl *= rate
		return &pnl, nil
	}
	inverse, inverseErr := r.currencyConversionRateAt(ctx, "USD/"+quote, exitedAt)
	if inverseErr != nil {
		return nil, fmt.Errorf(
			"currency-strength convert %s P&L to USD: direct: %v; inverse: %w",
			quote, err, inverseErr,
		)
	}
	pnl /= inverse
	return &pnl, nil
}

func (r *currencyStrengthPaperRunner) currencyConversionRateAt(
	ctx context.Context,
	symbol string,
	at time.Time,
) (float64, error) {
	if at.IsZero() {
		return 0, fmt.Errorf("%s conversion requires an exit timestamp", symbol)
	}
	candles, err := r.market.FetchCandles(
		ctx,
		symbol,
		data.Timeframe1Min,
		at.Add(-15*time.Minute),
		at.Add(time.Minute),
	)
	if err != nil {
		return 0, err
	}
	var rate float64
	var latest time.Time
	for _, candle := range candles {
		if candle.Close > 0 && !candle.Timestamp.After(at) &&
			(latest.IsZero() || candle.Timestamp.After(latest)) {
			rate = candle.Close
			latest = candle.Timestamp
		}
	}
	if rate <= 0 {
		return 0, fmt.Errorf("%s has no positive conversion candle at or before %s",
			symbol, at.UTC().Format(time.RFC3339))
	}
	return rate, nil
}

func (r *currencyStrengthPaperRunner) writeExitJournal(
	ctx context.Context,
	audit currencyStrengthExitAudit,
	realizedPnL *float64,
) error {
	if r.journal == nil {
		return nil
	}
	if !r.entryRecorded {
		if err := r.syncActiveEntryJournal(ctx); err != nil {
			return fmt.Errorf("entry history sync before exit: %w", err)
		}
	}
	sessionDate, err := time.Parse("2006-01-02", r.state.Date)
	if err != nil {
		return fmt.Errorf("parse state date %q: %w", r.state.Date, err)
	}
	exitedAt, _ := time.Parse(time.RFC3339, audit.ExitedAt)
	return r.journal.RecordExit(ctx, store.CurrencyStrengthRun{
		SessionDate:   sessionDate,
		ExitOrderID:   audit.OrderID,
		ExitStatus:    audit.Status,
		ExitFillPrice: audit.FillPrice,
		ExitFilledQty: audit.FilledQty,
		ExitedAt:      optionalTime(exitedAt),
		RealizedPnL:   realizedPnL,
	})
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
	loadedPath := r.statePath
	legacy := false
	if os.IsNotExist(err) && r.legacyStatePath != "" {
		claimPath := r.statePath + ".legacy-claim"
		b, err = os.ReadFile(claimPath)
		if os.IsNotExist(err) {
			// Rename before reading: only one account-specific runner can claim
			// an untagged legacy obligation during a concurrent first upgrade.
			if renameErr := os.Rename(r.legacyStatePath, claimPath); renameErr != nil {
				if os.IsNotExist(renameErr) {
					return nil
				}
				return fmt.Errorf("currency-strength claim legacy state: %w", renameErr)
			}
			b, err = os.ReadFile(claimPath)
		}
		loadedPath = claimPath
		legacy = err == nil
		if legacy {
			r.claimedLegacyPath = claimPath
		}
	}
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("currency-strength read state %s: %w", loadedPath, err)
	}
	var state currencyStrengthRunState
	if err := json.Unmarshal(b, &state); err != nil {
		return fmt.Errorf("currency-strength decode state %s: %w", loadedPath, err)
	}
	stateAccountID := normalizeCurrencyStrengthAccountID(state.AccountID)
	if r.accountID != "" && stateAccountID != "" && stateAccountID != r.accountID {
		if legacy {
			if restoreErr := r.restoreLegacyClaim(); restoreErr != nil {
				return fmt.Errorf(
					"currency-strength state belongs to account %q, current account is %q (restore claim: %v)",
					state.AccountID, r.accountID, restoreErr,
				)
			}
		}
		return fmt.Errorf(
			"currency-strength state belongs to account %q, current account is %q",
			state.AccountID, r.accountID,
		)
	}
	if r.accountID != "" && stateAccountID == "" && !legacy {
		return fmt.Errorf(
			"currency-strength account-scoped state %s has no account id",
			loadedPath,
		)
	}
	if legacy && r.accountID != "" {
		stateAccountID = r.accountID
	}
	state.AccountID = stateAccountID
	if state.Symbol != "" && state.Quantity > 0 {
		r.state = &state
	}
	if legacy && r.state != nil {
		if err := r.migrateLegacyState(); err != nil {
			// Migration durability is secondary to managing the obligation
			// already recovered into memory. Retry migration on the next start.
			logger.Warn("currency-strength: legacy sidecar migration deferred; continuing recovery",
				"path", r.legacyStatePath, "err", err)
		}
	}
	return nil
}

func (r *currencyStrengthPaperRunner) restoreLegacyClaim() error {
	if r.claimedLegacyPath == "" {
		return nil
	}
	if _, err := os.Stat(r.legacyStatePath); err == nil {
		return fmt.Errorf("legacy source %s already exists", r.legacyStatePath)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(r.claimedLegacyPath, r.legacyStatePath); err != nil {
		return err
	}
	r.claimedLegacyPath = ""
	return nil
}

func (r *currencyStrengthPaperRunner) migrateLegacyState() error {
	// Tag the source first. If the process stops before the account-scoped copy
	// is durable, another configured account cannot subsequently claim it.
	b, err := json.MarshalIndent(r.state, "", "  ")
	if err != nil {
		return fmt.Errorf("currency-strength encode legacy state migration: %w", err)
	}
	if err := writeCurrencyStrengthStateAtomic(r.claimedLegacyPath, b); err != nil {
		return fmt.Errorf("currency-strength tag legacy state migration: %w", err)
	}
	if err := r.saveState(); err != nil {
		return fmt.Errorf("currency-strength persist account-scoped state migration: %w", err)
	}
	if err := os.Remove(r.claimedLegacyPath); err != nil && !os.IsNotExist(err) {
		// The tagged legacy copy is safe to leave behind: a different account
		// will reject it, while this account now prefers its scoped sidecar.
		logger.Warn("currency-strength: migrated legacy sidecar but could not remove tagged source",
			"path", r.claimedLegacyPath, "err", err)
	}
	logger.Info("currency-strength: migrated legacy recovery sidecar",
		"from", r.claimedLegacyPath, "to", r.statePath, "account_id", r.accountID)
	r.claimedLegacyPath = ""
	return nil
}

func (r *currencyStrengthPaperRunner) saveState() error {
	if r.state == nil {
		return r.clearState()
	}
	if err := os.MkdirAll(filepath.Dir(r.statePath), 0750); err != nil {
		return fmt.Errorf("currency-strength create state dir: %w", err)
	}
	if r.accountID != "" {
		r.state.AccountID = r.accountID
	}
	b, err := json.MarshalIndent(r.state, "", "  ")
	if err != nil {
		return fmt.Errorf("currency-strength encode state: %w", err)
	}
	return writeCurrencyStrengthStateAtomic(r.statePath, b)
}

func (r *currencyStrengthPaperRunner) clearState() error {
	r.state = nil
	if err := os.Remove(r.statePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("currency-strength remove state: %w", err)
	}
	return nil
}

func writeCurrencyStrengthStateAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("currency-strength create temporary state: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()
	if err := tmp.Chmod(0600); err != nil {
		return fmt.Errorf("currency-strength chmod temporary state: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("currency-strength write temporary state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("currency-strength sync temporary state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("currency-strength close temporary state: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("currency-strength replace state: %w", err)
	}
	if dirHandle, err := os.Open(dir); err == nil {
		if syncErr := dirHandle.Sync(); syncErr != nil {
			_ = dirHandle.Close()
			return fmt.Errorf("currency-strength sync state directory: %w", syncErr)
		}
		_ = dirHandle.Close()
	}
	return nil
}

func defaultCurrencyStrengthStatePath(accountID string) string {
	accountKey := strings.ToLower(normalizeCurrencyStrengthAccountID(accountID))
	accountKey = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, accountKey)
	if accountKey == "" {
		accountKey = "unknown"
	}
	filename := "currency_strength_state_" + accountKey + ".json"
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "tradeengine-"+filename)
	}
	return filepath.Join(home, ".tradeengine", filename)
}

func defaultLegacyCurrencyStrengthStatePath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "tradeengine-currency-strength-state.json")
	}
	return filepath.Join(home, ".tradeengine", "currency_strength_state.json")
}

func isIBKRPaperAccountID(accountID string) bool {
	return strings.HasPrefix(normalizeCurrencyStrengthAccountID(accountID), "DU")
}

func normalizeCurrencyStrengthAccountID(accountID string) string {
	return strings.ToUpper(strings.TrimSpace(accountID))
}

func (r *currencyStrengthPaperRunner) currencyStrengthNow() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
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
