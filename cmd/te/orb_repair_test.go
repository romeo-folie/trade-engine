package main

import (
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/store"
)

func TestValidateORBRepairPlan(t *testing.T) {
	valid := orbRepairPlan{Trades: []orbTradeRepair{{
		TradeID: "mym", ExpectStatus: "filled", AssetClass: "futures",
		OwnerScope: "futures_index_orb", ConID: "815824200", PointValue: 0.5,
		Close: &orbRepairClose{ExitType: "broker_exit", ExitSource: "broker", ExitPrice: 54115,
			ClosedAt: "2026-08-10T08:00:00Z", BrokerOrderID: "54071206"},
	}}}
	if err := validateORBRepairPlan(valid); err != nil {
		t.Fatalf("valid plan rejected: %v", err)
	}

	badPointValue := valid
	badPointValue.Trades = append([]orbTradeRepair(nil), valid.Trades...)
	badPointValue.Trades[0].PointValue = 0
	if err := validateORBRepairPlan(badPointValue); err == nil {
		t.Fatal("missing futures point value accepted")
	}

	badBrokerEvidence := valid
	badBrokerEvidence.Trades = append([]orbTradeRepair(nil), valid.Trades...)
	closeCopy := *valid.Trades[0].Close
	closeCopy.BrokerOrderID = ""
	badBrokerEvidence.Trades[0].Close = &closeCopy
	if err := validateORBRepairPlan(badBrokerEvidence); err == nil {
		t.Fatal("broker close without order provenance accepted")
	}

	badOwnerAsset := valid
	badOwnerAsset.Trades = append([]orbTradeRepair(nil), valid.Trades...)
	badOwnerAsset.Trades[0].AssetClass = "equity"
	if err := validateORBRepairPlan(badOwnerAsset); err == nil {
		t.Fatal("futures owner with equity asset accepted")
	}

	missingConID := valid
	missingConID.Trades = append([]orbTradeRepair(nil), valid.Trades...)
	missingConID.Trades[0].ConID = ""
	if err := validateORBRepairPlan(missingConID); err == nil {
		t.Fatal("futures repair without broker contract identity accepted")
	}
}

func TestORBRepairCloseTimeRejectsCloseBeforeFill(t *testing.T) {
	filledAt := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	trade := store.ORBTrade{ID: "mym", CreatedAt: filledAt.Add(-time.Minute), FilledAt: &filledAt}
	if _, err := orbRepairCloseTime(trade, "2026-08-10T08:59:59Z"); err == nil {
		t.Fatal("close before entry fill accepted")
	}
	if _, err := orbRepairCloseTime(trade, "2026-08-10T09:00:00Z"); err != nil {
		t.Fatalf("close at entry fill rejected: %v", err)
	}
}

func TestValidateORBRepairTargetRejectsInvalidStoredEconomics(t *testing.T) {
	valid := store.ORBTrade{ID: "mym", Side: "buy", FillPrice: 52874, Qty: 3}
	if err := validateORBRepairTarget(valid); err != nil {
		t.Fatalf("valid target rejected: %v", err)
	}
	for name, mutate := range map[string]func(*store.ORBTrade){
		"zero fill": func(t *store.ORBTrade) { t.FillPrice = 0 },
		"zero qty":  func(t *store.ORBTrade) { t.Qty = 0 },
		"bad side":  func(t *store.ORBTrade) { t.Side = "hold" },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			mutate(&candidate)
			if err := validateORBRepairTarget(candidate); err == nil {
				t.Fatal("invalid stored economics accepted")
			}
		})
	}
}

func TestValidateORBRepairPlanAllowsPendingIdentityOnly(t *testing.T) {
	maxHold := 240
	plan := orbRepairPlan{Trades: []orbTradeRepair{{
		TradeID: "pending-mym", ExpectStatus: "pending", AssetClass: "futures",
		OwnerScope: "futures_index_orb", ConID: "815824200", PointValue: 0.5,
		MaxHoldMinutes: &maxHold,
	}}}
	if err := validateORBRepairPlan(plan); err != nil {
		t.Fatalf("pending identity-only repair rejected: %v", err)
	}
	plan.Trades[0].Close = &orbRepairClose{ExitType: "broker_exit", ExitSource: "broker", ExitPrice: 1, ClosedAt: "2026-08-10T08:00:00Z", BrokerOrderID: "1"}
	if err := validateORBRepairPlan(plan); err == nil {
		t.Fatal("pending repair with close action accepted")
	}
}
