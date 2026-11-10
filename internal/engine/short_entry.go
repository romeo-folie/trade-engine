package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/risk"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// shortMarketFacts are the market-side inputs to risk.EvaluateShortEligibility
// that do not come from the broker admission snapshot. A zero value means
// unknown, and the eligibility rules treat unknown as ineligible.
type shortMarketFacts struct {
	PrevClose       float64
	SessionLow      float64
	SSRCarriedOver  bool
	AvgDollarVolume float64
	EarningsKnown   bool
	DaysToEarnings  *int
}

// executeShortEntry opens a short (#599) through the same durable entry path
// as a long BUY: a sell-side order intent, broker admission priced off the
// fresh bid, the full entry-risk gate, and a mandatory broker-held bracket
// (stop above, target below). Eligibility is re-checked against facts
// observed at order time; anything unknown blocks the short.
//
// The post-placement projection (trade row, bracket legs, protection
// verification, fill polling, containment) is the shared
// placeStockEntryAndProject, with direction "short".
func (e *Engine) executeShortEntry(ctx context.Context, sigID string, sig store.Signal, fromQueue bool) {
	block := func(reason, failedCheck string) {
		if err := store.UpdateSignalStatus(ctx, sigID, "blocked", reason); err != nil {
			logger.Warn("update signal status", "id", sigID, "err", err)
		}
		logger.Warn("short entry blocked", "ticker", sig.Ticker, "reason", reason)
		e.bus.Publish(events.SignalBlocked, events.SignalBlockedPayload{
			Ticker: sig.Ticker, Action: sig.Action, Confidence: sig.Confidence,
			BlockReason: reason, FailedCheck: failedCheck,
		})
	}
	shortsCfg := e.cfg.Risk.Shorts
	if !shortsCfg.Enabled {
		block("short selling is disabled (risk.shorts.enabled=false)", "executable_action")
		return
	}
	if sig.PullbackStrength {
		// The pullback setup is a long-only construct with a low-of-day stop.
		block("pullback strength setups are long-only", "executable_action")
		return
	}
	if !store.IsConnected() {
		block("durable short entry requires the database", "durable_intent")
		return
	}
	signalReferencePrice := sig.AcceptedReferencePrice
	if signalReferencePrice <= 0 {
		signalReferencePrice = sig.SignalPrice
	}

	// An in-flight exit in either direction still owns broker quantity. Never
	// open a short behind it.
	owned, err := store.GetOwnedPositionTrades(ctx)
	if err != nil {
		block("unable to verify open positions (DB error)", "position lookup")
		return
	}
	if tickerHasExitingTradeIn(owned, sig.Ticker) {
		block("prior exit is still reconciling", "exit_ownership")
		return
	}

	// Market facts are gathered before admission: they can take seconds, and
	// the coordinated broker snapshot must stay fresh until release.
	facts := e.shortMarketFacts(ctx, sig.Ticker, time.Now())

	sizePct := sig.SizePct
	if !(sizePct > 0) || sizePct > shortsCfg.SizePct {
		sizePct = shortsCfg.SizePct
	}
	intentKey := "entry:" + sigID
	admission, existingRequest, effectiveIntentKey, releaseAdmission, err := e.beginStockEntryAdmissionSide(
		ctx, execution.OrderSideSell, intentKey, sig.Ticker, sizePct,
	)
	if err != nil {
		if errors.Is(err, errStockEntryIntentComplete) {
			return
		}
		if fromQueue && existingRequest != nil {
			if queueErr := store.QueueSignalForDeferredExecution(ctx, sigID); queueErr != nil {
				logger.Warn("keep prepared short entry queued after temporary admission failure", "id", sigID, "err", queueErr)
			}
			logger.Warn("prepared short entry admission unavailable — retrying on a later tick", "ticker", sig.Ticker, "err", err)
			return
		}
		block("short entry admission failed: "+err.Error(), "short_admission")
		return
	}
	defer releaseAdmission()
	intentKey = effectiveIntentKey
	if !admission.BrokerAuthoritative || admission.Side != execution.OrderSideSell {
		block("short entry requires an authoritative broker admission", "short_admission")
		return
	}

	bid := admission.Bid
	if !admission.BrokerHeldReplay {
		if e.rejectStaleSignalExecution(ctx, sigID, sig, signalReferencePrice, bid) {
			return
		}
		sig.SignalPrice = bid
		if err := store.UpdateSignalPrice(ctx, sigID, bid); err != nil {
			logger.Warn("update signal with fresh broker bid", "id", sigID, "err", err)
		}
	}

	qty := admission.Quantity
	orderReq := execution.OrderRequest{
		Ticker:           sig.Ticker,
		Side:             execution.OrderSideSell,
		Type:             execution.OrderTypeMarket,
		Quantity:         qty,
		ReferencePrice:   bid,
		IntentKey:        intentKey,
		PolicyStopLoss:   sig.StopLoss,
		PolicyTakeProfit: sig.TakeProfit,
	}
	replayingExisting := existingRequest != nil
	if replayingExisting {
		orderReq = *existingRequest
		hydrateStockEntryReplayPolicy(&sig, orderReq)
		if !orderReq.RequireBrokerProtection || orderReq.OrderClass != "bracket" {
			// Every short is persisted as a protected bracket; anything else
			// is not a payload this build wrote and is never released.
			block("prepared short entry lacks mandatory broker protection", "broker protection")
			return
		}
		if !admission.BrokerHeldReplay {
			var repriceErr error
			orderReq, repriceErr = repricePreparedStockEntryRequest(orderReq, qty, bid)
			if repriceErr != nil {
				block("unable to preserve whole-share protected short replay", "broker protection")
				return
			}
		}
	}

	var durableOwned []store.Trade
	var riskIn risk.CheckInput
	if !admission.BrokerHeldReplay {
		durableOwned = append(durableOwned, owned...)
		riskOwned := append([]store.Trade(nil), owned...)
		riskOwned = append(riskOwned, reservationTrades(admission.Reservations, intentKey)...)
		riskOwned = append(riskOwned, manualWorkingEntryTrades(admission.WorkingOrders, admission.Reservations, bid)...)

		grossShort, tickerShort, exposureErr := shortExposure(
			admission.BrokerPositions, durableOwned, admission.Reservations, intentKey, sig.Ticker,
		)
		if !replayingExisting {
			// The short exposure caps are a capacity limit, not only a veto:
			// size down to the tighter remaining cap before the bracket is
			// built, then let eligibility confirm the exact proposal.
			if exposureErr == nil {
				qty = capShortQuantity(qty, bid, admission.Account.Equity, grossShort, tickerShort,
					shortsCfg.MaxGrossShortPct, shortsCfg.MaxTickerShortPct)
			}
			stopLoss, takeProfit, eligible := normalizedShortBracketPrices(bid, sig.StopLoss, sig.TakeProfit)
			whole := math.Floor(qty)
			if !eligible || whole < 1 {
				reason := "broker protection requires a compatible whole-share short bracket"
				if eligible && whole < 1 {
					reason = "short exposure caps and buying power leave no whole share to short"
				}
				block(reason, "broker protection")
				return
			}
			sig.StopLoss, sig.TakeProfit = stopLoss, takeProfit
			orderReq.Quantity = whole
			orderReq.OrderClass = "bracket"
			orderReq.StopLossPrice, orderReq.TakeProfitPrice = stopLoss, takeProfit
			orderReq.PolicyStopLoss, orderReq.PolicyTakeProfit = stopLoss, takeProfit
			orderReq.RequireBrokerProtection = true
			orderReq.TimeInForce = "gtc"
		} else {
			stopLoss, takeProfit, eligible := normalizedShortBracketPrices(bid, sig.StopLoss, sig.TakeProfit)
			if !eligible {
				block("prepared short bracket became incompatible with the fresh bid", "broker protection")
				return
			}
			sig.StopLoss, sig.TakeProfit = stopLoss, takeProfit
		}

		riskIn = buildRiskInput(e.cfg.Risk, string(sigActionShort), sig.Ticker,
			sig.Confidence, sizePct, sig.StopLoss, sig.TakeProfit, bid,
			brokerStockRiskState(e.portfolio.CurrentState(), admission), riskOwned)
		if err := mergeBrokerShortOccupancy(&riskIn, admission.BrokerPositions, sig.Ticker, durableOwned); err != nil {
			block(err.Error(), "position_ownership")
			return
		}
		riskIn.ProposedQuantity = orderReq.Quantity
		riskIn.ProposedNotional = orderReq.Quantity * bid
		if riskIn.ProposedNotional > admission.RemainingCapacity+1e-7 {
			block("exact short quantity exceeds available buying power", "buying_power")
			return
		}
		if results, passed := risk.CheckAll(riskIn); !passed {
			block("execution risk re-check failed: "+risk.BlockReason(results), risk.BlockReason(results))
			return
		}

		eligibility := risk.EvaluateShortEligibility(shortEligibilityInput(
			e, admission, riskIn, facts, bid, grossShort, tickerShort, exposureErr, riskIn.ProposedNotional,
		))
		if !eligibility.Eligible {
			block(eligibility.Error(), "short_eligibility")
			return
		}
	}

	if err := validateStockAdmissionFreshness(admission, time.Now().UTC()); err != nil {
		if !admission.BrokerHeldReplay {
			e.queueBrokerAdmissionRefresh(ctx, sigID, sig, signalReferencePrice,
				"broker admission aged before short release controls", err)
			return
		}
		block(err.Error(), "admission_freshness")
		return
	}
	// The stock entry pause blocks every new risk, short or long. A broker-held
	// replay is already live and must still reach its projection.
	if !admission.BrokerHeldReplay {
		if enabled, reason := buyEntriesEnabled(ctx); !enabled {
			block(reason, "buy_control")
			return
		}
		if !shortEntryMarketOpen(time.Now()) {
			block("market closed before order release — order would not fill this session", "market_hours")
			return
		}
	}
	if replayingExisting && !admission.BrokerHeldReplay {
		payload, err := json.Marshal(orderReq)
		if err != nil {
			block("unable to prepare repriced short entry replay", "durable_intent")
			return
		}
		if err := store.RefreshPreparedStockEntryRequest(ctx, intentKey, orderReq.Quantity, payload); err != nil {
			block("unable to durably refresh prepared short entry at the fresh bid", "durable_intent")
			return
		}
	}
	if err := validateFinalStockEntryRelease(admission, false, nil, sig.Ticker, time.Now()); err != nil {
		if !admission.BrokerHeldReplay {
			e.queueBrokerAdmissionRefresh(ctx, sigID, sig, signalReferencePrice,
				"broker admission aged before final short submission", err)
			return
		}
		block(err.Error(), "admission_freshness")
		return
	}
	e.placeStockEntryAndProject(ctx, sigID, sig, orderReq, "short", orderReq.OrderClass == "bracket", false, fromQueue)
}

// shortEntryMarketOpen is the release-time market-hours check. It is a
// variable only so tests can exercise the release path outside trading hours.
var shortEntryMarketOpen = IsMarketOpen

// sigActionShort is the signal action string for a short entry.
const sigActionShort = "SHORT"

// shortEligibilityInput assembles the order-time eligibility facts.
func shortEligibilityInput(
	e *Engine,
	admission stockEntryAdmission,
	riskIn risk.CheckInput,
	facts shortMarketFacts,
	bid, grossShort, tickerShort float64,
	exposureErr error,
	proposedValue float64,
) risk.ShortEligibilityInput {
	account, asset := admission.Account, admission.Asset
	in := risk.ShortEligibilityInput{
		Cfg:                    e.cfg.Risk.Shorts,
		LiveAccount:            !e.cfg.Markets.Stocks.Paper,
		AccountShortingEnabled: account.ShortingEnabled,
		AccountMultiplier:      account.Multiplier,
		AccountBlocked: account.TradingBlocked || account.AccountBlocked ||
			account.TradeSuspendedByUser || !strings.EqualFold(account.Status, "active"),
		AssetActiveTradable: asset.Tradable && strings.EqualFold(asset.Status, "active"),
		Shortable:           asset.Shortable,
		ShortableKnown:      asset.ShortableAvailable,
		EasyToBorrow:        asset.EasyToBorrow,
		EasyToBorrowKnown:   asset.EasyToBorrowAvailable,
		HoldsLong:           riskIn.HoldsLong,
		HoldsShort:          riskIn.HoldsShort,
		Price:               bid,
		PrevClose:           facts.PrevClose,
		SessionLow:          facts.SessionLow,
		SSRCarriedOver:      facts.SSRCarriedOver,
		AvgDollarVolume:     facts.AvgDollarVolume,
		EarningsKnown:       facts.EarningsKnown,
		DaysToEarnings:      facts.DaysToEarnings,
		Equity:              account.Equity,
		GrossShortValue:     grossShort,
		TickerShortValue:    tickerShort,
		ProposedShortValue:  proposedValue,
	}
	if in.SessionLow > 0 && bid > 0 {
		// The current bid is a traded-through price for SSR purposes.
		in.SessionLow = math.Min(in.SessionLow, bid)
	}
	if exposureErr != nil {
		in.GrossShortValue = math.NaN() // unknown exposure fails closed
	}
	return in
}

// shortMarketFacts fetches fresh daily candles and the earnings calendar.
// Every failure leaves the corresponding fact unknown.
func (e *Engine) shortMarketFacts(ctx context.Context, ticker string, now time.Time) shortMarketFacts {
	var facts shortMarketFacts
	if e.marketData != nil {
		candles, err := e.marketData.FetchHistory(ctx, ticker, data.Timeframe1Day, 30)
		if err != nil {
			logger.Warn("short eligibility: daily candles unavailable", "ticker", ticker, "err", err)
		} else {
			facts = shortFactsFromDailyCandles(candles, now, e.cfg.Risk.Shorts.SSRDropPct)
		}
	}
	if store.IsConnected() {
		next, found, err := store.NextEarningsOnOrAfter(ctx, store.GetDB(), ticker, now)
		switch {
		case err != nil:
			logger.Warn("short eligibility: earnings calendar unavailable", "ticker", ticker, "err", err)
		case found:
			facts.EarningsKnown = true
			days := calendarDaysUntil(now, next)
			facts.DaysToEarnings = &days
		default:
			facts.EarningsKnown = true
		}
	}
	return facts
}

// shortFactsFromDailyCandles derives SSR and liquidity facts from daily bars.
// Today's partial bar supplies the session low; the prior completed bar the
// previous close; and the bar before it decides whether yesterday triggered a
// restriction that carries over to today.
func shortFactsFromDailyCandles(candles []data.Candle, now time.Time, ssrDropPct float64) shortMarketFacts {
	var facts shortMarketFacts
	bars := make([]data.Candle, 0, len(candles))
	for _, c := range candles {
		if c.Close > 0 && c.Low > 0 && !c.Timestamp.IsZero() {
			bars = append(bars, c)
		}
	}
	sort.Slice(bars, func(i, j int) bool { return bars[i].Timestamp.Before(bars[j].Timestamp) })
	today := now.In(nyLoc).Format("2006-01-02")
	var todayBar *data.Candle
	var prior []data.Candle
	for i := range bars {
		day := bars[i].Timestamp.In(nyLoc).Format("2006-01-02")
		switch {
		case day == today:
			todayBar = &bars[i]
		case day < today:
			prior = append(prior, bars[i])
		}
	}
	if todayBar == nil || len(prior) == 0 {
		return facts // session low or previous close unknown → SSR unknown
	}
	prev := prior[len(prior)-1]
	facts.PrevClose = prev.Close
	facts.SessionLow = todayBar.Low
	if len(prior) >= 2 && ssrDropPct > 0 {
		before := prior[len(prior)-2]
		facts.SSRCarriedOver = prev.Low <= before.Close*(1-ssrDropPct/100)
	}
	window := prior
	if len(window) > 20 {
		window = window[len(window)-20:]
	}
	var dollarVolume float64
	for _, c := range window {
		dollarVolume += c.Close * float64(c.Volume)
	}
	if len(window) >= 10 {
		// Fewer than ten completed sessions is not a liquidity history.
		facts.AvgDollarVolume = dollarVolume / float64(len(window))
	}
	return facts
}

func calendarDaysUntil(now, date time.Time) int {
	n := now.In(nyLoc)
	start := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)
	d := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, time.UTC)
	return int(d.Sub(start).Hours() / 24)
}

// shortExposure returns the current gross and per-ticker short market value:
// the larger of broker truth and the local projection, plus pending short
// reservations other than excludeKey. A malformed broker position is an
// error, which the caller treats as unknown exposure.
func shortExposure(
	positions []execution.BrokerPosition,
	owned []store.Trade,
	reservations []store.ActiveStockEntryReservation,
	excludeKey, ticker string,
) (gross, tickerValue float64, err error) {
	var brokerGross, brokerTicker float64
	for _, p := range positions {
		if !strings.EqualFold(p.Side, "short") {
			continue
		}
		value := math.Abs(p.MarketValue)
		if !finitePositive(value) {
			value = math.Abs(p.Quantity) * p.CurrentPrice
		}
		if !finitePositive(value) {
			return 0, 0, errorsForAdmission("broker short position has no market value")
		}
		brokerGross += value
		if strings.EqualFold(p.Symbol, ticker) {
			brokerTicker += value
		}
	}
	var localGross, localTicker float64
	for _, t := range owned {
		if t.Direction != "short" || t.Quantity <= 0 {
			continue
		}
		value := t.Quantity * t.EntryPrice
		localGross += value
		if strings.EqualFold(t.Ticker, ticker) {
			localTicker += value
		}
	}
	var pendingGross, pendingTicker float64
	for _, r := range reservations {
		if r.IdempotencyKey == excludeKey || reservationSide(r) != string(execution.OrderSideSell) || r.RemainingQty <= 0 {
			continue
		}
		value := r.RemainingQty * r.ReferencePrice
		pendingGross += value
		if strings.EqualFold(r.Ticker, ticker) {
			pendingTicker += value
		}
	}
	return math.Max(brokerGross, localGross) + pendingGross,
		math.Max(brokerTicker, localTicker) + pendingTicker, nil
}

// capShortQuantity shrinks a short to the remaining gross and per-ticker caps.
// It never grows the quantity, and returns it unchanged when equity or price
// is unusable (eligibility then blocks on the unknown input).
func capShortQuantity(qty, price, equity, gross, ticker, maxGrossPct, maxTickerPct float64) float64 {
	if !finitePositive(price) || !finitePositive(equity) {
		return qty
	}
	room := math.Min(equity*maxGrossPct/100-gross, equity*maxTickerPct/100-ticker)
	if !(room > 0) {
		return 0
	}
	return math.Min(qty, math.Floor(room/price))
}

// mergeBrokerShortOccupancy is the short-side counterpart of
// mergeBrokerLongOccupancy: broker truth must show the ticker flat (no flips,
// no adding), and every broker position not already represented locally
// consumes a position slot.
func mergeBrokerShortOccupancy(in *risk.CheckInput, positions []execution.BrokerPosition, target string, durableOwned []store.Trade) error {
	if in == nil {
		return nil
	}
	if in.OpenByTicker == nil {
		in.OpenByTicker = make(map[string]int)
	}
	represented := make(map[string]struct{})
	for _, t := range durableOwned {
		if t.Quantity > 0 {
			represented[strings.ToUpper(strings.TrimSpace(t.Ticker))+"|"+t.Direction] = struct{}{}
		}
	}
	for _, p := range positions {
		symbol := strings.TrimSpace(p.Symbol)
		if symbol == "" || !finite(p.Quantity) || math.Abs(p.Quantity) <= 1e-9 {
			return errorsForAdmission("broker position is malformed for capacity check")
		}
		direction := strings.ToLower(strings.TrimSpace(p.Side))
		if direction != "long" && direction != "short" {
			return errorsForAdmission("broker position has invalid side for capacity check")
		}
		if strings.EqualFold(symbol, target) {
			if direction == "long" {
				in.HoldsLong = true
			} else {
				in.HoldsShort = true
			}
			return errorsForAdmission(fmt.Sprintf("broker holds a %s position in the requested SHORT ticker", direction))
		}
		if _, ok := represented[strings.ToUpper(symbol)+"|"+direction]; !ok {
			in.OpenPositions++
		}
	}
	return nil
}

// normalizedShortBracketPrices is normalizedStockBracketPrices for a short:
// after Alpaca's price increment is applied the stop must sit above entry and
// the positive target below it.
func normalizedShortBracketPrices(signalPrice, stopLoss, takeProfit float64) (float64, float64, bool) {
	if !finitePositive(signalPrice) || !finitePositive(stopLoss) || !finitePositive(takeProfit) {
		return 0, 0, false
	}
	stopLoss = roundStockOrderPrice(stopLoss)
	takeProfit = roundStockOrderPrice(takeProfit)
	if !finitePositive(stopLoss) || !finitePositive(takeProfit) ||
		stopLoss <= signalPrice || takeProfit >= signalPrice || takeProfit >= stopLoss {
		return 0, 0, false
	}
	return stopLoss, takeProfit, true
}

// tickerHasExitingTradeIn reports an in-flight exit for ticker in either
// direction.
func tickerHasExitingTradeIn(trades []store.Trade, ticker string) bool {
	for _, t := range trades {
		if strings.EqualFold(t.Ticker, ticker) && t.Status == "exiting" {
			return true
		}
	}
	return false
}
