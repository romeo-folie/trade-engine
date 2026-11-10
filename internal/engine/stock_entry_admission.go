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
	// Side is the entry side: buy opens a long, sell opens a short (#599).
	Side    execution.OrderSide
	Account execution.BrokerAccount
	Asset   execution.StockAsset
	// Ask prices a BUY; Bid prices a short SELL. Only the side's own price is
	// populated, so a short can never be sized or released off the ask.
	Ask               float64
	Bid               float64
	QuoteAt           time.Time
	Quantity          float64
	RemainingCapacity float64
	PeakEquity        float64
	DailyBaseline     float64
	Reservations      []store.ActiveStockEntryReservation
	BrokerHeldReplay  bool
	WorkingOrders     []execution.WorkingOrder
	BrokerPositions   []execution.BrokerPosition
}

func (e *Engine) beginStockEntryAdmission(
	ctx context.Context,
	intentKey, ticker string,
	sizePct float64,
) (stockEntryAdmission, *execution.OrderRequest, string, func(), error) {
	return e.beginStockEntryAdmissionSide(ctx, execution.OrderSideBuy, intentKey, ticker, sizePct)
}

// beginStockEntryAdmissionSide is beginStockEntryAdmission for either entry
// side. A sell-side entry opens a short (#599) through the same durable intent,
// reservation, and replay rules as a long.
func (e *Engine) beginStockEntryAdmissionSide(
	ctx context.Context,
	side execution.OrderSide,
	intentKey, ticker string,
	sizePct float64,
) (stockEntryAdmission, *execution.OrderRequest, string, func(), error) {
	if side != execution.OrderSideBuy && side != execution.OrderSideSell {
		return stockEntryAdmission{}, nil, "", nil, errorsForAdmission("unknown entry side")
	}
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
	existing, err := e.existingStockEntryRequestSide(ctx, side, effectiveKey, ticker)
	if err != nil {
		release()
		return stockEntryAdmission{}, nil, "", nil, err
	}
	admission, err := e.prepareStockEntryAdmissionSide(ctx, side, effectiveKey, ticker, sizePct, existing)
	if err != nil {
		release()
		return stockEntryAdmission{}, existing, effectiveKey, nil, err
	}
	return admission, existing, effectiveKey, release, nil
}

func (e *Engine) existingStockEntryRequestSide(ctx context.Context, side execution.OrderSide, intentKey, ticker string) (*execution.OrderRequest, error) {
	if !store.IsConnected() {
		return nil, nil
	}
	intent, err := store.GetStockOrderIntentByKey(ctx, intentKey)
	if err != nil || intent == nil {
		return nil, err
	}
	return stockEntryRequestFromIntentSide(intent, side, intentKey, ticker)
}

func stockEntryRequestFromIntent(intent *store.StockOrderIntent, intentKey, ticker string) (*execution.OrderRequest, error) {
	return stockEntryRequestFromIntentSide(intent, execution.OrderSideBuy, intentKey, ticker)
}

func stockEntryRequestFromIntentSide(intent *store.StockOrderIntent, side execution.OrderSide, intentKey, ticker string) (*execution.OrderRequest, error) {
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
		req.Side != side || req.Type != execution.OrderTypeMarket {
		return nil, errorsForAdmission(fmt.Sprintf("durable entry replay payload does not match the current %s signal", entrySignalLabel(side)))
	}
	return &req, nil
}

func (e *Engine) prepareStockEntryAdmission(
	ctx context.Context,
	intentKey, ticker string,
	sizePct float64,
	existing *execution.OrderRequest,
) (stockEntryAdmission, error) {
	return e.prepareStockEntryAdmissionSide(ctx, execution.OrderSideBuy, intentKey, ticker, sizePct, existing)
}

func (e *Engine) prepareStockEntryAdmissionSide(
	ctx context.Context,
	side execution.OrderSide,
	intentKey, ticker string,
	sizePct float64,
	existing *execution.OrderRequest,
) (stockEntryAdmission, error) {
	short := side == execution.OrderSideSell
	if brokerName(e.executor) != "alpaca" {
		if short {
			// Short execution is defined only against broker truth: shortable,
			// easy-to-borrow, margin, and a broker-held bracket.
			return stockEntryAdmission{}, errorsForAdmission("short entries require the Alpaca broker")
		}
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
	if short && !asset.Marginable {
		return stockEntryAdmission{}, errorsForAdmission("short entry requires a marginable asset")
	}
	capacity, err := validateStockAccount(
		account, asset.Marginable, e.cfg.Markets.Stocks.ExpectedAccountNumber, now,
	)
	if err != nil {
		return stockEntryAdmission{}, err
	}
	// ask is the side's executable price: the ask for a BUY, the bid for a
	// short SELL (a market sell fills at or near the bid).
	var ask float64
	if short {
		ask, err = validateStockBid(quote, ticker, now)
	} else {
		ask, err = validateStockAsk(quote, ticker, now)
	}
	if err != nil {
		return stockEntryAdmission{}, err
	}
	// Shorts are whole-share only: Alpaca does not short fractional shares.
	fractionable := asset.Fractionable && !short

	// Engine-owned BUY exits (a buy-to-cover, software exit, containment
	// flatten, or resting buy stop on a short, #599) are not entries: they
	// reduce exposure and reserve nothing. Drop them before entry accounting
	// so they neither wedge admission as "unattributed" nor count as a long.
	working, err = withoutEngineBuyExits(ctx, working, store.GetStockOrderIntentByClientOrderID, store.GetStockOrderIntentByBrokerOrderID)
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
		if !fractionable && quantity != math.Floor(quantity) {
			return stockEntryAdmission{}, errorsForAdmission("existing entry quantity is fractional for a nonfractionable asset")
		}
	} else {
		quantity, err = sizeStockEntry(account.Equity, sizePct, capacity, ask, fractionable)
		if err != nil {
			return stockEntryAdmission{}, err
		}
		// A small account can have a valid positive allocation that rounds to
		// zero only because this asset is whole-share-only. Permit exactly one
		// share, never a fractional fallback, and only when the resulting
		// notional itself fits the configured position cap and fresh capacity.
		// The final release gate still applies the exact stop-distance dollar
		// risk check before the broker request is sent.
		if minimumWholeShareAllowed(quantity, fractionable, sizePct, ask, account.Equity, capacity, e.cfg.Risk.MaxPositionPct) {
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
	admission := stockEntryAdmission{
		BrokerAuthoritative: true, Side: side,
		Account: account, Asset: asset, Ask: ask, QuoteAt: quote.Timestamp, Quantity: quantity,
		RemainingCapacity: capacity,
		PeakEquity:        baseline.PeakEquity, DailyBaseline: baseline.LastEquity,
		Reservations: reservations, BrokerHeldReplay: brokerHeldReplay,
		WorkingOrders: working, BrokerPositions: positions,
	}
	if short {
		admission.Ask, admission.Bid = 0, ask
	}
	return admission, nil
}

// entrySignalLabel names the signal action that opens a position on side.
func entrySignalLabel(side execution.OrderSide) string {
	if side == execution.OrderSideSell {
		return "SHORT"
	}
	return "BUY"
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
			if isStandardStockWorkingOrder(order) && strings.EqualFold(order.Side, reservationSide(reservation)) &&
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
		if !isStandardStockWorkingOrder(order) {
			continue
		}
		if strings.EqualFold(order.Side, string(execution.OrderSideSell)) {
			// A working SELL is either a pending short entry the engine owns
			// (#599), whose hold Alpaca's buying power already reflects, or an
			// exit leg (stop/target) that reserves nothing. Only an exact
			// sell-side reservation match is recorded; anything else is skipped.
			if matched := matchSellEntryReservation(order, reservations); matched != nil {
				matchedBrokerOrders[matched.IntentID] = struct{}{}
			}
			continue
		}
		if !strings.EqualFold(order.Side, string(execution.OrderSideBuy)) {
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

// reservationSide is the broker order side of a reserved entry ("buy" when
// unrecorded, for reservations persisted before shorts existed).
func reservationSide(r store.ActiveStockEntryReservation) string {
	if strings.EqualFold(r.Side, string(execution.OrderSideSell)) {
		return string(execution.OrderSideSell)
	}
	return string(execution.OrderSideBuy)
}

// matchSellEntryReservation returns the sell-side entry reservation a working
// SELL order belongs to, only when its identity, ticker, and remaining
// quantity agree exactly.
func matchSellEntryReservation(order execution.WorkingOrder, reservations []store.ActiveStockEntryReservation) *store.ActiveStockEntryReservation {
	for i := range reservations {
		r := &reservations[i]
		if reservationSide(*r) != string(execution.OrderSideSell) {
			continue
		}
		byBrokerID := order.OrderID != "" && order.OrderID == r.BrokerOrderID
		byClientID := order.TradeID != "" && order.TradeID == r.ClientOrderID
		if (byBrokerID || byClientID) && strings.EqualFold(order.Ticker, r.Ticker) &&
			math.Abs(order.RemainingQty-r.RemainingQty) <= 0.000000001 {
			return r
		}
	}
	return nil
}

// withoutEngineBuyExits removes working BUY orders that are exits rather than
// entries: a bracket child (it has a parent order; a short's stop and target
// are BUYs with broker-generated client IDs), or an order the durable intent
// store attributes to a non-entry intent by client or broker order ID. An
// engine-shaped client ID whose intent cannot be read fails closed; an order
// with no intent stays in the list, where entry accounting rejects or counts
// it as before.
func withoutEngineBuyExits(
	ctx context.Context,
	working []execution.WorkingOrder,
	byClientID, byBrokerID func(context.Context, string) (*store.StockOrderIntent, error),
) ([]execution.WorkingOrder, error) {
	out := make([]execution.WorkingOrder, 0, len(working))
	for _, order := range working {
		if !strings.EqualFold(order.Side, string(execution.OrderSideBuy)) {
			out = append(out, order)
			continue
		}
		if strings.TrimSpace(order.ParentOrderID) != "" {
			continue // a bracket child closes its parent's position
		}
		var intent *store.StockOrderIntent
		var err error
		if strings.HasPrefix(order.TradeID, "te-stock-") {
			intent, err = byClientID(ctx, order.TradeID)
		} else if order.OrderID != "" {
			intent, err = byBrokerID(ctx, order.OrderID)
		}
		if err != nil {
			return nil, fmt.Errorf("stock entry admission: attribute working BUY %q: %w", order.OrderID, err)
		}
		if intent != nil && !strings.HasPrefix(intent.IdempotencyKey, "entry:") {
			continue
		}
		out = append(out, order)
	}
	return out, nil
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

// validateStockBid returns the fresh executable bid for a short sale. A
// crossed or one-sided quote is refused rather than priced.
func validateStockBid(quote *data.Quote, ticker string, now time.Time) (float64, error) {
	if quote == nil || !strings.EqualFold(quote.Ticker, ticker) || !finitePositive(quote.Bid) ||
		!finitePositive(quote.Ask) || quote.Bid > quote.Ask {
		return 0, errorsForAdmission("fresh positive two-sided bid is unavailable")
	}
	if err := validateAdmissionTimestamp("quote", quote.Timestamp, now); err != nil {
		return 0, err
	}
	return quote.Bid, nil
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

// repricePreparedStockEntryRequest makes the request that reaches the broker
// match the final ask and quantity evaluated by release risk. It is used only
// for a durable intent proven not to be broker-held; persistence happens at
// the final release boundary before PlaceOrder.
func repricePreparedStockEntryRequest(req execution.OrderRequest, quantity, referencePrice float64) (execution.OrderRequest, error) {
	quantity, err := preparedReplayQuantity(req, quantity)
	if err != nil {
		return execution.OrderRequest{}, err
	}
	req.Quantity = quantity
	req.ReferencePrice = referencePrice
	return req, nil
}

// preparedReplayQuantity preserves the persisted broker-protection contract.
// Bracket and uncapped entries require a whole-share parent so their broker
// protective stop can be submitted for the same quantity.
func preparedReplayQuantity(req execution.OrderRequest, quantity float64) (float64, error) {
	if req.RequireBrokerProtection || req.UncappedTakeProfit || req.OrderClass == "bracket" {
		quantity = math.Floor(quantity)
		if quantity < 1 {
			return 0, errorsForAdmission("protected prepared replay requires at least one whole share")
		}
	}
	return quantity, nil
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
			Ticker: reservation.Ticker, Direction: reservation.Direction(), Status: "open",
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
// so a broker position overlays a filled local lot for that ticker instead of
// adding a second position. Pending reservations are not positions yet, so an
// otherwise-unrepresented broker holding consumes its own slot alongside them.
func mergeBrokerLongOccupancy(in *risk.CheckInput, positions []execution.BrokerPosition, target string, durableOwned []store.Trade) error {
	if in == nil {
		return nil
	}
	if in.OpenByTicker == nil {
		in.OpenByTicker = make(map[string]int)
	}
	filledLongs := make(map[string]struct{})
	for _, trade := range durableOwned {
		if strings.EqualFold(trade.Direction, "long") && trade.Quantity > 0 {
			filledLongs[strings.ToUpper(strings.TrimSpace(trade.Ticker))] = struct{}{}
		}
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
			if strings.EqualFold(symbol, target) {
				return errorsForAdmission("broker holds a short position in the requested BUY ticker")
			}
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
		_, representedByFilledLot := filledLongs[strings.ToUpper(symbol)]
		if !representedByFilledLot {
			// A broker-only long and a pending BUY are distinct future lots. Do
			// not let the pending reservation hide the existing broker holding.
			in.OpenByTicker[key] = count + 1
			in.OpenPositions++
		} else if count == 0 {
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
