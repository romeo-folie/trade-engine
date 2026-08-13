package execution

import (
	"context"
	"errors"
	"testing"
)

type readOnlySpy struct {
	accountCalls int
	ordersCalls  int
	lookupCalls  int
	chainCalls   int
	snapCalls    int
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
func (s *readOnlySpy) GetOrderByClientID(context.Context, string) (OrderResult, error) {
	s.lookupCalls++
	return OrderResult{BrokerOrderID: "by-client"}, nil
}
func (s *readOnlySpy) GetOptionChain(context.Context, string, OptionChainFilter) ([]OptionSnapshot, error) {
	s.chainCalls++
	return []OptionSnapshot{{OCCSymbol: "SPY260814C00500000"}}, nil
}
func (s *readOnlySpy) GetOptionSnapshots(context.Context, []string) ([]OptionSnapshot, error) {
	s.snapCalls++
	return []OptionSnapshot{{OCCSymbol: "SPY260814C00500000"}}, nil
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
	lookup, ok := any(readOnly).(ClientOrderLookup)
	if !ok {
		t.Fatal("ReadOnlyExecutor does not preserve ClientOrderLookup")
	}
	if result, err := lookup.GetOrderByClientID(context.Background(), "client-1"); err != nil || result.BrokerOrderID != "by-client" || inner.lookupCalls != 1 {
		t.Fatalf("GetOrderByClientID = %+v, %v (calls=%d)", result, err, inner.lookupCalls)
	}
	chain, err := readOnly.GetOptionChain(context.Background(), "SPY", OptionChainFilter{})
	if err != nil || len(chain) != 1 || inner.chainCalls != 1 {
		t.Fatalf("GetOptionChain = %+v, %v (calls=%d)", chain, err, inner.chainCalls)
	}
	snaps, err := readOnly.GetOptionSnapshots(context.Background(), []string{"SPY260814C00500000"})
	if err != nil || len(snaps) != 1 || inner.snapCalls != 1 {
		t.Fatalf("GetOptionSnapshots = %+v, %v (calls=%d)", snaps, err, inner.snapCalls)
	}
}
