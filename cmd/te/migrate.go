package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/store"
	"github.com/spf13/cobra"
)

var migrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Apply pending database migrations",
	Long: `Apply all pending SQL migrations from the migrations/ directory.

Migrations are tracked in the schema_migrations table. Running migrate
when all migrations are already applied is a no-op.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		_ = logger.Init("info", "text")

		if err := store.Connect(context.Background()); err != nil {
			return fmt.Errorf("connect to database: %w\n\nEnsure DATABASE_URL is set.", err)
		}
		defer store.Close()

		// Locate the migrations directory relative to the binary or working directory.
		migrationsDir := migrationsPath()
		if _, err := os.Stat(migrationsDir); os.IsNotExist(err) {
			return fmt.Errorf("migrations directory not found: %s\nRun from the project root or install with 'make install'.", migrationsDir)
		}

		fmt.Printf("Running migrations from: %s\n", migrationsDir)
		if err := store.Migrate(context.Background(), migrationsDir); err != nil {
			return err
		}
		fmt.Println("✓ Database is up to date.")
		return nil
	},
}

// migrationsPath resolves the migrations directory.
// Looks for ./migrations first (project root), then alongside the binary.
func migrationsPath() string {
	if _, err := os.Stat("migrations"); err == nil {
		return "migrations"
	}
	exe, err := os.Executable()
	if err != nil {
		return "migrations"
	}
	return filepath.Join(filepath.Dir(exe), "migrations")
}
