package execution

import (
	"context"
	"fmt"
)

// PolymarketExecutor places orders on Polymarket via their CLOB API.
//
// NOTE: Polymarket's CLOB (Central Limit Order Book) requires ECDSA signing
// of each request using an EVM private key, plus API key + secret for the
// CLOB credentials layer.  Full implementation of the signing pipeline is
// deferred.  Until then, this executor returns ErrPolymarketNotImplemented on
// all mutating calls — the engine will fall through to PredictionPaperExecutor.
type PolymarketExecutor struct {
	apiKey    string // CLOB API key (from api_key_env)
	// privateKey would be the EVM signing key — not yet wired.
}

// ErrPolymarketNotImplemented is returned for live Polymarket order calls until
// the ECDSA signing layer is implemented.
var ErrPolymarketNotImplemented = fmt.Errorf("polymarket live execution not yet implemented: CLOB ECDSA signing required")

// NewPolymarketExecutor creates a PolymarketExecutor.
func NewPolymarketExecutor(apiKey string) *PolymarketExecutor {
	return &PolymarketExecutor{apiKey: apiKey}
}

// PlaceOrder is not yet implemented for live Polymarket trading.
func (e *PolymarketExecutor) PlaceOrder(_ context.Context, _ OrderRequest) (OrderResult, error) {
	return OrderResult{}, ErrPolymarketNotImplemented
}

// CancelOrder is not yet implemented.
func (e *PolymarketExecutor) CancelOrder(_ context.Context, _ string) error {
	return ErrPolymarketNotImplemented
}

// GetOrderStatus is not yet implemented.
func (e *PolymarketExecutor) GetOrderStatus(_ context.Context, _ string) (OrderResult, error) {
	return OrderResult{}, ErrPolymarketNotImplemented
}

// GetPositions is not yet implemented.
func (e *PolymarketExecutor) GetPositions(_ context.Context) ([]Position, error) {
	return nil, ErrPolymarketNotImplemented
}
