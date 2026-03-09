package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/store"
	"github.com/spf13/cobra"
)

var modelsCmd = &cobra.Command{
	Use:   "models",
	Short: "Manage and test configured LLM analysts",
}

// ─── models list ─────────────────────────────────────────────────────────────

var modelsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all configured analysts and arbiter with their status",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		az := analysis.NewAnalyzer(cfg.Models)
		providers := az.Providers()

		fmt.Printf("%-20s  %-12s  %-30s  %s\n", "ROLE", "PROVIDER", "MODEL", "STATUS")
		fmt.Println(strings.Repeat("─", 80))
		for i, p := range providers {
			role := fmt.Sprintf("analyst-%d", i+1)
			status := "active"
			if !p.IsAvailable() {
				status = "inactive (missing API key)"
			}
			fmt.Printf("%-20s  %-12s  %-30s  %s\n", role, p.ProviderName(), p.ModelName(), status)
		}
		if arb := az.Arbiter(); arb != nil {
			fmt.Println(strings.Repeat("─", 80))
			status := "active"
			if !arb.IsAvailable() {
				status = "inactive (missing API key)"
			}
			fmt.Printf("%-20s  %-12s  %-30s  %s\n", "arbiter", arb.ProviderName(), arb.ModelName(), status)
		}
		if cfg.Models.Consensus.Enabled {
			fmt.Printf("\n[ensemble] enabled | fast-path conf ≥ %.2f | spread ≤ %.2f\n",
				cfg.Models.Consensus.FastPathConfidence, cfg.Models.Consensus.FastPathMaxSpread)
		} else {
			fmt.Println("\n[ensemble] disabled (single-analyst mode)")
		}
		return nil
	},
}

// ─── models test ─────────────────────────────────────────────────────────────

var modelsTestCmd = &cobra.Command{
	Use:   "test <prompt>",
	Short: "Send a freeform prompt to all configured analysts in parallel",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		az := analysis.NewAnalyzer(cfg.Models)
		providers := az.Providers()
		prompt := strings.Join(args, " ")
		req := analysis.AnalysisRequest{Ticker: prompt, Strategy: "test"}
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()

		type result struct {
			label string
			meta  analysis.ModelCallMeta
			err   error
		}
		results := make([]result, len(providers))
		var wg sync.WaitGroup
		for i, p := range providers {
			i, p := i, p
			if !p.IsAvailable() {
				results[i] = result{label: fmt.Sprintf("analyst-%d (%s/%s)", i+1, p.ProviderName(), p.ModelName()), err: fmt.Errorf("not available")}
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, meta, err := p.Analyze(ctx, req)
				results[i] = result{label: fmt.Sprintf("analyst-%d (%s/%s)", i+1, p.ProviderName(), p.ModelName()), meta: meta, err: err}
			}()
		}
		wg.Wait()

		for _, r := range results {
			fmt.Printf("\n%s\n%s\n", r.label, strings.Repeat("─", len(r.label)))
			if r.err != nil {
				fmt.Fprintf(os.Stderr, "  ERROR: %v\n", r.err)
				continue
			}
			fmt.Println(r.meta.RawResponse)
			fmt.Fprintf(os.Stderr, "[tokens in=%d out=%d latency=%dms]\n", r.meta.InputTokens, r.meta.OutputTokens, r.meta.LatencyMs)
		}
		return nil
	},
}

// ─── models compare ──────────────────────────────────────────────────────────

var modelsCompareCmd = &cobra.Command{
	Use:   "compare <ticker>",
	Short: "Query all analysts for a ticker and show a side-by-side comparison table",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ticker := strings.ToUpper(args[0])
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		if err := store.Connect(ctx); err != nil {
			return fmt.Errorf("db connect: %w", err)
		}
		defer store.Close()

		az := analysis.NewAnalyzer(cfg.Models)
		req := analysis.AnalysisRequest{Ticker: ticker, Strategy: "momentum"}

		fmt.Fprintf(os.Stderr, "→ Querying %d analysts for %s…\n", len(az.Providers()), ticker)
		results, err := az.AnalyzeEnsemble(ctx, req)
		if err != nil {
			fmt.Fprintf(os.Stderr, "WARN: %v\n", err)
		}

		fpConf := cfg.Models.Consensus.FastPathConfidence
		if fpConf == 0 { fpConf = 0.80 }
		fpSpread := cfg.Models.Consensus.FastPathMaxSpread
		if fpSpread == 0 { fpSpread = 0.15 }
		_, fastPath := analysis.TryFastPath(results, fpConf, fpSpread)

		fmt.Printf("\n%s — Analyst Comparison\n%s\n", ticker, strings.Repeat("═", 85))
		fmt.Printf("%-22s  %-6s  %-10s  %-10s  %-10s  %s\n", "ANALYST", "ACTION", "CONFIDENCE", "STOP", "TARGET", "LATENCY")
		fmt.Println(strings.Repeat("─", 85))

		for i, r := range results {
			label := fmt.Sprintf("analyst-%d (%s)", i+1, r.Model)
			if r.Err != nil {
				fmt.Printf("%-22s  ERROR: %v\n", label, r.Err)
				continue
			}
			fmt.Printf("%-22s  %-6s  %-10.3f  %-10.4f  %-10.4f  %dms\n",
				label, r.Response.Action, r.Response.Confidence, r.Response.StopLoss, r.Response.TakeProfit, r.LatencyMs)
		}
		fmt.Println(strings.Repeat("─", 85))

		if fastPath {
			fmt.Println("→ FAST-PATH: All analysts unanimous — arbiter not needed.")
		} else {
			fmt.Println("→ ARBITER REQUIRED: Analysts disagree or confidence below threshold.")
			if arb := az.Arbiter(); arb != nil && arb.IsAvailable() {
				fmt.Fprintln(os.Stderr, "\n→ Running arbiter synthesis…")
				arbResult, err := analysis.Arbitrate(ctx, arb, req, results)
				if err != nil {
					fmt.Fprintf(os.Stderr, "  Arbiter failed: %v\n", err)
				} else {
					fmt.Printf("\n%-22s  %-6s  %-10.3f  %-10.4f  %-10.4f  %dms\n",
						"ARBITER (final)", arbResult.Final.Action, arbResult.Final.Confidence,
						arbResult.Final.StopLoss, arbResult.Final.TakeProfit, arbResult.Meta.LatencyMs)
					if arbResult.DissentSummary != "" {
						fmt.Printf("\nDissent: %s\n", arbResult.DissentSummary)
					}
					fmt.Printf("\nArbiter reasoning:\n%s\n", arbResult.Final.Reasoning)
				}
			}
		}

		fmt.Printf("\n%s\nANALYST REASONING:\n", strings.Repeat("═", 85))
		for i, r := range results {
			if r.Err == nil {
				fmt.Printf("\n[analyst-%d: %s/%s]\n%s\n", i+1, r.Provider, r.Model, r.Response.Reasoning)
			}
		}
		return nil
	},
}

// ─── models performance ───────────────────────────────────────────────────────

var modelsPerformanceCmd = &cobra.Command{
	Use:   "performance",
	Short: "Show per-model accuracy stats from historical trade outcomes (Phase 12.6)",
	RunE: func(cmd *cobra.Command, args []string) error {
		minTrades, _ := cmd.Flags().GetInt("min-trades")
		asJSON, _ := cmd.Flags().GetBool("json")
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if err := store.Connect(ctx); err != nil {
			return err
		}
		defer store.Close()
		_ = cfg

		rows, err := store.GetModelPerformance(ctx, minTrades)
		if err != nil {
			return fmt.Errorf("query model performance: %w", err)
		}
		if len(rows) == 0 {
			fmt.Printf("No per-model data yet (need ensemble analyses with ≥%d trade outcomes).\n", minTrades)
			return nil
		}
		fmt.Printf("%-14s  %-30s  %8s  %10s  %8s  %8s\n", "PROVIDER", "MODEL", "TRADES", "AVG P&L %", "WIN RATE", "AVG CONF")
		fmt.Println(strings.Repeat("─", 82))
		for _, r := range rows {
			pnl, wr := "─", "─"
			if r.AvgPnLPct != nil { pnl = fmt.Sprintf("%+.2f%%", *r.AvgPnLPct) }
			if r.WinRate != nil    { wr  = fmt.Sprintf("%.1f%%", *r.WinRate*100) }
			fmt.Printf("%-14s  %-30s  %8d  %10s  %8s  %8.3f\n",
				r.Provider, r.Model, r.TradeCount, pnl, wr, r.AvgConfidence)
		}
		if asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(rows)
		}
		return nil
	},
}

func init() {
	modelsPerformanceCmd.Flags().Int("min-trades", 10, "Minimum trade count to include a model")
	modelsPerformanceCmd.Flags().Bool("json", false, "Also emit JSON after the table")

	modelsCmd.AddCommand(modelsListCmd, modelsTestCmd, modelsCompareCmd, modelsPerformanceCmd)
}
