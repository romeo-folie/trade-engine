package risk

import (
	"math"
	"reflect"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/config"
)

func ip(v int) *int { return &v }

func eligibleShortInput() ShortEligibilityInput {
	return ShortEligibilityInput{
		Cfg: config.ShortsConfig{
			Enabled: true, SizePct: 1.5, MaxGrossShortPct: 20, MaxTickerShortPct: 5,
			MinPrice: 5, MinAvgDollarVolume: 20_000_000, EarningsBlackoutDays: 3, SSRDropPct: 10,
		},
		AccountShortingEnabled: true, AccountMultiplier: 2,
		AssetActiveTradable: true, Shortable: true, ShortableKnown: true, EasyToBorrow: true, EasyToBorrowKnown: true,
		Price: 50, PrevClose: 52, SessionLow: 49.5,
		AvgDollarVolume: 80_000_000,
		EarningsKnown:   true,
		DaysToEarnings:  ip(20),
		Equity:          2500, GrossShortValue: 0, TickerShortValue: 0, ProposedShortValue: 37.5,
	}
}

func TestShortEligibleWhenEveryRulePasses(t *testing.T) {
	got := EvaluateShortEligibility(eligibleShortInput())
	if !got.Eligible || len(got.Reasons) != 0 || got.Error() != "" {
		t.Fatalf("baseline should be eligible: %+v", got)
	}
}

func TestShortDisabledShortCircuits(t *testing.T) {
	in := eligibleShortInput()
	in.Cfg.Enabled = false
	got := EvaluateShortEligibility(in)
	if got.Eligible || !reflect.DeepEqual(got.Reasons, []string{ShortReasonDisabled}) {
		t.Fatalf("disabled: %+v", got)
	}
}

func TestShortEachRuleBlocks(t *testing.T) {
	cases := map[string]struct {
		mutate func(*ShortEligibilityInput)
		reason string
	}{
		"live without opt-in":     {func(in *ShortEligibilityInput) { in.LiveAccount = true }, ShortReasonLiveNotAllowed},
		"account shorting off":    {func(in *ShortEligibilityInput) { in.AccountShortingEnabled = false }, ShortReasonAccountNoShort},
		"cash account":            {func(in *ShortEligibilityInput) { in.AccountMultiplier = 1 }, ShortReasonAccountNoMargin},
		"account blocked":         {func(in *ShortEligibilityInput) { in.AccountBlocked = true }, ShortReasonAccountBlocked},
		"asset inactive":          {func(in *ShortEligibilityInput) { in.AssetActiveTradable = false }, ShortReasonAssetInactive},
		"not shortable":           {func(in *ShortEligibilityInput) { in.Shortable = false }, ShortReasonNotShortable},
		"hard to borrow":          {func(in *ShortEligibilityInput) { in.EasyToBorrow = false }, ShortReasonNotEasyToBorrow},
		"borrow unknown":          {func(in *ShortEligibilityInput) { in.EasyToBorrowKnown = false }, ShortReasonBorrowUnknown},
		"shortable unknown":       {func(in *ShortEligibilityInput) { in.ShortableKnown = false }, ShortReasonBorrowUnknown},
		"long held (no flip)":     {func(in *ShortEligibilityInput) { in.HoldsLong = true }, ShortReasonHoldsLong},
		"short held (no adding)":  {func(in *ShortEligibilityInput) { in.HoldsShort = true }, ShortReasonHoldsShort},
		"price unknown":           {func(in *ShortEligibilityInput) { in.Price = 0 }, ShortReasonPriceUnknown},
		"price NaN":               {func(in *ShortEligibilityInput) { in.Price = math.NaN() }, ShortReasonPriceUnknown},
		"below min price":         {func(in *ShortEligibilityInput) { in.Price = 4.99 }, ShortReasonBelowMinPrice},
		"liquidity unknown":       {func(in *ShortEligibilityInput) { in.AvgDollarVolume = 0 }, ShortReasonLiquidityUnknown},
		"illiquid":                {func(in *ShortEligibilityInput) { in.AvgDollarVolume = 19_999_999 }, ShortReasonIlliquid},
		"ssr unknown":             {func(in *ShortEligibilityInput) { in.PrevClose = 0 }, ShortReasonSSRUnknown},
		"ssr triggered today":     {func(in *ShortEligibilityInput) { in.SessionLow = 46.8 }, ShortReasonSSRActive}, // 52 * 0.9 = 46.8
		"ssr carried over":        {func(in *ShortEligibilityInput) { in.SSRCarriedOver = true }, ShortReasonSSRActive},
		"earnings today":          {func(in *ShortEligibilityInput) { in.DaysToEarnings = ip(0) }, ShortReasonEarnings},
		"earnings at window edge": {func(in *ShortEligibilityInput) { in.DaysToEarnings = ip(3) }, ShortReasonEarnings},
		"earnings calendar unknown": {func(in *ShortEligibilityInput) {
			in.EarningsKnown, in.DaysToEarnings = false, nil
		}, ShortReasonEarningsUnknown},
		"equity unknown": {func(in *ShortEligibilityInput) { in.Equity = 0 }, ShortReasonEquityUnknown},
		"gross cap":      {func(in *ShortEligibilityInput) { in.GrossShortValue = 480 }, ShortReasonGrossCap},
		"ticker cap": {func(in *ShortEligibilityInput) {
			in.TickerShortValue = 100 // 100 + 37.5 = 137.5 > 5% of 2500 (125)
		}, ShortReasonTickerCap},
	}
	for name, tc := range cases {
		in := eligibleShortInput()
		tc.mutate(&in)
		got := EvaluateShortEligibility(in)
		if got.Eligible {
			t.Errorf("%s: eligible, want blocked by %s", name, tc.reason)
			continue
		}
		found := false
		for _, r := range got.Reasons {
			if r == tc.reason {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: reasons %v lack %s", name, got.Reasons, tc.reason)
		}
	}
}

func TestShortBoundariesThatShouldPass(t *testing.T) {
	cases := map[string]func(*ShortEligibilityInput){
		"exactly min price":           func(in *ShortEligibilityInput) { in.Price = 5 },
		"exactly min dollar volume":   func(in *ShortEligibilityInput) { in.AvgDollarVolume = 20_000_000 },
		"just above ssr trigger":      func(in *ShortEligibilityInput) { in.SessionLow = 46.81 },
		"earnings just outside":       func(in *ShortEligibilityInput) { in.DaysToEarnings = ip(4) },
		"earnings in the past":        func(in *ShortEligibilityInput) { in.DaysToEarnings = ip(-1) },
		"no earnings scheduled":       func(in *ShortEligibilityInput) { in.DaysToEarnings = nil },
		"exactly at ticker cap":       func(in *ShortEligibilityInput) { in.TickerShortValue = 87.5 }, // 87.5+37.5 = 125 = 5%
		"live with explicit opt-in":   func(in *ShortEligibilityInput) { in.LiveAccount, in.Cfg.AllowLive = true, true },
		"liquidity floor disabled":    func(in *ShortEligibilityInput) { in.Cfg.MinAvgDollarVolume, in.AvgDollarVolume = 0, 0 },
		"negative short values (abs)": func(in *ShortEligibilityInput) { in.GrossShortValue = -100 },
	}
	for name, mutate := range cases {
		in := eligibleShortInput()
		mutate(&in)
		if got := EvaluateShortEligibility(in); !got.Eligible {
			t.Errorf("%s: blocked by %v, want eligible", name, got.Reasons)
		}
	}
}

func TestShortReportsEveryFailedRule(t *testing.T) {
	in := eligibleShortInput()
	in.AccountShortingEnabled, in.HoldsLong, in.Price = false, true, 3
	got := EvaluateShortEligibility(in)
	want := []string{ShortReasonAccountNoShort, ShortReasonHoldsLong, ShortReasonBelowMinPrice}
	if !reflect.DeepEqual(got.Reasons, want) {
		t.Fatalf("reasons = %v, want %v", got.Reasons, want)
	}
	if got.Error() == "" || DescribeShortReasons(got.Reasons) == "" {
		t.Fatal("refusal must be describable")
	}
}

func TestShortEligibilityIsDeterministic(t *testing.T) {
	in := eligibleShortInput()
	in.SessionLow = 40
	first := EvaluateShortEligibility(in)
	for i := 0; i < 20; i++ {
		if got := EvaluateShortEligibility(in); !reflect.DeepEqual(got, first) {
			t.Fatal("same input produced a different eligibility")
		}
	}
}

// TestShortExposureMustBeFinite: NaN compares false, so an unvalidated
// non-finite exposure would silently pass both caps.
func TestShortExposureMustBeFinite(t *testing.T) {
	for name, mutate := range map[string]func(*ShortEligibilityInput){
		"proposed NaN":  func(in *ShortEligibilityInput) { in.ProposedShortValue = math.NaN() },
		"proposed zero": func(in *ShortEligibilityInput) { in.ProposedShortValue = 0 },
		"proposed +Inf": func(in *ShortEligibilityInput) { in.ProposedShortValue = math.Inf(1) },
		"gross NaN":     func(in *ShortEligibilityInput) { in.GrossShortValue = math.NaN() },
		"ticker -Inf":   func(in *ShortEligibilityInput) { in.TickerShortValue = math.Inf(-1) },
	} {
		in := eligibleShortInput()
		mutate(&in)
		got := EvaluateShortEligibility(in)
		if got.Eligible || !reflect.DeepEqual(got.Reasons, []string{ShortReasonExposureUnknown}) {
			t.Errorf("%s: %+v, want blocked by %s only", name, got, ShortReasonExposureUnknown)
		}
	}
}
