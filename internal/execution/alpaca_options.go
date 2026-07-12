package execution

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/logger"
)

// maxOptionPages bounds paginated chain/contract fetches so a malformed or
// repeating next_page_token cannot spin forever. A full single-underlying chain
// is well under this many pages at Alpaca's page sizes.
const maxOptionPages = 50

// ─── OCC symbol ─────────────────────────────────────────────────────────────
//
// Alpaca identifies option contracts with the compact OCC symbol:
//
//	UNDERLYING + YYMMDD + C|P + strike×1000 zero-padded to 8 digits
//	e.g. AAPL240223C00095000 = AAPL, 2026-02-23, Call, $95.00 strike.
//
// (Alpaca omits the OSI space-padding of the root, so the total length varies
// with the underlying length.)

// OCCSymbol renders an OptionContract as its Alpaca OCC symbol. The Multiplier
// field is not encoded in the symbol (it is always 100 for these contracts).
func OCCSymbol(oc OptionContract) string {
	right := strings.ToUpper(oc.Right)
	strikeThousandths := int64(math.Round(oc.Strike * 1000))
	return fmt.Sprintf("%s%s%s%08d",
		strings.ToUpper(oc.Underlying),
		oc.Expiry.Format("060102"),
		right,
		strikeThousandths,
	)
}

// ParseOCCSymbol parses an Alpaca OCC symbol into an OptionContract. It reads the
// fixed-width tail (8-digit strike, 1-char right, 6-digit date) and treats the
// remainder as the underlying. Multiplier is set to 100 (standard US equity
// option). Returns an error if the symbol is too short or malformed.
func ParseOCCSymbol(sym string) (OptionContract, error) {
	sym = strings.TrimSpace(sym)
	// underlying(≥1) + date(6) + right(1) + strike(8)
	if len(sym) < 16 {
		return OptionContract{}, fmt.Errorf("occ symbol %q too short", sym)
	}
	strikePart := sym[len(sym)-8:]
	rightPart := sym[len(sym)-9 : len(sym)-8]
	datePart := sym[len(sym)-15 : len(sym)-9]
	underlying := sym[:len(sym)-15]
	if underlying == "" {
		return OptionContract{}, fmt.Errorf("occ symbol %q has empty underlying", sym)
	}
	if rightPart != "C" && rightPart != "P" {
		return OptionContract{}, fmt.Errorf("occ symbol %q has invalid right %q", sym, rightPart)
	}
	expiry, err := time.Parse("060102", datePart)
	if err != nil {
		return OptionContract{}, fmt.Errorf("occ symbol %q bad date %q: %w", sym, datePart, err)
	}
	strikeThousandths, err := strconv.ParseInt(strikePart, 10, 64)
	if err != nil {
		return OptionContract{}, fmt.Errorf("occ symbol %q bad strike %q: %w", sym, strikePart, err)
	}
	return OptionContract{
		Underlying: underlying,
		Expiry:     expiry,
		Strike:     float64(strikeThousandths) / 1000.0,
		Right:      rightPart,
		OCCSymbol:  sym,
		Multiplier: 100,
	}, nil
}

// IsOCCSymbol reports whether sym parses as an option OCC symbol. Reconcilers that
// key on equity/forex tickers use it to skip option positions (which report their
// OCC symbol as the position symbol) so they don't false-alarm on them.
func IsOCCSymbol(sym string) bool {
	_, err := ParseOCCSymbol(sym)
	return err == nil
}

// ─── Option orders ──────────────────────────────────────────────────────────

// placeOptionOrder submits a single-leg option order. Options-ORB v1 is long-only
// (buy calls/puts) with a limit order at mid; Alpaca rejects bracket/OCO on
// options, so no exit legs are attached here — exits are app-managed (issue #415).
func (b *AlpacaBroker) placeOptionOrder(ctx context.Context, req OrderRequest) (OrderResult, error) {
	if req.Option == nil || req.Option.OCCSymbol == "" {
		return OrderResult{}, fmt.Errorf("alpaca placeOptionOrder: OrderRequest.Option/OCCSymbol required for SecType OPT")
	}
	// Options trade in whole contracts.
	body := map[string]any{
		"symbol":        req.Option.OCCSymbol,
		"qty":           strconv.FormatInt(int64(req.Quantity), 10),
		"side":          string(req.Side),
		"type":          string(req.Type),
		"time_in_force": "day",
	}
	if req.Type != OrderTypeMarket && req.LimitPrice > 0 {
		body["limit_price"] = fmt.Sprintf("%.2f", req.LimitPrice)
	}

	var resp struct {
		ID          string `json:"id"`
		Status      string `json:"status"`
		FilledAvgPx string `json:"filled_avg_price"`
		FilledQty   string `json:"filled_qty"`
	}
	if err := b.do(ctx, http.MethodPost, "/v2/orders", body, &resp); err != nil {
		return OrderResult{}, fmt.Errorf("alpaca placeOptionOrder: %w", err)
	}
	filledPrice, err := parseFloat(resp.FilledAvgPx)
	if err != nil {
		return OrderResult{}, fmt.Errorf("alpaca placeOptionOrder: parse filled_avg_price %q: %w", resp.FilledAvgPx, err)
	}
	filledQty, err := parseFloat(resp.FilledQty)
	if err != nil {
		return OrderResult{}, fmt.Errorf("alpaca placeOptionOrder: parse filled_qty %q: %w", resp.FilledQty, err)
	}
	return OrderResult{
		BrokerOrderID: resp.ID,
		Status:        resp.Status,
		FilledPrice:   filledPrice,
		FilledQty:     filledQty,
	}, nil
}

// ─── Chain snapshots (market-data host) ─────────────────────────────────────

// OptionSnapshot is one contract's live quote plus greeks/implied volatility from
// the Alpaca options chain. Delta and IV are pointers because Alpaca omits greeks
// for some contracts (notably intermittently on 0DTE) — a nil is "unavailable",
// never a spurious zero, so the fail-closed gates (issue #412) can abort correctly.
type OptionSnapshot struct {
	OCCSymbol string
	Bid       float64
	Ask       float64
	Delta     *float64
	IV        *float64
}

// Mid returns the bid-ask midpoint, or 0 when either side is missing.
func (s OptionSnapshot) Mid() float64 {
	if s.Bid <= 0 || s.Ask <= 0 {
		return 0
	}
	return (s.Bid + s.Ask) / 2
}

// OptionChainFilter narrows a chain request. Zero-value fields are omitted.
type OptionChainFilter struct {
	Right     string // "call" | "put" | "" (both)
	ExpiryGTE time.Time
	ExpiryLTE time.Time
	StrikeGTE float64
	StrikeLTE float64
}

// GetOptionChain fetches live chain snapshots for an underlying from the market-
// data host, following pagination. It warns (rather than silently proceeding)
// when running on the delayed indicative feed, since options-ORB decisions at the
// open require the real-time OPRA feed.
func (b *AlpacaBroker) GetOptionChain(ctx context.Context, underlying string, filt OptionChainFilter) ([]OptionSnapshot, error) {
	if b.optionFeed != "opra" {
		logger.Warn("alpaca option chain on non-real-time feed",
			"feed", b.optionFeed, "underlying", underlying,
			"note", "quotes are delayed; options-ORB requires the OPRA (Algo Trader Plus) feed")
	}

	q := url.Values{}
	q.Set("feed", b.optionFeed)
	if filt.Right != "" {
		q.Set("type", filt.Right)
	}
	if !filt.ExpiryGTE.IsZero() {
		q.Set("expiration_date_gte", filt.ExpiryGTE.Format("2006-01-02"))
	}
	if !filt.ExpiryLTE.IsZero() {
		q.Set("expiration_date_lte", filt.ExpiryLTE.Format("2006-01-02"))
	}
	if filt.StrikeGTE > 0 {
		q.Set("strike_price_gte", strconv.FormatFloat(filt.StrikeGTE, 'f', -1, 64))
	}
	if filt.StrikeLTE > 0 {
		q.Set("strike_price_lte", strconv.FormatFloat(filt.StrikeLTE, 'f', -1, 64))
	}

	var out []OptionSnapshot
	// Bounded so a malformed or repeating next_page_token can never spin forever.
	for range maxOptionPages {
		var resp struct {
			Snapshots map[string]struct {
				LatestQuote struct {
					BidPrice float64 `json:"bp"`
					AskPrice float64 `json:"ap"`
				} `json:"latestQuote"`
				Greeks *struct {
					Delta float64 `json:"delta"`
				} `json:"greeks"`
				ImpliedVolatility *float64 `json:"impliedVolatility"`
			} `json:"snapshots"`
			NextPageToken *string `json:"next_page_token"`
		}
		path := "/v1beta1/options/snapshots/" + url.PathEscape(strings.ToUpper(underlying)) + "?" + q.Encode()
		if err := b.doData(ctx, http.MethodGet, path, nil, &resp); err != nil {
			return nil, fmt.Errorf("alpaca GetOptionChain %s: %w", underlying, err)
		}
		for sym, s := range resp.Snapshots {
			snap := OptionSnapshot{
				OCCSymbol: sym,
				Bid:       s.LatestQuote.BidPrice,
				Ask:       s.LatestQuote.AskPrice,
				IV:        s.ImpliedVolatility,
			}
			if s.Greeks != nil {
				d := s.Greeks.Delta
				snap.Delta = &d
			}
			out = append(out, snap)
		}
		if resp.NextPageToken == nil || *resp.NextPageToken == "" {
			break
		}
		q.Set("page_token", *resp.NextPageToken)
	}
	return out, nil
}

// ─── Contract resolution (trading host) ─────────────────────────────────────

// OptionContractInfo is a tradable contract returned by the contracts endpoint.
type OptionContractInfo struct {
	Contract OptionContract
	Tradable bool
}

// OptionContractQuery filters the contracts lookup. Zero-value fields are omitted.
type OptionContractQuery struct {
	Underlying string
	ExpiryGTE  time.Time
	ExpiryLTE  time.Time
	StrikeGTE  float64
	StrikeLTE  float64
	Right      string // "call" | "put"
	Limit      int
}

// ResolveOptionContracts looks up tradable option contracts for an underlying on
// the trading host, returning each contract's OCC symbol and tradable flag. This
// is how the router (issue #414) turns a chosen (underlying, expiry, strike,
// right) into an actual tradable symbol.
func (b *AlpacaBroker) ResolveOptionContracts(ctx context.Context, query OptionContractQuery) ([]OptionContractInfo, error) {
	q := url.Values{}
	q.Set("underlying_symbols", strings.ToUpper(query.Underlying))
	if !query.ExpiryGTE.IsZero() {
		q.Set("expiration_date_gte", query.ExpiryGTE.Format("2006-01-02"))
	}
	if !query.ExpiryLTE.IsZero() {
		q.Set("expiration_date_lte", query.ExpiryLTE.Format("2006-01-02"))
	}
	if query.StrikeGTE > 0 {
		q.Set("strike_price_gte", strconv.FormatFloat(query.StrikeGTE, 'f', -1, 64))
	}
	if query.StrikeLTE > 0 {
		q.Set("strike_price_lte", strconv.FormatFloat(query.StrikeLTE, 'f', -1, 64))
	}
	if query.Right != "" {
		q.Set("type", query.Right)
	}
	if query.Limit > 0 {
		q.Set("limit", strconv.Itoa(query.Limit))
	}

	var out []OptionContractInfo
	// Bounded so a malformed or repeating next_page_token can never spin forever.
	for range maxOptionPages {
		var resp struct {
			OptionContracts []struct {
				Symbol         string `json:"symbol"`
				Tradable       bool   `json:"tradable"`
				ExpirationDate string `json:"expiration_date"`
				StrikePrice    string `json:"strike_price"`
				Type           string `json:"type"`
				Underlying     string `json:"underlying_symbol"`
			} `json:"option_contracts"`
			NextPageToken *string `json:"next_page_token"`
		}
		path := "/v2/options/contracts?" + q.Encode()
		if err := b.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
			return nil, fmt.Errorf("alpaca ResolveOptionContracts %s: %w", query.Underlying, err)
		}
		for _, c := range resp.OptionContracts {
			oc, err := ParseOCCSymbol(c.Symbol)
			if err != nil {
				// Fall back to the structured fields if the symbol doesn't parse.
				oc = OptionContract{Underlying: c.Underlying, OCCSymbol: c.Symbol, Multiplier: 100}
				if strings.EqualFold(c.Type, "call") {
					oc.Right = "C"
				} else if strings.EqualFold(c.Type, "put") {
					oc.Right = "P"
				}
				if exp, perr := time.Parse("2006-01-02", c.ExpirationDate); perr == nil {
					oc.Expiry = exp
				}
				if sp, perr := strconv.ParseFloat(c.StrikePrice, 64); perr == nil {
					oc.Strike = sp
				}
			}
			out = append(out, OptionContractInfo{Contract: oc, Tradable: c.Tradable})
		}
		if resp.NextPageToken == nil || *resp.NextPageToken == "" {
			break
		}
		q.Set("page_token", *resp.NextPageToken)
	}
	return out, nil
}
