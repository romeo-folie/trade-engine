package orb

import (
	"context"
	"errors"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/execution"
)

type optionsWiringBroker struct{}

func (*optionsWiringBroker) PlaceOrder(context.Context, execution.OrderRequest) (execution.OrderResult, error) {
	return execution.OrderResult{}, nil
}
func (*optionsWiringBroker) CancelOrder(context.Context, string) error { return nil }
func (*optionsWiringBroker) GetOrderStatus(context.Context, string) (execution.OrderResult, error) {
	return execution.OrderResult{}, nil
}
func (*optionsWiringBroker) GetPositions(context.Context) ([]execution.Position, error) {
	return nil, nil
}
func (*optionsWiringBroker) GetAccount(context.Context) (execution.AccountInfo, error) {
	return execution.AccountInfo{}, nil
}
func (*optionsWiringBroker) GetOptionChain(context.Context, string, execution.OptionChainFilter) ([]execution.OptionSnapshot, error) {
	return nil, nil
}
func (*optionsWiringBroker) GetOptionSnapshots(context.Context, []string) ([]execution.OptionSnapshot, error) {
	return nil, nil
}

func TestSetupOptionsORBDryRunKeepsRouterButBlocksMutations(t *testing.T) {
	dryRun := execution.NewReadOnlyExecutor(&optionsWiringBroker{})
	scheduler := &ORBScheduler{}
	scheduler.SetupOptionsORB(config.OptionsORBConfig{
		Enabled: true, Underlyings: []string{"SPY"}, PremiumBudget: 500,
	}, dryRun, dryRun, dryRun)

	if scheduler.optionsRouter == nil || !scheduler.optionsRouter.Handles("SPY") {
		t.Fatal("dry-run options router was not wired for configured underlying")
	}
	if scheduler.optionExit == nil {
		t.Fatal("dry-run option exit manager was not wired")
	}
	if _, err := scheduler.optionsRouter.placer.PlaceOrder(context.Background(), execution.OrderRequest{SecType: "OPT"}); !errors.Is(err, execution.ErrReadOnlyExecutor) {
		t.Fatalf("options entry error = %v, want read-only blocking", err)
	}
	if _, err := scheduler.optionExit.placer.PlaceOrder(context.Background(), execution.OrderRequest{SecType: "OPT"}); !errors.Is(err, execution.ErrReadOnlyExecutor) {
		t.Fatalf("options exit error = %v, want read-only blocking", err)
	}
}

func TestSetupOptionsORBLiveKeepsRawMutationPlacer(t *testing.T) {
	raw := &optionsWiringBroker{}
	scheduler := &ORBScheduler{}
	scheduler.SetupOptionsORB(config.OptionsORBConfig{Enabled: true, Underlyings: []string{"SPY"}}, raw, raw, raw)
	if scheduler.optionsRouter == nil || scheduler.optionsRouter.placer != raw {
		t.Fatal("non-dry-run options router did not retain its execution adapter")
	}
}
