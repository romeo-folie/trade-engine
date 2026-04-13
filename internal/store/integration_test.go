package store_test

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/romeo-folie/trade-engine/internal/store"
)

const testDBName = "tradeengine_test"

// TestMain creates (if needed) and connects to a dedicated test database
// so integration tests never pollute the production database.
//
// If DATABASE_URL is not set, the suite exits cleanly (no failure).
func TestMain(m *testing.M) {
	prodDSN := os.Getenv("DATABASE_URL")
	if prodDSN == "" {
		fmt.Fprintln(os.Stderr, "DATABASE_URL not set; skipping store integration tests")
		os.Exit(0)
	}

	ctx := context.Background()

	// Derive a maintenance DSN targeting the default "postgres" database
	// and a test DSN targeting "tradeengine_test".
	maintDSN, testDSN, err := deriveDSNs(prodDSN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "derive test DSN: %v\n", err)
		os.Exit(1)
	}

	// Create the test database if it doesn't exist.
	if err := ensureTestDB(ctx, maintDSN); err != nil {
		fmt.Fprintf(os.Stderr, "ensure test DB: %v\n", err)
		os.Exit(1)
	}

	// Point the store at the test database.
	os.Setenv("DATABASE_URL", testDSN)
	if err := store.Connect(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "store.Connect (test DB): %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	// Run migrations.
	migrationsDir := findMigrationsDir()
	if err := store.Migrate(ctx, migrationsDir); err != nil {
		fmt.Fprintf(os.Stderr, "migrate test DB: %v\n", err)
		os.Exit(1)
	}

	// Truncate all application tables for a clean slate.
	if err := truncateAll(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "truncate test DB: %v\n", err)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

// deriveDSNs takes the production DATABASE_URL and returns:
//   - maintDSN: same host/credentials but targeting the "postgres" database
//   - testDSN:  same host/credentials but targeting "tradeengine_test"
func deriveDSNs(prodDSN string) (string, string, error) {
	u, err := url.Parse(prodDSN)
	if err != nil {
		return "", "", fmt.Errorf("parse DSN: %w", err)
	}
	u.Path = "/postgres"
	maintDSN := u.String()
	u.Path = "/" + testDBName
	testDSN := u.String()
	return maintDSN, testDSN, nil
}

// ensureTestDB creates the test database if it doesn't already exist.
func ensureTestDB(ctx context.Context, maintDSN string) error {
	conn, err := pgx.Connect(ctx, maintDSN)
	if err != nil {
		return fmt.Errorf("connect to postgres DB: %w", err)
	}
	defer conn.Close(ctx)

	var exists bool
	err = conn.QueryRow(ctx,
		"SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)", testDBName,
	).Scan(&exists)
	if err != nil {
		return fmt.Errorf("check DB exists: %w", err)
	}
	if !exists {
		// CREATE DATABASE cannot run inside a transaction and doesn't support
		// parameterised identifiers, but the name is a constant so this is safe.
		if _, err := conn.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s", testDBName)); err != nil {
			return fmt.Errorf("create test DB: %w", err)
		}
		fmt.Fprintf(os.Stderr, "created test database %q\n", testDBName)
	}
	return nil
}

// truncateAll removes all rows from application tables (not schema_migrations)
// using TRUNCATE … CASCADE for a clean test run.
func truncateAll(ctx context.Context) error {
	db := store.GetDB()
	rows, err := db.Query(ctx, `
		SELECT tablename FROM pg_tables
		WHERE schemaname = 'public' AND tablename != 'schema_migrations'
		ORDER BY tablename
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return err
		}
		tables = append(tables, t)
	}
	if len(tables) == 0 {
		return nil
	}
	_, err = db.Exec(ctx, "TRUNCATE "+strings.Join(tables, ", ")+" CASCADE")
	return err
}

// findMigrationsDir walks up from the test file to find the migrations directory.
func findMigrationsDir() string {
	_, filename, _, _ := runtime.Caller(0)
	dir := filepath.Dir(filename)
	for i := 0; i < 5; i++ {
		candidate := filepath.Join(dir, "migrations")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
		dir = filepath.Dir(dir)
	}
	// Fallback: relative to working directory.
	return "migrations"
}
