package orb

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

// The orb package's DB-backed tests (phantom_exit_guard_test.go) run against
// an ISOLATED per-package database, mirroring internal/engine's pattern
// (PR #333 lineage): never the DSN in DATABASE_URL directly, which may point
// at a live paper/production database. Tests that don't touch the store are
// unaffected. With no DATABASE_URL the suite still runs; DB-backed tests
// skip individually via store.IsConnected().
const orbTestDBName = "tradeengine_orb_test"

func TestMain(m *testing.M) {
	prodDSN := os.Getenv("DATABASE_URL")
	if prodDSN == "" {
		os.Exit(m.Run())
	}

	ctx := context.Background()
	maintDSN, testDSN, err := deriveORBTestDSNs(prodDSN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "derive test DSN: %v\n", err)
		os.Exit(1)
	}
	if err := ensureORBTestDB(ctx, maintDSN); err != nil {
		fmt.Fprintf(os.Stderr, "ensure test DB: %v\n", err)
		os.Exit(1)
	}
	os.Setenv("DATABASE_URL", testDSN)
	if err := store.Connect(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "store.Connect (orb test DB): %v\n", err)
		os.Exit(1)
	}
	defer store.Close()
	if err := store.Migrate(ctx, findORBMigrationsDir()); err != nil {
		fmt.Fprintf(os.Stderr, "migrate orb test DB: %v\n", err)
		os.Exit(1)
	}
	// Start every `go test` invocation from a clean slate — the isolated DB
	// persists across runs. Scoped to OUR database (tradeengine_orb_test) only.
	for _, tbl := range []string{"orb_trades", "orb_sessions"} {
		if _, err := store.GetDB().Exec(ctx,
			fmt.Sprintf("TRUNCATE TABLE %s RESTART IDENTITY CASCADE", tbl)); err != nil {
			fmt.Fprintf(os.Stderr, "truncate %s: %v\n", tbl, err)
		}
	}
	os.Exit(m.Run())
}

func deriveORBTestDSNs(prodDSN string) (string, string, error) {
	u, err := url.Parse(prodDSN)
	if err != nil {
		return "", "", fmt.Errorf("parse DSN: %w", err)
	}
	u.Path = "/postgres"
	maint := u.String()
	u.Path = "/" + orbTestDBName
	return maint, u.String(), nil
}

func ensureORBTestDB(ctx context.Context, maintDSN string) error {
	conn, err := pgx.Connect(ctx, maintDSN)
	if err != nil {
		return fmt.Errorf("connect to postgres DB: %w", err)
	}
	defer conn.Close(ctx)

	var exists bool
	if err := conn.QueryRow(ctx,
		"SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)", orbTestDBName,
	).Scan(&exists); err != nil {
		return fmt.Errorf("check DB exists: %w", err)
	}
	if !exists {
		if _, err := conn.Exec(ctx, "CREATE DATABASE "+orbTestDBName); err != nil {
			return fmt.Errorf("create test DB: %w", err)
		}
	}
	return nil
}

func findORBMigrationsDir() string {
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
