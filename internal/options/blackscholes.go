package options

import "math"

// Black-Scholes pricing and implied-volatility inversion. Alpaca does not serve
// historical greeks/IV (only current snapshots), so the bounded up-front IV
// history (issue #413) is derived by inverting historical option prices through
// this solver. Forward-accumulated IV, by contrast, comes straight from the live
// chain snapshot and needs no inversion.

// normCDF is the standard-normal cumulative distribution function.
func normCDF(x float64) float64 {
	return 0.5 * math.Erfc(-x/math.Sqrt2)
}

// BSPrice returns the Black-Scholes price of a European option.
//
//	right: "C" or "P"; s: spot; k: strike; t: years to expiry;
//	r: risk-free rate; q: dividend yield; sigma: volatility.
//
// Degenerate inputs (t<=0 or sigma<=0) collapse to discounted intrinsic value.
func BSPrice(right string, s, k, t, r, q, sigma float64) float64 {
	call := right == "C" || right == "c"
	if t <= 0 || sigma <= 0 {
		fwd := s*math.Exp(-q*t) - k*math.Exp(-r*t)
		if call {
			return math.Max(0, fwd)
		}
		return math.Max(0, -fwd)
	}
	sqrtT := math.Sqrt(t)
	d1 := (math.Log(s/k) + (r-q+0.5*sigma*sigma)*t) / (sigma * sqrtT)
	d2 := d1 - sigma*sqrtT
	if call {
		return s*math.Exp(-q*t)*normCDF(d1) - k*math.Exp(-r*t)*normCDF(d2)
	}
	return k*math.Exp(-r*t)*normCDF(-d2) - s*math.Exp(-q*t)*normCDF(-d1)
}

// ImpliedVol inverts BSPrice for sigma via bisection. It returns (iv, true) on
// convergence and (0, false) when the price is outside the no-arbitrage band or
// the inputs are degenerate — callers treat !ok as "IV unavailable" and, in the
// fail-closed pipeline, do not trade. Bisection (not Newton) is used for
// unconditional robustness: vega collapses to ~0 for deep ITM/OTM contracts and
// would make Newton diverge exactly on the noisy contracts we most want to reject.
func ImpliedVol(right string, price, s, k, t, r, q float64) (float64, bool) {
	if price <= 0 || s <= 0 || k <= 0 || t <= 0 {
		return 0, false
	}
	// Price must sit within the no-arbitrage band to be invertible. The upper bound
	// differs by right: a call is bounded by the discounted spot, a put by the
	// discounted strike. (Using the call bound for puts wrongly rejects valid ITM
	// put prices where k > s.)
	call := right == "C" || right == "c"
	upper := s * math.Exp(-q*t)
	if !call {
		upper = k * math.Exp(-r*t)
	}
	intrinsic := BSPrice(right, s, k, t, r, q, 0)
	if price < intrinsic-1e-9 || price > upper+1e-9 {
		return 0, false
	}
	lo, hi := 1e-4, 5.0 // 0.01%–500% vol brackets any real equity-option IV
	if BSPrice(right, s, k, t, r, q, hi) < price {
		return 0, false // price implies vol above the bracket — reject rather than clamp
	}
	for range 100 {
		mid := (lo + hi) / 2
		p := BSPrice(right, s, k, t, r, q, mid)
		if math.Abs(p-price) < 1e-6 {
			return mid, true
		}
		if p < price {
			lo = mid
		} else {
			hi = mid
		}
	}
	iv := (lo + hi) / 2
	return iv, true
}
