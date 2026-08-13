package execution

import (
	"context"
	"errors"
	"time"
)

// ErrBrokerUnavailable is returned by BrokerView implementations that have no
// real broker connection (e.g. PaperExecutor). Web handlers treat this as a
// signal to fall back to DB-derived data and surface a "stale" badge.
var ErrBrokerUnavailable = errors.New("broker view unavailable")

// BrokerAccount is the canonical broker-side account snapshot used by the
// portfolio web handlers and stock preflight. Account identity deliberately
// does not serialize: it is only displayed by the explicit local preflight
// command.
type BrokerAccount struct {
	AccountID                         string    `json:"-"`
	AccountNumber                     string    `json:"-"`
	Status                            string    `json:"status"`
	Equity                            float64   `json:"equity"`
	LastEquity                        float64   `json:"last_equity"` // previous trading day close
	Cash                              float64   `json:"cash"`
	BuyingPower                       float64   `json:"buying_power"`
	RegTBuyingPower                   float64   `json:"regt_buying_power"`
	DayTradingBuyingPower             float64   `json:"daytrading_buying_power"`
	NonMarginableBuyingPower          float64   `json:"non_marginable_buying_power"`
	LastEquityAvailable               bool      `json:"-"`
	CashAvailable                     bool      `json:"-"`
	BuyingPowerAvailable              bool      `json:"-"`
	RegTBuyingPowerAvailable          bool      `json:"-"`
	DayTradingBuyingPowerAvailable    bool      `json:"-"`
	NonMarginableBuyingPowerAvailable bool      `json:"-"`
	Multiplier                        float64   `json:"multiplier"`
	MultiplierAvailable               bool      `json:"-"`
	Currency                          string    `json:"currency"`
	TradingBlocked                    bool      `json:"trading_blocked"`
	TransfersBlocked                  bool      `json:"transfers_blocked"`
	AccountBlocked                    bool      `json:"account_blocked"`
	TradeSuspendedByUser              bool      `json:"trade_suspended_by_user"`
	ShortingEnabled                   bool      `json:"shorting_enabled"`
	FetchedAt                         time.Time `json:"fetched_at"`
}

// StockAsset is the broker's admission-grade metadata for one equity symbol.
// Entry sizing uses it to select the applicable buying-power field and the
// exact quantity precision accepted by the broker.
type StockAsset struct {
	Symbol                string
	Status                string
	AssetClass            string
	Tradable              bool
	Marginable            bool
	Fractionable          bool
	TradableAvailable     bool
	MarginableAvailable   bool
	FractionableAvailable bool
	FetchedAt             time.Time
}

// StockAssetProvider is the optional read-only capability used immediately
// before a standard-stock BUY is released.
type StockAssetProvider interface {
	StockAssetSnapshot(ctx context.Context, symbol string) (StockAsset, error)
}

// BrokerPosition is the canonical broker-side open position.
type BrokerPosition struct {
	Symbol        string  `json:"symbol"`
	Quantity      float64 `json:"quantity"`
	AvgEntryPrice float64 `json:"avg_entry_price"`
	CurrentPrice  float64 `json:"current_price"`
	MarketValue   float64 `json:"market_value"`
	UnrealizedPL  float64 `json:"unrealized_pl"`
	Side          string  `json:"side"` // "long" | "short"
}

// EquityPoint is one sample on a broker equity curve.
type EquityPoint struct {
	Time   time.Time `json:"t"`
	Equity float64   `json:"v"`
}

// EquityHistory is a sampled equity series returned by EquityCurve.
// Period / Timeframe follow Alpaca's /v2/account/portfolio/history conventions
// ("1D"/"1W"/"1M"/"3M"/"1A"/"all" and "1Min"/"5Min"/"15Min"/"1H"/"1D").
type EquityHistory struct {
	Period    string        `json:"period"`
	Timeframe string        `json:"timeframe"`
	BaseValue float64       `json:"base_value"`
	Points    []EquityPoint `json:"points"`
	FetchedAt time.Time     `json:"fetched_at"`
}

// BrokerOrder is an order as reported by the broker (typically a recent fill).
type BrokerOrder struct {
	BrokerOrderID string    `json:"broker_order_id"`
	TradeID       string    `json:"trade_id,omitempty"`
	Symbol        string    `json:"symbol"`
	Side          string    `json:"side"`
	Qty           float64   `json:"qty"`
	FilledQty     float64   `json:"filled_qty"`
	FilledAvgPx   float64   `json:"filled_avg_price"`
	Status        string    `json:"status"`
	SubmittedAt   time.Time `json:"submitted_at"`
	FilledAt      time.Time `json:"filled_at"`
}

// BrokerView is the read-only side of a broker integration. It exists so
// the portfolio web handlers can serve broker-truth data independently of
// the engine tick loop, even when the engine is paused or sleeping.
//
// Implementations MUST be safe for concurrent use. Implementations that have
// no real broker (e.g. PaperExecutor) MUST return ErrBrokerUnavailable from
// every method so the handler can fall back cleanly.
//
// Method names are deliberately distinct from execution.Executor's methods
// (e.g. GetAccount/GetPositions) to avoid signature collisions: Executor
// returns engine-facing types, BrokerView returns wire-shaped types tailored
// to the web layer.
type BrokerView interface {
	AccountSnapshot(ctx context.Context) (BrokerAccount, error)
	PositionsSnapshot(ctx context.Context) ([]BrokerPosition, error)
	EquityCurve(ctx context.Context, period, timeframe string) (EquityHistory, error)
	RecentFills(ctx context.Context) ([]BrokerOrder, error)
}
