package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	orbstrategy "github.com/romeo-folie/trade-engine/internal/orb"
	"github.com/romeo-folie/trade-engine/internal/store"
)

type orbRepairPlan struct {
	Trades []orbTradeRepair `yaml:"trades"`
}

type orbTradeRepair struct {
	TradeID        string          `yaml:"trade_id"`
	ExpectStatus   string          `yaml:"expect_status"`
	AssetClass     string          `yaml:"asset_class"`
	OwnerScope     string          `yaml:"owner_scope"`
	ConID          string          `yaml:"conid"`
	PointValue     float64         `yaml:"point_value"`
	MaxHoldMinutes *int            `yaml:"max_hold_minutes"`
	RepairSession  bool            `yaml:"repair_session"`
	Close          *orbRepairClose `yaml:"close"`
}

type orbRepairClose struct {
	ExitType      string  `yaml:"exit_type"`
	ExitSource    string  `yaml:"exit_source"`
	ExitPrice     float64 `yaml:"exit_price"`
	ClosedAt      string  `yaml:"closed_at"`
	BrokerOrderID string  `yaml:"broker_order_id"`
}

var (
	orbRepairPlanPath string
	orbRepairApply    bool
)

var orbRepairCmd = &cobra.Command{
	Use:   "repair-trades",
	Short: "Preview or transactionally repair stuck ORB trade identity and exits",
	Long: `Repair stuck ORB rows from an explicit YAML plan. Dry-run is the default.
Every action uses an expected-status compare-and-set; --apply commits the entire
plan in one transaction or rolls it all back. Broker and replay closes must be
labelled explicitly, and P&L is recomputed from the row's entry fill and the
plan's durable point_value.`,
	RunE: runORBRepair,
}

func init() {
	orbRepairCmd.Flags().StringVar(&orbRepairPlanPath, "plan", "", "YAML repair plan (required)")
	orbRepairCmd.Flags().BoolVar(&orbRepairApply, "apply", false, "apply transaction (default: dry-run)")
	_ = orbRepairCmd.MarkFlagRequired("plan")
	orbCmd.AddCommand(orbRepairCmd)
}

func runORBRepair(cmd *cobra.Command, _ []string) error {
	planBytes, err := os.ReadFile(orbRepairPlanPath)
	if err != nil {
		return fmt.Errorf("read repair plan: %w", err)
	}
	var plan orbRepairPlan
	if err := yaml.Unmarshal(planBytes, &plan); err != nil {
		return fmt.Errorf("parse repair plan: %w", err)
	}
	if err := validateORBRepairPlan(plan); err != nil {
		return err
	}

	ctx := context.Background()
	if err := store.Connect(ctx); err != nil {
		return fmt.Errorf("connect db: %w", err)
	}
	defer store.Close()

	type preview struct {
		TradeID        string  `json:"trade_id"`
		Ticker         string  `json:"ticker"`
		Current        string  `json:"current_status"`
		OwnerScope     string  `json:"owner_scope"`
		AssetClass     string  `json:"asset_class"`
		ConID          string  `json:"conid,omitempty"`
		PointValue     float64 `json:"point_value"`
		MaxHoldMinutes *int    `json:"max_hold_minutes,omitempty"`
		ExitSource     string  `json:"exit_source,omitempty"`
		ExitType       string  `json:"exit_type,omitempty"`
		ExitPrice      float64 `json:"exit_price,omitempty"`
		CalculatedPnL  float64 `json:"calculated_pnl,omitempty"`
	}
	previews := make([]preview, 0, len(plan.Trades))
	rows := make([]*store.ORBTrade, 0, len(plan.Trades))
	for _, action := range plan.Trades {
		t, err := store.GetORBTradeByID(ctx, store.GetDB(), action.TradeID)
		if err != nil {
			return fmt.Errorf("load trade %s: %w", action.TradeID, err)
		}
		if t == nil {
			return fmt.Errorf("trade %s not found", action.TradeID)
		}
		if t.Status != action.ExpectStatus {
			return fmt.Errorf("trade %s status is %q, expected %q", t.ID, t.Status, action.ExpectStatus)
		}
		if action.Close != nil {
			if err := validateORBRepairTarget(*t); err != nil {
				return err
			}
			if _, err := orbRepairCloseTime(*t, action.Close.ClosedAt); err != nil {
				return err
			}
		}
		p := preview{TradeID: t.ID, Ticker: t.Ticker, Current: t.Status,
			OwnerScope: action.OwnerScope, AssetClass: action.AssetClass,
			ConID: action.ConID, PointValue: action.PointValue, MaxHoldMinutes: action.MaxHoldMinutes}
		if action.Close != nil {
			p.ExitSource, p.ExitType, p.ExitPrice = action.Close.ExitSource, action.Close.ExitType, action.Close.ExitPrice
			p.CalculatedPnL = orbstrategy.ExitPnLF(t.Side, t.FillPrice, action.Close.ExitPrice, t.Qty, action.PointValue)
		}
		previews = append(previews, p)
		rows = append(rows, t)
	}
	encoded, _ := json.MarshalIndent(previews, "", "  ")
	fmt.Fprintln(cmd.OutOrStdout(), string(encoded))
	if !orbRepairApply {
		fmt.Fprintln(cmd.OutOrStdout(), "DRY RUN — re-run with --apply to commit this plan")
		return nil
	}

	tx, err := store.GetDB().Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin repair: %w", err)
	}
	defer tx.Rollback(ctx) // no-op after commit
	for i, action := range plan.Trades {
		t := rows[i]
		closedAt, pnl, status, exitType, exitSource, exitOrderID := any(nil), any(nil), t.Status, any(nil), any(nil), any(nil)
		exitPrice := any(nil)
		if action.Close != nil {
			parsed, err := orbRepairCloseTime(*t, action.Close.ClosedAt)
			if err != nil {
				return err
			}
			closedAt = parsed
			pnl = orbstrategy.ExitPnLF(t.Side, t.FillPrice, action.Close.ExitPrice, t.Qty, action.PointValue)
			status, exitType, exitSource, exitPrice = "closed", action.Close.ExitType, action.Close.ExitSource, action.Close.ExitPrice
			if action.Close.BrokerOrderID != "" {
				exitOrderID = action.Close.BrokerOrderID
			}
		}
		tag, err := tx.Exec(ctx, `
			UPDATE orb_trades
			SET asset_class=$1, owner_scope=$2, conid=NULLIF($3,''), point_value=$4,
			    max_hold_minutes=COALESCE($5,max_hold_minutes),
			    status=$6, exit_type=COALESCE($7,exit_type), exit_source=COALESCE($8,exit_source),
			    exit_price=COALESCE($9,exit_price), pnl=COALESCE($10,pnl),
			    closed_at=COALESCE($11,closed_at), exit_order_id=COALESCE($12,exit_order_id)
			WHERE id=$13 AND status=$14`,
			action.AssetClass, action.OwnerScope, action.ConID, action.PointValue,
			action.MaxHoldMinutes, status, exitType, exitSource, exitPrice, pnl, closedAt, exitOrderID,
			t.ID, action.ExpectStatus)
		if err != nil {
			return fmt.Errorf("repair trade %s: %w", t.ID, err)
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("repair trade %s lost expected-status race", t.ID)
		}
		if action.RepairSession {
			if _, err := tx.Exec(ctx, `UPDATE orb_sessions SET asset_class=$1, owner_scope=$2 WHERE id=$3`,
				action.AssetClass, action.OwnerScope, t.SessionID); err != nil {
				return fmt.Errorf("repair session %s: %w", t.SessionID, err)
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit repair: %w", err)
	}
	fmt.Fprintln(cmd.OutOrStdout(), "APPLIED — all repair actions committed transactionally")
	return nil
}

func orbRepairCloseTime(t store.ORBTrade, value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("trade %s closed_at: %w", t.ID, err)
	}
	entryAt := t.CreatedAt
	if t.FilledAt != nil {
		entryAt = *t.FilledAt
	}
	if !entryAt.IsZero() && parsed.Before(entryAt) {
		return time.Time{}, fmt.Errorf("trade %s closed_at %s precedes entry fill %s", t.ID, parsed.Format(time.RFC3339), entryAt.Format(time.RFC3339))
	}
	return parsed, nil
}

func validateORBRepairTarget(t store.ORBTrade) error {
	if t.FillPrice <= 0 || t.Qty <= 0 || (t.Side != "buy" && t.Side != "sell") {
		return fmt.Errorf("trade %s has invalid stored entry economics (side=%q fill_price=%v qty=%d)",
			t.ID, t.Side, t.FillPrice, t.Qty)
	}
	return nil
}

func validateORBRepairPlan(plan orbRepairPlan) error {
	if len(plan.Trades) == 0 {
		return fmt.Errorf("repair plan has no trades")
	}
	seen := make(map[string]bool, len(plan.Trades))
	for _, a := range plan.Trades {
		if strings.TrimSpace(a.TradeID) == "" || seen[a.TradeID] {
			return fmt.Errorf("every repair action needs a unique trade_id")
		}
		seen[a.TradeID] = true
		if (a.ExpectStatus != "filled" && a.ExpectStatus != "pending") || a.AssetClass == "" || !store.IsValidORBOwnerScope(a.OwnerScope) || a.PointValue <= 0 {
			return fmt.Errorf("trade %s has incomplete/invalid identity", a.TradeID)
		}
		if a.MaxHoldMinutes != nil && *a.MaxHoldMinutes < 0 {
			return fmt.Errorf("trade %s has invalid max_hold_minutes", a.TradeID)
		}
		expectedAsset, _ := store.ORBOwnerAssetClass(a.OwnerScope)
		if a.AssetClass != expectedAsset || (strings.HasPrefix(a.OwnerScope, "futures_") && strings.TrimSpace(a.ConID) == "") {
			return fmt.Errorf("trade %s owner_scope does not match its asset/conid identity", a.TradeID)
		}
		if a.Close != nil {
			if a.ExpectStatus != "filled" {
				return fmt.Errorf("trade %s close action requires expect_status filled", a.TradeID)
			}
			if a.Close.ExitPrice <= 0 || a.Close.ExitType == "" ||
				(a.Close.ExitSource != "broker" && a.Close.ExitSource != "replay") || a.Close.ClosedAt == "" {
				return fmt.Errorf("trade %s has incomplete/invalid close evidence", a.TradeID)
			}
			if _, err := time.Parse(time.RFC3339, a.Close.ClosedAt); err != nil {
				return fmt.Errorf("trade %s has invalid closed_at: %w", a.TradeID, err)
			}
			if a.Close.ExitSource == "broker" && a.Close.BrokerOrderID == "" {
				return fmt.Errorf("trade %s broker close requires broker_order_id", a.TradeID)
			}
		}
	}
	return nil
}
