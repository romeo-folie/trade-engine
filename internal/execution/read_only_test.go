package execution

import (
	"context"
	"errors"
	"testing"
)

type readOnlySpy struct {
	accountCalls int
	ordersCalls  int
}

func (*readOnlySpy) PlaceOrder(context.Context, OrderRequest) (OrderResult, error) {
	return OrderResult{}, nil
}
func (*readOnlySpy) CancelOrder(context.Context, string) error { return nil }
func (*readOnlySpy) GetOrderStatus(context.Context, string) (OrderResult, error) {
	return OrderResult{}, nil
}
func (*readOnlySpy) GetPositions(context.Context) ([]Position, error) { return nil, nil }
func (*readOnlySpy) GetAccount(context.Context) (AccountInfo, error)  { return AccountInfo{}, nil }
func (s *readOnlySpy) AccountSnapshot(context.Context) (BrokerAccount, error) {
	s.accountCalls++
	return BrokerAccount{Equity: 100}, nil
}
func (*readOnlySpy) PositionsSnapshot(context.Context) ([]BrokerPosition, error) { return nil, nil }
func (*readOnlySpy) EquityCurve(context.Context, string, string) (EquityHistory, error) {
	return EquityHistory{}, nil
}
func (*readOnlySpy) RecentFills(context.Context) ([]BrokerOrder, error) { return nil, nil }
func (s *readOnlySpy) GetWorkingOrders(context.Context) ([]WorkingOrder, error) {
	s.ordersCalls++
	return []WorkingOrder{{OrderID: "read-only"}}, nil
}

func TestReadOnlyExecutorBlocksEveryBrokerMutation(t *testing.T) {
	inner := NewPaperExecutor()
	readOnly := NewReadOnlyExecutor(inner)

	if _, err := readOnly.PlaceOrder(context.Background(), OrderRequest{}); !errors.Is(err, ErrReadOnlyExecutor) {
		t.Fatalf("PlaceOrder error = %v, want ErrReadOnlyExecutor", err)
	}
	if err := readOnly.CancelOrder(context.Background(), "order-1"); !errors.Is(err, ErrReadOnlyExecutor) {
		t.Fatalf("CancelOrder error = %v, want ErrReadOnlyExecutor", err)
	}
	if _, err := readOnly.ReplaceStopOrder(context.Background(), "order-1", 10); !errors.Is(err, ErrReadOnlyExecutor) {
		t.Fatalf("ReplaceStopOrder error = %v, want ErrReadOnlyExecutor", err)
	}
	if got := inner.PendingStopOrders(); len(got) != 0 {
		t.Fatalf("mutations reached inner executor: %+v", got)
	}
}

func TestReadOnlyExecutorForwardsBrokerReads(t *testing.T) {
	inner := &readOnlySpy{}
	readOnly := NewReadOnlyExecutor(inner)

	bv, ok := any(readOnly).(BrokerView)
	if !ok {
		t.Fatal("ReadOnlyExecutor does not preserve BrokerView")
	}
	account, err := bv.AccountSnapshot(context.Background())
	if err != nil || account.Equity != 100 || inner.accountCalls != 1 {
		t.Fatalf("AccountSnapshot = %+v, %v (calls=%d)", account, err, inner.accountCalls)
	}
	lister, ok := any(readOnly).(WorkingOrdersLister)
	if !ok {
		t.Fatal("ReadOnlyExecutor does not preserve WorkingOrdersLister")
	}
	orders, err := lister.GetWorkingOrders(context.Background())
	if err != nil || len(orders) != 1 || inner.ordersCalls != 1 {
		t.Fatalf("GetWorkingOrders = %+v, %v (calls=%d)", orders, err, inner.ordersCalls)
	}
}
