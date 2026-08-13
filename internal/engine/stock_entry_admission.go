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
	Reservations        []store.ActiveStockEntryReservation
	BrokerHeldReplay    bool
	WorkingOrders       []execution.WorkingOrder
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
	brokerHeldReplay := existing != nil && existingEntryBrokerHeld(intentKey, reservations, working)
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
	}
	if !finitePositive(quantity) {
		return stockEntryAdmission{}, errorsForAdmission("available buying power produces zero quantity")
	}
	if !brokerHeldReplay && quantity*ask > capacity+1e-7 {
		return stockEntryAdmission{}, errorsForAdmission("exact entry quantity exceeds available buying power")
	}

	e.updateStockAccountPeak(account.AccountID, account.Equity)
	peak := e.stockPeak(account.AccountID)
	return stockEntryAdmission{
		BrokerAuthoritative: true,
		Account:             account, Asset: asset, Ask: ask, QuoteAt: quote.Timestamp, Quantity: quantity,
		RemainingCapacity: capacity,
		PeakEquity:        peak, Reservations: reservations, BrokerHeldReplay: brokerHeldReplay,
		WorkingOrders: working,
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

func (e *Engine) updateStockAccountPeak(accountID string, equity float64) {
	e.stockAccountPeakMu.Lock()
	defer e.stockAccountPeakMu.Unlock()
	if e.stockAccountPeakID != accountID {
		e.stockAccountPeakID = accountID
		e.stockAccountPeak = equity
		return
	}
	if equity > e.stockAccountPeak {
		e.stockAccountPeak = equity
	}
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
	if err := validateAdmissionTimestamp("account", account.FetchedAt, now); err != nil {
		return err
	}
	if !strings.EqualFold(account.Status, "active") || !strings.EqualFold(account.Currency, "USD") ||
		account.TradingBlocked || account.AccountBlocked || account.TradeSuspendedByUser {
		return errorsForAdmission("account is not active, unrestricted, and USD-denominated")
	}
	if strings.TrimSpace(account.AccountID) == "" || strings.TrimSpace(account.AccountNumber) == "" {
		return errorsForAdmission("account identity is missing")
	}
	if expected := strings.TrimSpace(expectedAccountNumber); expected != "" && account.AccountNumber != expected {
		return errorsForAdmission("connected account does not match expected_account_number")
	}
	if !account.CashAvailable || !account.BuyingPowerAvailable || !account.MultiplierAvailable ||
		!finiteNonNegative(account.Equity) || !finite(account.Cash) ||
		!finiteNonNegative(account.BuyingPower) || !finite(account.Multiplier) || account.Multiplier <= 0 {
		return errorsForAdmission("account equity, cash, buying power, or multiplier is invalid")
	}
	if !account.LastEquityAvailable || !finiteNonNegative(account.LastEquity) {
		return errorsForAdmission("account last_equity is missing or invalid")
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
	// RealizedPnLDay intentionally remains local realized P&L. Alpaca's
	// equity-last_equity includes unrealized movement and transfers.
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
