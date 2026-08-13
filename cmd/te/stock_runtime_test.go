package main

import (
	"context"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/execution"
)

type startupRecoveryBroker struct{ calls int }

func (b *startupRecoveryBroker) Recover(context.Context) error { b.calls++; return nil }
func (*startupRecoveryBroker) PlaceOrder(context.Context, execution.OrderRequest) (execution.OrderResult, error) {
	return execution.OrderResult{}, nil
}
func (*startupRecoveryBroker) CancelOrder(context.Context, string) error { return nil }
func (*startupRecoveryBroker) GetOrderStatus(context.Context, string) (execution.OrderResult, error) {
	return execution.OrderResult{}, nil
}
func (*startupRecoveryBroker) GetPositions(context.Context) ([]execution.Position, error) {
	return nil, nil
}
func (*startupRecoveryBroker) GetAccount(context.Context) (execution.AccountInfo, error) {
	return execution.AccountInfo{}, nil
}
func (*startupRecoveryBroker) ReplaceStopOrder(context.Context, string, float64) (execution.OrderResult, error) {
	return execution.OrderResult{}, nil
}

func TestStockOrderRecoveryForStartupSkipsNoDB(t *testing.T) {
	broker := &startupRecoveryBroker{}
	if recoverer := stockOrderRecoveryForStartup(false, broker); recoverer != nil {
		t.Fatal("DB-unavailable startup must not invoke durable order recovery")
	}
	if recoverer := stockOrderRecoveryForStartup(true, broker); recoverer == nil {
		t.Fatal("connected startup must preserve durable order recovery")
	}
}
