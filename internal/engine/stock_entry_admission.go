package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/portfolio"
	"github.com/romeo-folie/trade-engine/internal/risk"
	"github.com/romeo-folie/trade-engine/internal/store"
)

var errStockEntryIntentComplete = errors.New("stock entry intent is terminal with a fill")

const (
	stockAdmissionMaxAge          = 15 * time.Second
	stockAdmissionFutureTolerance = 10 * time.Second
	stockQuantityScale            = 1_000_000.0
)

type stockEntryAdmission struct {
	BrokerAuthoritative bool
	Account             execution.BrokerAccount
	Asset               execution.StockAsset
	Ask                 float64
	QuoteAt             time.Time
	Quantity            float64
	RemainingCapacity   float64
	PeakEquity          float64
	DailyBaseline       float64
	Reservations        []store.ActiveStockEntryReservation
	BrokerHeldReplay    bool
	WorkingOrders       []execution.WorkingOrder
	BrokerPositions     []execution.BrokerPosition
}

func (e *Engine) beginStockEntryAdmission(
	ctx context.Context,
	intentKey, ticker string,
	sizePct float64,
) (stockEntryAdmission, *execution.OrderRequest, string, func(), error) {
	e.stockEntryAdmissionMu.Lock()
	release := e.stockEntryAdmissionMu.Unlock
	effectiveKey := intentKey
	if store.IsConnected() {
		_, resolvedKey, err := store.ResolveStockOrderIntentRetryGeneration(ctx, intentKey)
		if err == nil {
			effectiveKey = resolvedKey
		} else if existing, lookupErr := store.GetStockOrderIntentByKey(ctx, intentKey); lookupErr != nil || existing != nil {
			release()
			return stockEntryAdmission{}, nil, "", nil, err
		}
	}
	existing, err := e.existingStockEntryRequest(ctx, effectiveKey, ticker)
	if err != nil {
		release()
		return stockEntryAdmission{}, nil, "", nil, err
	}
	admission, err := e.prepareStockEntryAdmission(ctx, effectiveKey, ticker, sizePct, existing)
	if err != nil {
		release()
		return stockEntryAdmission{}, existing, effectiveKey, nil, err
	}
	return admission, existing, effectiveKey, release, nil
}

func (e *Engine) existingStockEntryRequest(ctx context.Context, intentKey, ticker string) (*execution.OrderRequest, error) {
	if !store.IsConnected() {
		return nil, nil
	}
	intent, err := store.GetStockOrderIntentByKey(ctx, intentKey)
	if err != nil || intent == nil {
		return nil, err
	}
	return stockEntryRequestFromIntent(intent, intentKey, ticker)
}

func stockEntryRequestFromIntent(intent *store.StockOrderIntent, intentKey, ticker string) (*execution.OrderRequest, error) {
	if execution.IsTerminalOrderStatus(intent.Status) {
		if intent.FilledQty > 0 {
			return nil, errStockEntryIntentComplete
		}
		return nil, nil
	}
	var req execution.OrderRequest
	if err := json.Unmarshal(intent.RequestPayload, &req); err != nil {
		return nil, fmt.Errorf("decode existing stock entry intent: %w", err)
	}
	if req.IntentKey != intentKey || !strings.EqualFold(req.Ticker, ticker) ||
		req.Side != execution.OrderSideBuy || req.Type != execution.OrderTypeMarket {
		return nil, errorsForAdmission("durable entry replay payload does not match the current BUY signal")
	}
	return &req, nil
}

func (e *Engine) prepareStockEntryAdmission(
	ctx context.Context,
	intentKey, ticker string,
	sizePct float64,
	existing *execution.OrderRequest,
) (stockEntryAdmission, error) {
	if brokerName(e.executor) != "alpaca" {
		return stockEntryAdmission{}, nil
	}
	accountView, ok := e.executor.(interface {
		AccountSnapshot(context.Context) (execution.BrokerAccount, error)
	})
	if !ok {
		return stockEntryAdmission{}, errorsForAdmission("broker account snapshot is unavailable")
	}
	assetView, ok := e.executor.(execution.StockAssetProvider)
	if !ok {
		return stockEntryAdmission{}, errorsForAdmission("broker asset snapshot is unavailable")
	}
	workingView, ok := e.executor.(execution.WorkingOrdersLister)
	if !ok {
		return stockEntryAdmission{}, errorsForAdmission("broker working orders are unavailable")
	}
	positionView, ok := e.executor.(interface {
		PositionsSnapshot(context.Context) ([]execution.BrokerPosition, error)
	})
	if !ok {
		return stockEntryAdmission{}, errorsForAdmission("broker position snapshot is unavailable")
	}
	if !store.IsConnected() {
		return stockEntryAdmission{}, errorsForAdmission("durable stock entry reservations are unavailable")
	}

	asset, err := assetView.StockAssetSnapshot(ctx, ticker)
	if err != nil {
		return stockEntryAdmission{}, fmt.Errorf("stock entry admission: fetch asset: %w", err)
	}
	working, err := workingView.GetWorkingOrders(ctx)
	if err != nil {
		return stockEntryAdmission{}, fmt.Errorf("stock entry admission: fetch working orders: %w", err)
	}
	// Fetch buying power after the working-order list. applyStockEntryReservations
	// assumes this account snapshot already reflects every listed broker hold.
	// The reverse order can overstate capacity when a BUY appears between reads.
	account, err := accountView.AccountSnapshot(ctx)
	if err != nil {
		return stockEntryAdmission{}, fmt.Errorf("stock entry admission: fetch account: %w", err)
	}
	positions, err := positionView.PositionsSnapshot(ctx)
	if err != nil {
		return stockEntryAdmission{}, fmt.Errorf("stock entry admission: fetch positions: %w", err)
	}
	reservations, err := store.ListActiveStockEntryReservations(ctx)
	if err != nil {
		return stockEntryAdmission{}, fmt.Errorf("stock entry admission: load reservations: %w", err)
	}
	quote, err := e.marketData.FetchQuote(ctx, ticker)
	if err != nil {
		return stockEntryAdmission{}, fmt.Errorf("stock entry admission: fetch quote: %w", err)
	}

	now := time.Now().UTC()
	if err := validateStockAsset(asset, ticker, now); err != nil {
		return stockEntryAdmission{}, err
	}
	capacity, err := validateStockAccount(
		account, asset.Marginable, e.cfg.Markets.Stocks.ExpectedAccountNumber, now,
	)
	if err != nil {
		return stockEntryAdmission{}, err
	}
	ask, err := validateStockAsk(quote, ticker, now)
	if err != nil {
		return stockEntryAdmission{}, err
	}

	capacity, err = applyStockEntryReservations(capacity, intentKey, reservations, working)
	if err != nil {
		return stockEntryAdmission{}, err
	}

	var quantity float64
	brokerHeldReplay, err := e.existingEntryBrokerReplay(ctx, intentKey, existing, reservations, working)
	if err != nil {
		return stockEntryAdmission{}, err
	}
	if existing != nil {
		quantity = existing.Quantity
		if !asset.Fractionable && quantity != math.Floor(quantity) {
			return stockEntryAdmission{}, errorsForAdmission("existing entry quantity is fractional for a nonfractionable asset")
		}
	} else {
		quantity, err = sizeStockEntry(account.Equity, sizePct, capacity, ask, asset.Fractionable)
		if err != nil {
			return stockEntryAdmission{}, err
		}
		// A small account can have a valid positive allocation that rounds to
		// zero only because this asset is whole-share-only. Permit exactly one
		// share, never a fractional fallback, and only when the resulting
		// notional itself fits the configured position cap and fresh capacity.
		// The final release gate still applies the exact stop-distance dollar
		// risk check before the broker request is sent.
		if minimumWholeShareAllowed(quantity, asset.Fractionable, sizePct, ask, account.Equity, capacity, e.cfg.Risk.MaxPositionPct) {
			quantity = 1
		}
	}
	if !finitePositive(quantity) {
		return stockEntryAdmission{}, errorsForAdmission("available buying power produces zero quantity")
	}
	if !brokerHeldReplay && quantity*ask > capacity+1e-7 {
		return stockEntryAdmission{}, errorsForAdmission("exact entry quantity exceeds available buying power")
	}

	peakEquity, peakObservedAt := e.updateStockAccountPeak(account.AccountID, account.Equity, account.FetchedAt)
	baseline, err := store.ObserveAccountRiskBaselineWithPeak(ctx, account.AccountID, account.Equity, peakEquity, account.LastEquity, account.FetchedAt, peakObservedAt)
	if err != nil {
		return stockEntryAdmission{}, fmt.Errorf("stock entry admission: persist account risk baseline: %w", err)
	}
	return stockEntryAdmission{
		BrokerAuthoritative: true,
		Account:             account, Asset: asset, Ask: ask, QuoteAt: quote.Timestamp, Quantity: quantity,
		RemainingCapacity: capacity,
		PeakEquity:        baseline.PeakEquity, DailyBaseline: baseline.LastEquity,
		Reservations: reservations, BrokerHeldReplay: brokerHeldReplay,
		WorkingOrders: working, BrokerPositions: positions,
	}, nil
}

func manualWorkingEntryTrades(
	working []execution.WorkingOrder,
	reservations []store.ActiveStockEntryReservation,
	freshAsk float64,
) []store.Trade {
	trades := make([]store.Trade, 0, len(working))
	for _, order := range working {
		if !isStandardStockWorkingOrder(order) || !strings.EqualFold(order.Side, "buy") {
			continue
		}
		matched := false
		for _, reservation := range reservations {
			if (order.OrderID != "" && order.OrderID == reservation.BrokerOrderID) ||
				(order.TradeID != "" && order.TradeID == reservation.ClientOrderID) {
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		if strings.TrimSpace(order.Ticker) == "" || !finitePositive(order.RemainingQty) {
			continue // validation rejects malformed orders before this projection
		}
		entryPrice := order.Price
		if !finitePositive(entryPrice) {
			entryPrice = freshAsk
		}
		if !finitePositive(entryPrice) {
			continue
		}
		trades = append(trades, store.Trade{
			Ticker: order.Ticker, Direction: "long", Status: "open",
			Quantity: order.RemainingQty, EntryPrice: entryPrice,
		})
	}
	return trades
}

func validateStockAdmissionFreshness(admission stockEntryAdmission, now time.Time) error {
	if !admission.BrokerAuthoritative || admission.BrokerHeldReplay {
		return nil
	}
	if err := validateAdmissionTimestamp("asset", admission.Asset.FetchedAt, now); err != nil {
		return err
	}
	if err := validateAdmissionTimestamp("account", admission.Account.FetchedAt, now); err != nil {
		return err
	}
	return validateAdmissionTimestamp("quote", admission.QuoteAt, now)
}

func existingEntryBrokerHeld(
	intentKey string,
	reservations []store.ActiveStockEntryReservation,
	working []execution.WorkingOrder,
) bool {
	for _, reservation := range reservations {
		if reservation.IdempotencyKey != intentKey {
			continue
		}
		for _, order := range working {
			if isStandardStockWorkingOrder(order) && strings.EqualFold(order.Side, "buy") &&
				((order.OrderID != "" && order.OrderID == reservation.BrokerOrderID) ||
					(order.TradeID != "" && order.TradeID == reservation.ClientOrderID)) {
				return true
			}
		}
	}
	return false
}

// existingEntryBrokerReplay identifies a durable entry that must be handed
// back to the coordinator rather than re-admitted as a new BUY. A working
// order is enough to prove that case. When the order has raced from working to
// filled (or another terminal state), ask the broker by client ID before
// applying fresh-entry risk or pause gates. The coordinator then records the
// broker truth and projects any missing local trade or protective stop.
func (e *Engine) existingEntryBrokerReplay(
	ctx context.Context,
	intentKey string,
	existing *execution.OrderRequest,
	reservations []store.ActiveStockEntryReservation,
	working []execution.WorkingOrder,
) (bool, error) {
	if existing == nil {
		return false, nil
	}
	if existingEntryBrokerHeld(intentKey, reservations, working) {
		return true, nil
	}

	var reservation *store.ActiveStockEntryReservation
	for i := range reservations {
		if reservations[i].IdempotencyKey == intentKey {
			reservation = &reservations[i]
			break
		}
	}
	if reservation == nil {
		return false, errorsForAdmission("durable entry replay has no active reservation")
	}
	lookup, ok := e.executor.(execution.ClientOrderLookup)
	if !ok {
		return false, errorsForAdmission("broker client-id lookup is unavailable for durable entry replay")
	}
	if _, err := lookup.GetOrderByClientID(ctx, existing.ClientOrderID); err == nil {
		return true, nil
	} else if !errors.Is(err, execution.ErrOrderNotFound) {
		return false, fmt.Errorf("stock entry admission: look up durable entry by client ID: %w", err)
	}

	// A submitted intent whose client ID is temporarily unavailable is
	// ambiguous. Preserve the replay path so the coordinator can fail closed
	// instead of allowing a second BUY to be admitted and submitted.
	if reservation.SubmittedAt != nil || reservation.BrokerOrderID != "" {
		return true, nil
	}
	return false, nil
}

func (e *Engine) updateStockAccountPeak(accountID string, equity float64, observedTimes ...time.Time) (float64, time.Time) {
	observedAt := time.Now().UTC()
	if len(observedTimes) > 0 && !observedTimes[0].IsZero() {
		observedAt = observedTimes[0]
	}
	e.stockAccountPeakMu.Lock()
	defer e.stockAccountPeakMu.Unlock()
	if e.stockAccountPeakID != accountID {
		e.stockAccountPeakID = accountID
		e.stockAccountPeak = equity
		e.stockAccountPeakAt = observedAt
		return e.stockAccountPeak, e.stockAccountPeakAt
	}
	if equity > e.stockAccountPeak {
		e.stockAccountPeak = equity
		e.stockAccountPeakAt = observedAt
	}
	return e.stockAccountPeak, e.stockAccountPeakAt
}

func (e *Engine) stockPeak(accountID string) float64 {
	e.stockAccountPeakMu.Lock()
	defer e.stockAccountPeakMu.Unlock()
	if e.stockAccountPeakID != accountID {
		return 0
	}
	return e.stockAccountPeak
}

func applyStockEntryReservations(
	capacity float64,
	intentKey string,
	reservations []store.ActiveStockEntryReservation,
	working []execution.WorkingOrder,
) (float64, error) {
	matchedBrokerOrders := make(map[string]struct{}, len(working))
	for _, order := range working {
		if !isStandardStockWorkingOrder(order) || !strings.EqualFold(order.Side, string(execution.OrderSideBuy)) {
			continue
		}
		if (order.Status != "pending" && order.Status != "partial") ||
			!finitePositive(order.RemainingQty) || strings.TrimSpace(order.Ticker) == "" {
			return 0, errorsForAdmission(fmt.Sprintf("working BUY order %q is malformed", order.OrderID))
		}
		var matched *store.ActiveStockEntryReservation
		for i := range reservations {
			byBrokerID := order.OrderID != "" && order.OrderID == reservations[i].BrokerOrderID
			byClientID := order.TradeID != "" && order.TradeID == reservations[i].ClientOrderID
			if !byBrokerID && !byClientID {
				continue
			}
			if matched != nil && matched.IntentID != reservations[i].IntentID {
				return 0, errorsForAdmission(fmt.Sprintf("working BUY order %q has contradictory identity", order.OrderID))
			}
			matched = &reservations[i]
		}
		if matched != nil {
			if !strings.EqualFold(order.Ticker, matched.Ticker) ||
				math.Abs(order.RemainingQty-matched.RemainingQty) > 0.000000001 ||
				(order.OrderID != "" && matched.BrokerOrderID != "" && order.OrderID != matched.BrokerOrderID) ||
				(order.TradeID != "" && order.TradeID != matched.ClientOrderID) {
				return 0, errorsForAdmission(fmt.Sprintf("working BUY order %q conflicts with durable reservation", order.OrderID))
			}
			matchedBrokerOrders[matched.IntentID] = struct{}{}
		} else if strings.HasPrefix(order.TradeID, "te-stock-") {
			return 0, errorsForAdmission(
				fmt.Sprintf("unattributed working BUY order %q", order.OrderID),
			)
		}
	}

	for _, reservation := range reservations {
		if reservation.IdempotencyKey == intentKey {
			// This request already owns its durable reservation. Re-admission
			// verifies the immutable quantity without charging it twice.
			continue
		}
		if _, brokerHeld := matchedBrokerOrders[reservation.IntentID]; brokerHeld {
			// Alpaca's fresh buying power already includes this hold.
			continue
		}
		if reservation.SubmittedAt != nil && reservation.BrokerError != "" && reservation.BrokerOrderID == "" {
			return 0, errorsForAdmission(
				fmt.Sprintf("entry intent %q has unresolved broker truth", reservation.IdempotencyKey),
			)
		}
		reservationPrice := reservation.ReferencePrice
		if !finitePositive(reservationPrice) || !finiteNonNegative(reservation.RemainingQty) {
			return 0, errorsForAdmission("active entry reservation is malformed")
		}
		capacity -= reservation.RemainingQty * reservationPrice
	}
	if capacity < 0 {
		capacity = 0
	}

	return capacity, nil
}

func isStandardStockWorkingOrder(order execution.WorkingOrder) bool {
	assetClass := strings.TrimSpace(order.AssetClass)
	return assetClass == "" || strings.EqualFold(assetClass, "us_equity")
}

func brokerName(exec execution.Executor) string {
	named, _ := exec.(interface{ BrokerName() string })
	if named == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(named.BrokerName()))
}

func validateStockAsset(asset execution.StockAsset, ticker string, now time.Time) error {
	if !asset.TradableAvailable || !asset.MarginableAvailable || !asset.FractionableAvailable ||
		!strings.EqualFold(asset.Symbol, ticker) || asset.AssetClass != "us_equity" ||
		!strings.EqualFold(asset.Status, "active") || !asset.Tradable {
		return errorsForAdmission("asset is not an active, tradable US equity")
	}
	if err := validateAdmissionTimestamp("asset", asset.FetchedAt, now); err != nil {
		return err
	}
	return nil
}

func validateStockAccount(account execution.BrokerAccount, marginable bool, expectedAccountNumber string, now time.Time) (float64, error) {
	if err := validateBrokerPortfolioAccount(account, expectedAccountNumber, now); err != nil {
		return 0, err
	}
	var capacity float64
	if marginable {
		if !account.RegTBuyingPowerAvailable || !finiteNonNegative(account.RegTBuyingPower) {
			return 0, errorsForAdmission("account Reg-T buying power is missing or invalid")
		}
		capacity = math.Min(account.BuyingPower, account.RegTBuyingPower)
	} else {
		if !account.NonMarginableBuyingPowerAvailable || !finiteNonNegative(account.NonMarginableBuyingPower) {
			return 0, errorsForAdmission("account non-marginable buying power is missing or invalid")
		}
		capacity = account.NonMarginableBuyingPower
	}
	if account.Multiplier <= 1 {
		if account.Cash < 0 {
			return 0, errorsForAdmission("negative cash is invalid for a cash account")
		}
		capacity = math.Min(capacity, account.Cash)
	}
	return capacity, nil
}

func validateBrokerPortfolioAccount(account execution.BrokerAccount, expectedAccountNumber string, now time.Time) error {
	if err := execution.ValidateBrokerAccount(account, expectedAccountNumber, now); err != nil {
		return errorsForAdmission(err.Error())
	}
	return nil
}

func validateStockAsk(quote *data.Quote, ticker string, now time.Time) (float64, error) {
	if quote == nil || !strings.EqualFold(quote.Ticker, ticker) || !finitePositive(quote.Ask) {
		return 0, errorsForAdmission("fresh positive ask is unavailable")
	}
	if err := validateAdmissionTimestamp("quote", quote.Timestamp, now); err != nil {
		return 0, err
	}
	return quote.Ask, nil
}

func validateAdmissionTimestamp(label string, observed, now time.Time) error {
	if observed.IsZero() {
		return errorsForAdmission(label + " snapshot timestamp is missing")
	}
	age := now.Sub(observed)
	if age > stockAdmissionMaxAge || age < -stockAdmissionFutureTolerance {
		return errorsForAdmission(label + " snapshot is stale or future-dated")
	}
	return nil
}

func floorStockQuantity(quantity float64, fractionable bool) float64 {
	if !finitePositive(quantity) {
		return 0
	}
	if !fractionable {
		return math.Floor(quantity)
	}
	return math.Floor(quantity*stockQuantityScale) / stockQuantityScale
}

func sizeStockEntry(equity, sizePct, capacity, ask float64, fractionable bool) (float64, error) {
	desiredNotional := equity * sizePct / 100
	if !finitePositive(desiredNotional) || !finiteNonNegative(capacity) || !finitePositive(ask) {
		return 0, errorsForAdmission("desired notional, buying power, or ask is invalid")
	}
	return floorStockQuantity(math.Min(desiredNotional, capacity)/ask, fractionable), nil
}

func minimumWholeShareAllowed(quantity float64, fractionable bool, sizePct, ask, equity, capacity, maxPositionPct float64) bool {
	return quantity <= 0 && !fractionable && sizePct > 0 && ask > 0 &&
		maxPositionPct > 0 && ask <= equity*maxPositionPct/100+1e-9 && ask <= capacity+1e-9
}

func finite(v float64) bool            { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func finitePositive(v float64) bool    { return finite(v) && v > 0 }
func finiteNonNegative(v float64) bool { return finite(v) && v >= 0 }

func errorsForAdmission(reason string) error {
	return fmt.Errorf("stock entry admission: %s", reason)
}

// checkStockEntryReleaseRisk applies fresh-entry policy only while the order
// is still under local release control. A broker-held replay is already live;
// it must reach the coordinator's exact client-ID lookup so any fill is
// projected locally instead of being abandoned because conditions changed.
func checkStockEntryReleaseRisk(admission stockEntryAdmission, in risk.CheckInput) ([]risk.CheckResult, bool) {
	if admission.BrokerHeldReplay {
		return nil, true
	}
	return risk.CheckAll(in)
}

func brokerStockRiskState(current portfolio.State, admission stockEntryAdmission) portfolio.State {
	if !admission.BrokerAuthoritative {
		return current
	}
	current.TotalValue = admission.Account.Equity
	current.Cash = admission.Account.Cash
	current.PeakValue = admission.PeakEquity
	// The persisted account-scoped daily reference captures both realized and
	// unrealized loss. It therefore remains correct across restarts instead of
	// resetting to a process-local realized-P&L counter.
	if admission.DailyBaseline > 0 {
		current.RealizedPnLDay = admission.Account.Equity - admission.DailyBaseline
	}
	return current
}

// stockEntryBracketReplayState keeps every durable replay immutable. Only a
// brand-new request may be reshaped from a plain market order into a bracket.
func stockEntryBracketReplayState(replaying bool, req execution.OrderRequest) (bracketed, mayBuild bool) {
	if replaying {
		return req.OrderClass == "bracket", false
	}
	return false, true
}

// hydrateStockEntryReplayPolicy makes every local trade and protection
// projection use the exit policy that was persisted with the immutable order.
func hydrateStockEntryReplayPolicy(sig *store.Signal, req execution.OrderRequest) {
	if sig == nil {
		return
	}
	sig.StopLoss = req.PolicyStopLoss
	sig.TakeProfit = req.PolicyTakeProfit
}

func reservationTrades(reservations []store.ActiveStockEntryReservation, excludeKey string) []store.Trade {
	trades := make([]store.Trade, 0, len(reservations))
	for _, reservation := range reservations {
		if reservation.IdempotencyKey == excludeKey || reservation.RemainingQty <= 0 || reservation.TradeID != "" {
			continue
		}
		trades = append(trades, store.Trade{
			Ticker: reservation.Ticker, Direction: "long", Status: "open",
			Quantity: reservation.RemainingQty, EntryPrice: reservation.ReferencePrice,
		})
	}
	return trades
}

// attachedReservationNotional accounts for the unfilled remainder of an entry
// that already has an attached trade. The trade supplies position ownership;
// this helper supplies only the still-working exposure, so it cannot inflate
// the position count or per-ticker lot count.
func attachedReservationNotional(reservations []store.ActiveStockEntryReservation, excludeKey, ticker string, mark float64) float64 {
	if mark <= 0 {
		return 0
	}
	var notional float64
	for _, reservation := range reservations {
		if reservation.IdempotencyKey == excludeKey || reservation.TradeID == "" ||
			!strings.EqualFold(reservation.Ticker, ticker) || reservation.RemainingQty <= 0 {
			continue
		}
		notional += reservation.RemainingQty * mark
	}
	return notional
}

func brokerLongNotional(positions []execution.BrokerPosition, ticker string, mark float64) float64 {
	if mark <= 0 {
		return 0
	}
	var quantity float64
	for _, position := range positions {
		if strings.EqualFold(position.Symbol, ticker) && !strings.EqualFold(position.Side, "short") && position.Quantity > 0 {
			quantity += position.Quantity
		}
	}
	return quantity * mark
}

// brokerLongPosition returns Alpaca's aggregate long for ticker. Broker
// positions are authoritative at entry release: a manual or otherwise
// unadopted holding must still be treated as an existing pyramid position.
// The boolean pair distinguishes no position from a malformed position whose
// missing average entry makes a safe pyramid-gain check impossible.
func brokerLongPosition(positions []execution.BrokerPosition, ticker string) (quantity, averageEntry float64, found, valid bool) {
	var costBasis float64
	for _, position := range positions {
		if !strings.EqualFold(position.Symbol, ticker) || strings.EqualFold(position.Side, "short") || position.Quantity <= 0 {
			continue
		}
		found = true
		if !finitePositive(position.AvgEntryPrice) {
			return 0, 0, true, false
		}
		quantity += position.Quantity
		costBasis += position.Quantity * position.AvgEntryPrice
	}
	if !found {
		return 0, 0, false, true
	}
	return quantity, costBasis / quantity, true, true
}

// mergeBrokerLongOccupancy makes broker truth part of every position budget,
// not only target-ticker notional. Alpaca aggregates one holding per symbol,
// so a broker position overlays local lots for that ticker instead of adding a
// second position; an otherwise-unrepresented broker symbol consumes one slot.
func mergeBrokerLongOccupancy(in *risk.CheckInput, positions []execution.BrokerPosition, target string) error {
	if in == nil {
		return nil
	}
	if in.OpenByTicker == nil {
		in.OpenByTicker = make(map[string]int)
	}
	for _, position := range positions {
		symbol := strings.TrimSpace(position.Symbol)
		if symbol == "" {
			return errorsForAdmission("broker position has no symbol for capacity check")
		}
		if !finite(position.Quantity) || math.Abs(position.Quantity) <= 1e-9 {
			return errorsForAdmission("broker position has invalid quantity for capacity check")
		}
		if strings.EqualFold(position.Side, "short") {
			// Shorts consume overall capacity but must not influence BUY-only
			// pyramid ownership or per-ticker long limits.
			key, count := riskInputTickerCount(in.OpenByTicker, symbol)
			if count == 0 {
				in.OpenByTicker[symbol] = 1
				in.OpenPositions++
			} else if key != symbol {
				in.OpenByTicker[key] = count
			}
			continue
		}
		if !strings.EqualFold(position.Side, "long") {
			return errorsForAdmission("broker position has invalid side for capacity check")
		}
		if position.Quantity <= 0 {
			return errorsForAdmission("broker long position has invalid quantity for capacity check")
		}
		key, count := riskInputTickerCount(in.OpenByTicker, symbol)
		if count == 0 {
			in.OpenByTicker[symbol] = 1
			in.OpenPositions++
		} else if key != symbol {
			// Preserve the map's existing casing so later risk checks observe
			// the same local count rather than creating a duplicate key.
			in.OpenByTicker[key] = count
		}
	}
	quantity, averageEntry, found, valid := brokerLongPosition(positions, target)
	if found && !valid {
		return errorsForAdmission("unable to verify broker-held position cost basis for pyramid risk check")
	}
	if found {
		in.HoldsLong = true
		// A broker snapshot can lag local durable fill projection. Preserve the
		// more conservative ownership values: a larger quantity and higher cost
		// basis cannot make a pyramid appear safer than either source does.
		in.LongQuantity = math.Max(in.LongQuantity, quantity)
		in.LongAvgEntryPrice = math.Max(in.LongAvgEntryPrice, averageEntry)
	}
	return nil
}

func riskInputTickerCount(counts map[string]int, ticker string) (string, int) {
	for key, count := range counts {
		if strings.EqualFold(key, ticker) {
			return key, count
		}
	}
	return ticker, 0
}

func pendingStockEntryNotional(
	reservations []store.ActiveStockEntryReservation,
	working []execution.WorkingOrder,
	excludeKey, ticker string,
	mark float64,
) float64 {
	var notional float64
	for _, trade := range reservationTrades(reservations, excludeKey) {
		if strings.EqualFold(trade.Ticker, ticker) {
			notional += trade.Quantity * mark
		}
	}
	notional += attachedReservationNotional(reservations, excludeKey, ticker, mark)
	for _, trade := range manualWorkingEntryTrades(working, reservations, mark) {
		if strings.EqualFold(trade.Ticker, ticker) {
			notional += trade.Quantity * mark
		}
	}
	return notional
}
