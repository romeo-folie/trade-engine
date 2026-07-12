package options

import (
	"math"
	"testing"
)

func TestBSPrice_KnownATMValue(t *testing.T) {
	// ATM call, S=K=100, T=1, r=q=0, sigma=0.20. Closed-form ≈ 7.9656.
	got := BSPrice("C", 100, 100, 1, 0, 0, 0.20)
	if math.Abs(got-7.9656) > 0.01 {
		t.Errorf("BSPrice ATM call = %.4f; want ≈7.9656", got)
	}
	// Put-call parity at r=q=0, ATM: call price == put price.
	put := BSPrice("P", 100, 100, 1, 0, 0, 0.20)
	if math.Abs(got-put) > 1e-6 {
		t.Errorf("ATM call %.4f != put %.4f (parity at r=q=0)", got, put)
	}
}

func TestBSPrice_DegenerateIsIntrinsic(t *testing.T) {
	// Expired ITM call -> intrinsic.
	if got := BSPrice("C", 110, 100, 0, 0, 0, 0.2); math.Abs(got-10) > 1e-9 {
		t.Errorf("expired ITM call = %v; want 10", got)
	}
	// Zero vol OTM put -> 0.
	if got := BSPrice("P", 110, 100, 0.1, 0, 0, 0); got != 0 {
		t.Errorf("zero-vol OTM put = %v; want 0", got)
	}
}

func TestImpliedVol_RoundTrip(t *testing.T) {
	for _, tc := range []struct {
		right          string
		s, k, t, sigma float64
	}{
		{"C", 100, 100, 0.05, 0.20},
		{"C", 550, 551, 4.0 / 365, 0.18},
		{"P", 120, 118, 0.02, 0.45},
		{"C", 100, 90, 0.25, 0.30}, // ITM call
		{"P", 100, 130, 0.5, 0.25}, // deep ITM put (k > s): exercises the put upper bound
		{"P", 100, 130, 1.0, 0.10}, // low-vol ITM put, near intrinsic
	} {
		price := BSPrice(tc.right, tc.s, tc.k, tc.t, 0.04, 0, tc.sigma)
		iv, ok := ImpliedVol(tc.right, price, tc.s, tc.k, tc.t, 0.04, 0)
		if !ok {
			t.Errorf("ImpliedVol(%+v) not ok", tc)
			continue
		}
		if math.Abs(iv-tc.sigma) > 1e-3 {
			t.Errorf("ImpliedVol(%+v) = %.4f; want ≈%.4f", tc, iv, tc.sigma)
		}
	}
}

func TestImpliedVol_NonInvertible(t *testing.T) {
	// Price above spot is impossible for a call -> not invertible.
	if _, ok := ImpliedVol("C", 105, 100, 100, 0.05, 0.04, 0); ok {
		t.Error("expected non-invertible for price > spot")
	}
	// Below intrinsic -> not invertible.
	if _, ok := ImpliedVol("C", 1, 120, 100, 0.05, 0.04, 0); ok {
		t.Error("expected non-invertible for price below intrinsic")
	}
	// Degenerate inputs.
	if _, ok := ImpliedVol("C", 5, 100, 100, 0, 0.04, 0); ok {
		t.Error("expected non-invertible for t<=0")
	}
}
