package feedback

import (
	"testing"

	"github.com/romeo-folie/trade-engine/internal/config"
)

func TestORBAutoThrottleMultiplier(t *testing.T) {
	// `active` mirrors the production Defaults() (sensitivity 1.0); the in-fn defaults supply
	// minTrades 20 / floor 0.5 / cap 1.5 when those are left at 0. A zero-value config (no
	// explicit sensitivity) means "flat" — covered separately below.
	active := config.ORBAutoThrottleConfig{Sensitivity: 1.0}

	cases := []struct {
		name      string
		avgPnlPct float64
		trades    int
		cfg       config.ORBAutoThrottleConfig
		want      float64
	}{
		{"below min trades → neutral", 0.9, 19, active, 1.0},
		{"profitable hits cap", 0.5, 50, active, 1.5},       // 1 + 1*0.5 = 1.5 (= cap)
		{"mild profit", 0.2, 50, active, 1.2},               // 1 + 1*0.2
		{"flat → neutral", 0.0, 50, active, 1.0},            // 1 + 0
		{"losing hits floor", -0.5, 50, active, 0.5},        // 1 - 0.5 = 0.5 (= floor)
		{"big win clamps to cap", 3.0, 50, active, 1.5},     // 1 + 3 → clamp 1.5
		{"big loss clamps to floor", -3.0, 50, active, 0.5}, // 1 - 3 → clamp 0.5
		{
			"custom params", 0.4, 12,
			config.ORBAutoThrottleConfig{MinTrades: 10, Sensitivity: 0.5, Floor: 0.3, Cap: 2.0},
			1.2, // 1 + 0.5*0.4 = 1.2
		},
		{
			"custom min gate not met", 0.4, 9,
			config.ORBAutoThrottleConfig{MinTrades: 10, Sensitivity: 0.5, Floor: 0.3, Cap: 2.0},
			1.0,
		},
		{
			// An explicit sensitivity:0 is a valid "flat — no performance scaling" choice and
			// must be HONOURED (always neutral), not clobbered to full sensitivity. A losing
			// instrument must stay at 1.0, not get sized down.
			"explicit zero sensitivity stays flat", -0.5, 50,
			config.ORBAutoThrottleConfig{Sensitivity: 0},
			1.0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := orbAutoThrottleMultiplier(c.avgPnlPct, c.trades, c.cfg); got != c.want {
				t.Errorf("orbAutoThrottleMultiplier(%.2f, %d) = %v; want %v", c.avgPnlPct, c.trades, got, c.want)
			}
		})
	}
}
