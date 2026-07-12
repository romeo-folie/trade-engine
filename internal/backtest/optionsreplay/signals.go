package optionsreplay

import (
	"fmt"
	"sort"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/orb"
)

// etLoc is US/Eastern for session-time gates; falls back to a fixed -05:00 offset if
// the zoneinfo database is unavailable (matches the orb package's approach).
var etLoc = func() *time.Location {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		return time.FixedZone("EST", -5*60*60)
	}
	return loc
}()

// AssembleConfig parameterizes how underlying 1-minute bars become breakout signals.
// The breakout/opening-range/bracket math is the LIVE ORB logic (orb.NewRangeBuffer,
// orb.ComputeBracket) so the underlying stop/target the options gate acts on are
// identical to production for the same bars.
type AssembleConfig struct {
	RangeMinutes    int     // opening-range length in minutes (e.g. 15)
	EntryWindowMins int     // minutes after SESSION OPEN that a breakout may still fire (matches the live scheduler's cleanup cutoff = open + 60m); NOT measured from the range lock
	SessionOpenET   string  // "HH:MM" ET regular-session open; default "09:30"
	SessionEndET    string  // "HH:MM" ET hard flatten; default = orb.OptionsFlattenCutoffET() (single-sourced with live)
	RiskReward      float64 // target = entry ± risk × RR (feeds orb.ComputeBracket)
}

func (c AssembleConfig) withDefaults() AssembleConfig {
	if c.RangeMinutes <= 0 {
		c.RangeMinutes = 15
	}
	if c.EntryWindowMins <= 0 {
		c.EntryWindowMins = 60
	}
	if c.SessionOpenET == "" {
		c.SessionOpenET = "09:30"
	}
	if c.SessionEndET == "" {
		// Single-sourced from the LIVE flatten cutoff so backtest EOD parity can't drift.
		c.SessionEndET = orb.OptionsFlattenCutoffET()
	}
	if c.RiskReward <= 0 {
		c.RiskReward = 2
	}
	return c
}

// AssembleSignals turns a stream of underlying 1-minute candles into at most one
// breakout Signal PER ET TRADING DAY (options-ORB is intraday, first breakout only).
// The returned Signals carry the underlying stop/target and the underlying-price
// Path from the breakout to the session-end flatten; the OptionMid on each Path bar
// is left zero for the runner to fill once the gate has chosen a contract.
func AssembleSignals(underlying string, candles []data.Candle, cfg AssembleConfig) []Signal {
	cfg = cfg.withDefaults()
	openMin := parseHHMM(cfg.SessionOpenET, 9*60+30)
	endMin := parseHHMM(cfg.SessionEndET, 15*60+50)

	byDay := groupByETDay(candles)
	days := make([]time.Time, 0, len(byDay))
	for d := range byDay {
		days = append(days, d)
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Before(days[j]) })

	orbCfg := config.ORBConfig{RiskReward: cfg.RiskReward}
	var out []Signal
	for _, day := range days {
		bars := rthBars(byDay[day], openMin, endMin)
		if len(bars) < cfg.RangeMinutes+1 {
			continue // not enough of a session to form a range and a breakout
		}
		// Build the session bounds as WALL-CLOCK ET times, not midnight+duration: on a
		// DST-transition day the latter drifts an hour (spring-forward would put the EOD
		// flatten past the last kept bar so it never fires; fall-back would flatten an
		// hour early). This matches rthBars, which filters by wall-clock minute.
		sessionEnd := etTimeOnDay(day, endMin)

		// Opening range from the first RangeMinutes bars (live RangeBuffer).
		rb := orb.NewRangeBuffer(underlying, cfg.RangeMinutes)
		var locked bool
		var rangeEndIdx int
		for i, c := range bars {
			done, _ := rb.Add(c)
			if done {
				locked = true
				rangeEndIdx = i
				break
			}
		}
		or := rb.Range()
		if !locked || or == nil {
			continue
		}
		// Near-flat range → skip (mirrors the live/orbreplay degenerate-range guard).
		if or.High-or.Low <= 0 {
			continue
		}

		buy := orb.ComputeBracket(or, "buy", 1, orbCfg)
		sell := orb.ComputeBracket(or, "sell", 1, orbCfg)

		watch := bars[rangeEndIdx+1:]
		// Deadline is SESSION OPEN + EntryWindowMins, matching the live scheduler's
		// entry-cancel cutoff (sessionCleanupMinutes = open + 60m). Measuring from the
		// range lock instead would keep accepting breakouts for RangeMinutes longer than
		// production would — replaying trades the live path had already cancelled.
		sessionOpenTime := etTimeOnDay(day, openMin)
		entryDeadline := sessionOpenTime.Add(time.Duration(cfg.EntryWindowMins) * time.Minute)
		side, bi, ok := orb.FirstBreakout(watch, buy, sell, entryDeadline)
		if !ok {
			continue
		}
		br := buy
		if side == "sell" {
			br = sell
		}
		breakoutBar := watch[bi]

		// Path = breakout bar through the session-end flatten (underlying prices only).
		path := make([]Bar, 0, len(watch)-bi)
		for _, c := range watch[bi:] {
			if c.Timestamp.After(sessionEnd) {
				break
			}
			path = append(path, Bar{Time: c.Timestamp, Underlying: c.Close})
		}
		if len(path) == 0 {
			continue
		}

		out = append(out, Signal{
			Underlying:       underlying,
			Direction:        side,
			AsOf:             breakoutBar.Timestamp,
			UnderlyingStop:   br.StopLoss,
			UnderlyingTarget: br.TakeProfit,
			SessionEnd:       sessionEnd,
			Path:             path,
		})
	}
	return out
}

// groupByETDay buckets candles by their ET calendar day (day floored in ET).
func groupByETDay(candles []data.Candle) map[time.Time][]data.Candle {
	out := make(map[time.Time][]data.Candle)
	for _, c := range candles {
		et := c.Timestamp.In(etLoc)
		day := time.Date(et.Year(), et.Month(), et.Day(), 0, 0, 0, 0, etLoc)
		out[day] = append(out[day], c)
	}
	return out
}

// rthBars filters one day's candles to the regular session [openMin, endMin] ET and
// returns them sorted by time.
func rthBars(day []data.Candle, openMin, endMin int) []data.Candle {
	out := make([]data.Candle, 0, len(day))
	for _, c := range day {
		et := c.Timestamp.In(etLoc)
		mod := et.Hour()*60 + et.Minute()
		if mod < openMin || mod > endMin {
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp.Before(out[j].Timestamp) })
	return out
}

// etTimeOnDay returns the wall-clock ET instant at minutesOfDay on day's ET calendar
// date. time.Date resolves the correct UTC offset per date, so it is DST-safe (unlike
// adding a fixed duration to an ET-midnight instant across a transition).
func etTimeOnDay(day time.Time, minutesOfDay int) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day(), minutesOfDay/60, minutesOfDay%60, 0, 0, etLoc)
}

// parseHHMM parses "HH:MM" to minutes-of-day, returning def on any malformed input.
func parseHHMM(s string, def int) int {
	var h, m int
	if n, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil || n != 2 {
		return def
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return def
	}
	return h*60 + m
}
