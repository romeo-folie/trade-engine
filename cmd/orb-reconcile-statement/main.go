// Command orb-reconcile-statement reconciles our ORB trade records against an IBKR
// Activity Statement (the broker's own record of executed fills) — the ground truth.
//
// WHY: the live trades feed only retains 7 days, so the 74 historical ORB trades stuck
// at status='filled' could only be reconstructed from market-data bars (a model). An IBKR
// Activity Statement retains real executions for the whole period, so it lets us record the
// ACTUAL exit price and PnL (exit_source='broker') instead of a daily-bar estimate.
//
// HOW: each ORB trade's entry already lives in the DB (fill_price, filled_at). The statement
// Trades section lists every real fill. For each DB trade we locate its entry fill in the
// statement (same symbol/side, broker time just before our recorded fill time), then take the
// next offsetting fill for that symbol as the real exit. PnL is computed from the recorded
// entry and the real broker exit price. Fills are consumed as they are matched so two trades
// never claim the same fill; DB trades are processed in fill-time order so the earliest entry
// claims the earliest fills.
//
// IBKR forex spot is netted (an opposite fill flattens the position regardless of intent), so
// "next offsetting fill after the entry" is the genuine exit. Statement Date/Time is in the
// account's US/Eastern zone; we parse in that zone so it lines up with our UTC filled_at.
//
// SAFETY: dry-run by default — prints the match/PnL it WOULD record and writes nothing. With
// --apply it writes the matched broker exits (exit_source='broker'); CloseORBTrade guards on
// status='filled', so it is idempotent and safe to re-run.
//
//	go run ./cmd/orb-reconcile-statement --file data/DUP723970_20260408.csv
//	go run ./cmd/orb-reconcile-statement --file data/DUP723970_20260408.csv --apply
package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/orb"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// fill is one execution row from the statement's Trades section.
type fill struct {
	symbol string  // normalized to our ticker form, e.g. "EUR/USD", "XAU/USD"
	qty    float64 // magnitude
	sign   float64 // +1 buy / -1 sell
	price  float64
	comm   float64 // commission/fee for this fill (negative = charged)
	t      time.Time
	used   bool
}

func normalizeSymbol(s string) string {
	if strings.Contains(s, ".") { // forex: EUR.USD -> EUR/USD
		return strings.Replace(s, ".", "/", 1)
	}
	if len(s) == 6 { // metal/commodity: XAUUSD -> XAU/USD
		return s[:3] + "/" + s[3:]
	}
	return s
}

func parseStatement(path string) ([]*fill, error) {
	// IBKR Activity Statement Date/Time is in the account's local zone (US/Eastern),
	// not UTC. Parse in that location so conversion to UTC is correct across DST, then
	// times line up with our UTC filled_at (modulo a few minutes of fill-poll lag).
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		return nil, fmt.Errorf("load tz: %w", err)
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1 // statement has ragged multi-section rows
	recs, err := r.ReadAll()
	if err != nil {
		return nil, err
	}

	var fills []*fill
	for _, rec := range recs {
		// Trades,Data,Order,<AssetCat>,<Ccy>,<Acct>,<Symbol>,<Date/Time>,<Qty>,<T.Price>,<C.Price>,<Proceeds>,<Comm>,...
		// Need through index 12 (Comm/Fee); FieldsPerRecord=-1 means rows are ragged.
		if len(rec) < 13 || rec[0] != "Trades" || rec[1] != "Data" || rec[2] != "Order" {
			continue
		}
		if rec[3] != "Forex" && rec[3] != "Commodities" {
			continue
		}
		qty, err1 := strconv.ParseFloat(strings.ReplaceAll(rec[8], ",", ""), 64)
		price, err2 := strconv.ParseFloat(strings.ReplaceAll(rec[9], ",", ""), 64)
		ts, err3 := time.ParseInLocation("2006-01-02, 15:04:05", strings.TrimSpace(rec[7]), loc)
		if err1 != nil || err2 != nil || err3 != nil || qty == 0 {
			continue
		}
		sign := 1.0
		if qty < 0 {
			sign = -1.0
		}
		comm, _ := strconv.ParseFloat(strings.ReplaceAll(rec[12], ",", ""), 64) // Comm/Fee column
		fills = append(fills, &fill{
			symbol: normalizeSymbol(rec[6]),
			qty:    math.Abs(qty),
			sign:   sign,
			price:  price,
			comm:   comm,
			t:      ts.UTC(),
		})
	}
	sort.Slice(fills, func(i, j int) bool { return fills[i].t.Before(fills[j].t) })
	return fills, nil
}

func priceTol(symbol string) float64 {
	if strings.HasPrefix(symbol, "XAU") || strings.HasPrefix(symbol, "XAG") {
		return 8.0
	}
	return 0.0030 // ~30 pips: entry sanity check, generous for stop-fill slippage
}

func sideSign(side string) float64 {
	if side == "sell" {
		return -1.0
	}
	return 1.0
}

// match is the reconciliation outcome for one DB trade.
type match struct {
	t         store.ORBTrade
	exitPrice float64
	exitTime  time.Time
	exitType  string
	pnl       float64 // gross, price-based (exit-entry)*qty
	comm      float64 // entry + exit commission (negative)
	ok        bool
	reason    string
}

// reconcile matches each DB filled trade to its real broker fills in two passes:
// pass 1 reserves every trade's ENTRY fill, pass 2 finds each trade's EXIT (the next
// offsetting fill after its entry). Reserving entries first matters because IBKR forex is
// netted: a single sell fill can serve as a short trade's *entry* or a prior long trade's
// *exit*. Doing exits first lets a long's exit-search greedily consume a fill that is
// actually a distinct short trade's entry; entries-first prevents that collision.
func reconcile(dbTrades []store.ORBTrade, bySym map[string][]*fill, futuresInstruments []config.FuturesInstrument) []match {
	// Process in fill-time order so the earliest trade claims the earliest fills.
	sort.Slice(dbTrades, func(i, j int) bool {
		return anchorTime(dbTrades[i]).Before(anchorTime(dbTrades[j]))
	})

	out := make([]match, len(dbTrades))
	entryIdx := make([]int, len(dbTrades))

	// Pass 1 — reserve each trade's entry fill (same sign, broker time just before our
	// recorded fill time, price within slippage tolerance), closest in time.
	for k, t := range dbTrades {
		out[k] = match{t: t}
		entryIdx[k] = -1
		if t.FillPrice == 0 {
			out[k].reason = "no fill_price"
			continue
		}
		anchor := anchorTime(t)
		want := sideSign(t.Side)
		fills := bySym[t.Ticker]
		best := -1
		var bestDT time.Duration
		for i, fl := range fills {
			if fl.used || fl.sign != want {
				continue
			}
			if math.Abs(fl.qty-float64(t.Qty)) > 0.5 {
				continue // size must match — never reserve a partial/unrelated fill as the entry
			}
			dt := anchor.Sub(fl.t)
			if dt < -5*time.Minute || dt > 90*time.Minute {
				continue
			}
			if math.Abs(fl.price-t.FillPrice) > priceTol(t.Ticker) {
				continue
			}
			adt := dt
			if adt < 0 {
				adt = -adt
			}
			if best < 0 || adt < bestDT {
				best, bestDT = i, adt
			}
		}
		if best < 0 {
			out[k].reason = "no entry fill in statement"
			continue
		}
		fills[best].used = true
		entryIdx[k] = best
	}

	// Pass 2 — for each trade with a reserved entry, take the earliest still-unused
	// offsetting fill after it as the real exit.
	for k, t := range dbTrades {
		if entryIdx[k] < 0 {
			continue
		}
		want := sideSign(t.Side)
		fills := bySym[t.Ticker]
		exitI := -1
		for i := entryIdx[k] + 1; i < len(fills); i++ {
			if fills[i].used || fills[i].sign == want {
				continue
			}
			if math.Abs(fills[i].qty-float64(t.Qty)) > 0.5 {
				continue // offsetting fill must close the same size we entered
			}
			exitI = i
			break
		}
		if exitI < 0 {
			out[k].reason = "entry matched but no offsetting exit fill"
			continue
		}
		fills[exitI].used = true
		ex := fills[exitI]
		out[k].ok = true
		out[k].exitPrice = ex.price
		out[k].exitTime = ex.t
		out[k].comm = fills[entryIdx[k]].comm + ex.comm
		out[k].pnl = orb.ExitPnLF(t.Side, t.FillPrice, ex.price, t.Qty, orb.PointValueForTicker(futuresInstruments, t.Ticker))
		if out[k].pnl >= 0 {
			out[k].exitType = "tp"
		} else {
			out[k].exitType = "stop"
		}
	}
	return out
}

func anchorTime(t store.ORBTrade) time.Time {
	if t.FilledAt != nil {
		return *t.FilledAt
	}
	return t.CreatedAt
}

func main() {
	var (
		path  = flag.String("file", "data/DUP723970_20260408.csv", "path to the IBKR Activity Statement CSV")
		apply = flag.Bool("apply", false, "write matched broker exits; without it the command only prints what it would do")
	)
	flag.Parse()

	_ = godotenv.Load()
	ctx := context.Background()

	fills, err := parseStatement(*path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse statement: %v\n", err)
		os.Exit(1)
	}
	bySym := map[string][]*fill{}
	for _, fl := range fills {
		bySym[fl.symbol] = append(bySym[fl.symbol], fl)
	}
	fmt.Printf("Parsed %d fills from %s\n", len(fills), *path)

	if err := store.Connect(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "connect db: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()
	db := store.GetDB()

	dbTrades, err := store.GetOpenFilledORBTrades(ctx, db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load filled trades: %v\n", err)
		os.Exit(1)
	}

	// Futures instruments (micro-index + COMEX metals) so exit P&L applies the contract
	// point value. Best-effort: if config can't load, fall back to per-unit (forex) P&L.
	var futuresInstruments []config.FuturesInstrument
	if cfg, cerr := config.Load(); cerr == nil {
		futuresInstruments = append(append([]config.FuturesInstrument(nil),
			cfg.FuturesORB.Instruments...), cfg.FuturesORBMetals.Instruments...)
	}

	matches := reconcile(dbTrades, bySym, futuresInstruments)

	mode := "DRY RUN — no changes written (pass --apply to record)"
	if *apply {
		mode = "APPLY — recording matched broker exits"
	}
	fmt.Printf("Matching %d DB filled trades against broker fills | %s\n\n", len(dbTrades), mode)
	fmt.Printf("%-9s %-5s %5s %10s %10s %9s %5s  %s\n", "TICKER", "SIDE", "QTY", "ENTRY", "EXIT", "PNL", "TYPE", "EXIT TIME")

	var matched, unmatched, wins, losses int
	var totalPnL, totalComm float64
	for _, m := range matches {
		if !m.ok {
			unmatched++
			fmt.Printf("%-9s %-5s %5d %10.5f %10s %9s %5s  ⚠ %s\n",
				m.t.Ticker, m.t.Side, m.t.Qty, m.t.FillPrice, "-", "-", "-", m.reason)
			continue
		}
		matched++
		totalPnL += m.pnl
		totalComm += m.comm
		if m.pnl >= 0 {
			wins++
		} else {
			losses++
		}
		fmt.Printf("%-9s %-5s %5d %10.5f %10.5f %+9.2f %5s  %s\n",
			m.t.Ticker, m.t.Side, m.t.Qty, m.t.FillPrice, m.exitPrice, m.pnl, m.exitType,
			m.exitTime.Format("2006-01-02 15:04"))

		if *apply {
			if dbErr := store.CloseORBTradeAt(ctx, db, m.t.ID, m.exitType, "broker", m.exitPrice, m.pnl, m.exitTime); dbErr != nil {
				fmt.Fprintf(os.Stderr, "  ! close %s (%s): %v\n", m.t.Ticker, m.t.ID, dbErr)
			}
		}
	}

	winRate := 0.0
	if matched > 0 {
		winRate = float64(wins) / float64(matched) * 100
	}
	fmt.Printf("\nReconciliation: %d matched (%d win / %d loss, %.0f%% win) | %d unmatched, of %d.\n",
		matched, wins, losses, winRate, unmatched, len(dbTrades))
	fmt.Printf("Broker PnL: gross %+.2f, commissions %.2f, NET %+.2f\n",
		totalPnL, totalComm, totalPnL+totalComm)
	if !*apply && matched > 0 {
		fmt.Println("Dry run only — re-run with --apply to record these broker exits.")
	}
}
