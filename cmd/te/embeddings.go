package main

import (
	"context"
	"fmt"
	"text/tabwriter"
	"os"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/embeddings"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/store"
	"github.com/spf13/cobra"
)

var embeddingsCmd = &cobra.Command{
	Use:   "embeddings",
	Short: "Manage vector embedding generation",
	Long: `Commands for monitoring and managing the embedding pipeline.

Embeddings power semantic search ("find similar past setups") and are generated
asynchronously by the background worker when embeddings.enabled = true.`,
}

func init() {
	embeddingsCmd.AddCommand(embeddingsStatusCmd)
	embeddingsCmd.AddCommand(embeddingsReindexCmd)
}

// ── te embeddings status ──────────────────────────────────────────────────────

var embeddingsStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show embedding coverage across all tables",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		if err := logger.Init(cfg.Logging.Level, cfg.Logging.Format); err != nil {
			return err
		}
		if err := connectDB(); err != nil {
			return err
		}
		defer store.Close()

		counts, err := store.EmbeddingCounts(context.Background())
		if err != nil {
			return fmt.Errorf("embedding counts: %w", err)
		}

		fmt.Printf("Embeddings status — provider: %s  model: %s  enabled: %v\n\n",
			cfg.Embeddings.Provider, effectiveModel(cfg.Embeddings), cfg.Embeddings.Enabled)

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "TABLE\tEMBEDDED\tTOTAL\tCOVERAGE")
		fmt.Fprintln(w, "-----\t--------\t-----\t--------")
		for _, table := range []string{"analyses", "news_items", "entities"} {
			c := counts[table]
			pct := 0.0
			if c[1] > 0 {
				pct = float64(c[0]) / float64(c[1]) * 100
			}
			fmt.Fprintf(w, "%s\t%d\t%d\t%.1f%%\n", table, c[0], c[1], pct)
		}
		w.Flush()
		return nil
	},
}

// ── te embeddings reindex ─────────────────────────────────────────────────────

var reindexForce bool

var embeddingsReindexCmd = &cobra.Command{
	Use:   "reindex",
	Short: "Re-generate all embeddings using the current provider",
	Long: `Re-generates embeddings for every row in analyses, news_items, and entities.
Use this after switching embedding providers or models to keep all vectors
consistent.  Pass --force to re-embed rows that already have an embedding.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		if err := logger.Init(cfg.Logging.Level, cfg.Logging.Format); err != nil {
			return err
		}
		if !cfg.Embeddings.Enabled {
			return fmt.Errorf("embeddings disabled in config (set embeddings.enabled: true)")
		}
		if err := connectDB(); err != nil {
			return err
		}
		defer store.Close()

		worker, err := embeddings.NewWorker(cfg.Embeddings)
		if err != nil {
			return fmt.Errorf("init embedder: %w", err)
		}

		ctx := context.Background()

		if reindexForce {
			// Clear existing embeddings so the worker picks them all up.
			fmt.Println("Clearing existing embeddings…")
			if err := store.ClearAllEmbeddings(ctx); err != nil {
				return fmt.Errorf("clear embeddings: %w", err)
			}
		}

		// Show pre-reindex counts.
		before, _ := store.EmbeddingCounts(ctx)

		fmt.Printf("Re-indexing with provider=%s model=%s…\n",
			cfg.Embeddings.Provider, effectiveModel(cfg.Embeddings))

		// Run cycles until all rows are covered.
		batchSize := cfg.Embeddings.BatchSize
		if batchSize <= 0 {
			batchSize = 50
		}
		total := 0
		for {
			counts, _ := store.EmbeddingCounts(ctx)
			remaining := 0
			for _, c := range counts {
				remaining += c[1] - c[0]
			}
			if remaining == 0 {
				break
			}
			fmt.Printf("  %d row(s) remaining…\r", remaining)
			worker.RunCycle(ctx)
			newCounts, _ := store.EmbeddingCounts(ctx)
			newRemaining := 0
			for _, c := range newCounts {
				newRemaining += c[1] - c[0]
			}
			done := remaining - newRemaining
			total += done
			if done == 0 {
				// No progress — break to avoid infinite loop (e.g. API error).
				fmt.Printf("\nWarning: no progress after batch — stopping.\n")
				break
			}
		}

		after, _ := store.EmbeddingCounts(ctx)
		fmt.Printf("\nReindex complete: %d embedding(s) generated.\n\n", total)

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "TABLE\tBEFORE\tAFTER\tTOTAL")
		fmt.Fprintln(w, "-----\t------\t-----\t-----")
		for _, table := range []string{"analyses", "news_items", "entities"} {
			b := before[table]
			a := after[table]
			fmt.Fprintf(w, "%s\t%d/%d\t%d/%d\t%d\n",
				table, b[0], b[1], a[0], a[1], a[1])
		}
		w.Flush()
		return nil
	},
}

func init() {
	embeddingsReindexCmd.Flags().BoolVar(&reindexForce, "force", false,
		"Clear existing embeddings and regenerate all rows from scratch")
}

func effectiveModel(cfg config.EmbeddingsConfig) string {
	if cfg.Model != "" {
		return cfg.Model
	}
	if cfg.Provider == "ollama" {
		return "nomic-embed-text"
	}
	return "text-embedding-3-small"
}
