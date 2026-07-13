package engine

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// The engine package's DB-backed tests (bracket_exits_test.go) run against
// an ISOLATED per-package database, mirroring internal/feedback's pattern
// (PR #333 lineage): never the DSN in DATABASE_URL directly, which may point
// at a live paper/production database. Tests that don't touch the store are
// unaffected. With no DATABASE_URL the suite still runs; DB-backed tests
// skip individually via store.IsConnected().
const engineTestDBName = "tradeengine_engine_test"

func TestMain(m *testing.M) {
	prodDSN := os.Getenv("DATABASE_URL")
	if prodDSN == "" {
		os.Exit(m.Run())
	}

	ctx := context.Background()
	maintDSN, testDSN, err := deriveEngineTestDSNs(prodDSN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "derive test DSN: %v\n", err)
		os.Exit(1)
	}
	if err := ensureEngineTestDB(ctx, maintDSN); err != nil {
		fmt.Fprintf(os.Stderr, "ensure test DB: %v\n", err)
		os.Exit(1)
	}
	os.Setenv("DATABASE_URL", testDSN)
	if err := store.Connect(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "store.Connect (engine test DB): %v\n", err)
		os.Exit(1)
	}
	defer store.Close()
	if err := store.Migrate(ctx, findEngineMigrationsDir()); err != nil {
		fmt.Fprintf(os.Stderr, "migrate engine test DB: %v\n", err)
		os.Exit(1)
	}
	// Start every `go test` invocation from a clean slate — tests count
	// rows by ticker, and the isolated DB persists across runs. Scoped to
	// OUR database (tradeengine_engine_test) only.
	for _, tbl := range []string{"orders", "trades", "signals"} {
		if _, err := store.GetDB().Exec(ctx,
			fmt.Sprintf("TRUNCATE TABLE %s RESTART IDENTITY CASCADE", tbl)); err != nil {
			fmt.Fprintf(os.Stderr, "truncate %s: %v\n", tbl, err)
		}
	}
	os.Exit(m.Run())
}

func deriveEngineTestDSNs(prodDSN string) (string, string, error) {
	u, err := url.Parse(prodDSN)
	if err != nil {
		return "", "", fmt.Errorf("parse DSN: %w", err)
	}
	u.Path = "/postgres"
	maint := u.String()
	u.Path = "/" + engineTestDBName
	return maint, u.String(), nil
}

func ensureEngineTestDB(ctx context.Context, maintDSN string) error {
	conn, err := pgx.Connect(ctx, maintDSN)
	if err != nil {
		return fmt.Errorf("connect to postgres DB: %w", err)
	}
	defer conn.Close(ctx)

	var exists bool
	if err := conn.QueryRow(ctx,
		"SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)", engineTestDBName,
	).Scan(&exists); err != nil {
		return fmt.Errorf("check DB exists: %w", err)
	}
	if !exists {
		if _, err := conn.Exec(ctx, "CREATE DATABASE "+engineTestDBName); err != nil {
			return fmt.Errorf("create test DB: %w", err)
		}
	}
	return nil
}

func findEngineMigrationsDir() string {
	dir, _ := os.Getwd()
	for range 6 {
		candidate := filepath.Join(dir, "migrations")
		if st, err := os.Stat(candidate); err == nil && st.IsDir() {
			return candidate
		}
		dir = filepath.Dir(dir)
	}
	return "migrations"
}
