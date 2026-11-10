package risk

import (
	"fmt"
	"math"
	"strings"

	"github.com/romeo-folie/trade-engine/internal/config"
)

// Short-eligibility reason codes (issue #599). Each is recorded verbatim
// when a short is refused, so blocks are auditable and countable.
const (
	ShortReasonDisabled         = "shorts_disabled"
	ShortReasonLiveNotAllowed   = "shorts_live_not_allowed"
	ShortReasonAccountNoShort   = "account_shorting_disabled"
	ShortReasonAccountNoMargin  = "account_not_margin"
	ShortReasonAccountBlocked   = "account_blocked"
	ShortReasonAssetInactive    = "asset_not_active_tradable"
	ShortReasonNotShortable     = "asset_not_shortable"
	ShortReasonNotEasyToBorrow  = "asset_not_easy_to_borrow"
	ShortReasonBorrowUnknown    = "asset_borrow_status_unknown"
	ShortReasonHoldsLong        = "long_position_held"
	ShortReasonHoldsShort       = "short_position_held"
	ShortReasonPriceUnknown     = "price_unknown"
	ShortReasonBelowMinPrice    = "below_min_price"
	ShortReasonLiquidityUnknown = "liquidity_unknown"
	ShortReasonIlliquid         = "below_min_dollar_volume"
	ShortReasonSSRUnknown       = "ssr_status_unknown"
	ShortReasonSSRActive        = "short_sale_restriction_active"
	ShortReasonEarnings         = "earnings_blackout"
	ShortReasonEarningsUnknown  = "earnings_calendar_unknown"
	ShortReasonGrossCap         = "gross_short_exposure_cap"
	ShortReasonTickerCap        = "ticker_short_exposure_cap"
	ShortReasonEquityUnknown    = "equity_unknown"
)

// ShortEligibilityInput is every fact a short decision depends on, observed
// fresh at decision time and again at order time. Unknown facts are
// expressed explicitly (the *Known flags / nil pointers) so that missing
// data blocks the short instead of being read as a permissive zero.
type ShortEligibilityInput struct {
	Cfg         config.ShortsConfig
	LiveAccount bool

	AccountShortingEnabled bool
	AccountMultiplier      float64
	AccountBlocked         bool // trading or account blocked, or trading suspended

	AssetActiveTradable bool
	Shortable           bool
	ShortableKnown      bool
	EasyToBorrow        bool
	EasyToBorrowKnown   bool

	HoldsLong  bool
	HoldsShort bool

	Price float64 // current executable reference price
	// PrevClose and SessionLow detect the SEC Rule 201 short-sale
	// restriction: it applies for the rest of the day (and the next) once
	// price falls SSRDropPct below the prior close. A zero means unknown.
	PrevClose  float64
	SessionLow float64
	// SSRCarriedOver is true when the restriction was triggered on the prior
	// session and therefore still applies today.
	SSRCarriedOver bool

	AvgDollarVolume float64 // 20-session average; 0 = unknown

	// EarningsKnown reports that the earnings calendar was consulted
	// successfully for this ticker. Only then does a nil DaysToEarnings mean
	// "nothing scheduled"; an unavailable calendar blocks the short.
	EarningsKnown bool
	// DaysToEarnings is calendar days until the next scheduled earnings date
	// (0 = today); nil when the (known) calendar has no upcoming date.
	DaysToEarnings *int

	Equity             float64
	GrossShortValue    float64 // current total short market value (absolute)
	TickerShortValue   float64 // current short market value in this ticker (absolute)
	ProposedShortValue float64 // market value of the short being considered
}

// ShortEligibility is the outcome. A short may proceed only when Eligible;
// Reasons lists every failed rule (not just the first) for audit.
type ShortEligibility struct {
	Eligible bool
	Reasons  []string
}

// Error returns the refusal as a single message, or "" when eligible.
func (e ShortEligibility) Error() string {
	if e.Eligible {
		return ""
	}
	return "short not eligible: " + strings.Join(e.Reasons, ", ")
}

func validPositive(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v > 0 }

// EvaluateShortEligibility applies every short rule. It is pure and
// deterministic: identical inputs always yield identical results.
func EvaluateShortEligibility(in ShortEligibilityInput) ShortEligibility {
	var reasons []string
	add := func(r string) { reasons = append(reasons, r) }
	cfg := in.Cfg

	if !cfg.Enabled {
		return ShortEligibility{Reasons: []string{ShortReasonDisabled}}
	}
	if in.LiveAccount && !cfg.AllowLive {
		add(ShortReasonLiveNotAllowed)
	}

	// Account: the broker must permit shorting on a margin account.
	if !in.AccountShortingEnabled {
		add(ShortReasonAccountNoShort)
	}
	if !(in.AccountMultiplier > 1) {
		add(ShortReasonAccountNoMargin)
	}
	if in.AccountBlocked {
		add(ShortReasonAccountBlocked)
	}

	// Asset: active, shortable, and easy to borrow. Unknown borrow status is
	// never assumed to be favourable.
	if !in.AssetActiveTradable {
		add(ShortReasonAssetInactive)
	}
	if !in.ShortableKnown || !in.EasyToBorrowKnown {
		add(ShortReasonBorrowUnknown)
	} else {
		if !in.Shortable {
			add(ShortReasonNotShortable)
		}
		if !in.EasyToBorrow {
			add(ShortReasonNotEasyToBorrow)
		}
	}

	// Positions: a short opens only from flat. No flips, no adding.
	if in.HoldsLong {
		add(ShortReasonHoldsLong)
	}
	if in.HoldsShort {
		add(ShortReasonHoldsShort)
	}

	// Price and liquidity floors.
	if !validPositive(in.Price) {
		add(ShortReasonPriceUnknown)
	} else if in.Price < cfg.MinPrice {
		add(ShortReasonBelowMinPrice)
	}
	if cfg.MinAvgDollarVolume > 0 {
		if !validPositive(in.AvgDollarVolume) {
			add(ShortReasonLiquidityUnknown)
		} else if in.AvgDollarVolume < cfg.MinAvgDollarVolume {
			add(ShortReasonIlliquid)
		}
	}

	// SEC Rule 201: once a stock falls SSRDropPct below the prior close, new
	// shorts are restricted for the rest of that day and the next.
	switch {
	case in.SSRCarriedOver:
		add(ShortReasonSSRActive)
	case !validPositive(in.PrevClose) || !validPositive(in.SessionLow):
		add(ShortReasonSSRUnknown)
	case in.SessionLow <= in.PrevClose*(1-cfg.SSRDropPct/100):
		add(ShortReasonSSRActive)
	}

	// Earnings: no new short inside the blackout window before a scheduled
	// date. A calendar that could not be consulted is not evidence of "no
	// earnings" and blocks the short.
	switch {
	case !in.EarningsKnown:
		add(ShortReasonEarningsUnknown)
	case in.DaysToEarnings != nil && *in.DaysToEarnings >= 0 && *in.DaysToEarnings <= cfg.EarningsBlackoutDays:
		add(ShortReasonEarnings)
	}

	// Exposure caps, measured against equity including the proposed short.
	if !validPositive(in.Equity) {
		add(ShortReasonEquityUnknown)
	} else {
		proposed := math.Abs(in.ProposedShortValue)
		if (math.Abs(in.GrossShortValue)+proposed)/in.Equity*100 > cfg.MaxGrossShortPct {
			add(ShortReasonGrossCap)
		}
		if (math.Abs(in.TickerShortValue)+proposed)/in.Equity*100 > cfg.MaxTickerShortPct {
			add(ShortReasonTickerCap)
		}
	}

	return ShortEligibility{Eligible: len(reasons) == 0, Reasons: reasons}
}

// DescribeShortReasons renders reason codes for operator-facing messages.
func DescribeShortReasons(reasons []string) string {
	if len(reasons) == 0 {
		return ""
	}
	return fmt.Sprintf("%d short rule(s) failed: %s", len(reasons), strings.Join(reasons, ", "))
}
