package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/spf13/cobra"
)

var modelsCmd = &cobra.Command{
	Use:   "models",
	Short: "Manage and test configured LLM models",
}

// models list — shows all configured models with provider, model name, and status.
var modelsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all configured models with their provider, model name, and status",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		az := analysis.NewAnalyzer(cfg.Models)
		providers := az.Providers()

		fmt.Printf("%-8s  %-12s  %-30s  %s\n", "ROLE", "PROVIDER", "MODEL", "STATUS")
		fmt.Println(strings.Repeat("-", 70))
		for i, p := range providers {
			role := "secondary"
			if i == 0 {
				role = "primary"
			}
			status := "active"
			switch p.ProviderName() {
			case "openai", "ollama":
				status = "not implemented"
			default:
				if !p.IsAvailable() {
					status = "inactive (missing API key)"
				}
			}
			fmt.Printf("%-8s  %-12s  %-30s  %s\n", role, p.ProviderName(), p.ModelName(), status)
		}
		return nil
	},
}

// models test <prompt> — sends a freeform prompt to the primary model.
var modelsTestCmd = &cobra.Command{
	Use:   "test <prompt>",
	Short: "Send a freeform prompt to the primary model and print the raw response",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		az := analysis.NewAnalyzer(cfg.Models)
		primary := az.Primary()

		if !primary.IsAvailable() {
			return fmt.Errorf("primary model %s/%s is not available — check API key env: %s",
				primary.ProviderName(), primary.ModelName(), cfg.Models.Primary.APIKeyEnv)
		}

		prompt := strings.Join(args, " ")
		fmt.Fprintf(os.Stderr, "→ Sending to %s/%s…\n", primary.ProviderName(), primary.ModelName())

		// Use a minimal AnalysisRequest; the test prompt goes into the Ticker field
		// so the prompt template surfaces it in the "TICKER" section header.
		req := analysis.AnalysisRequest{
			Ticker:   prompt,
			Strategy: "test",
		}

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		_, meta, err := primary.Analyze(ctx, req)
		if meta.RawResponse != "" {
			fmt.Println(meta.RawResponse)
		}
		if err != nil {
			return fmt.Errorf("model call failed: %w", err)
		}
		fmt.Fprintf(os.Stderr, "\n[tokens in=%d out=%d latency=%dms]\n",
			meta.InputTokens, meta.OutputTokens, meta.LatencyMs)
		return nil
	},
}

func init() {
	modelsCmd.AddCommand(modelsListCmd)
	modelsCmd.AddCommand(modelsTestCmd)
	// Future sub-commands (stubs)
	modelsCmd.AddCommand(
		&cobra.Command{Use: "performance", Short: "Show model performance stats", RunE: stub("te models performance")},
		&cobra.Command{Use: "set-primary", Short: "Set primary model", RunE: stub("te models set-primary")},
	)
}
