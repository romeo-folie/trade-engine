package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var pool *pgxpool.Pool

// Connect initialises the global connection pool from DATABASE_URL.
func Connect(ctx context.Context) error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("DATABASE_URL environment variable is not set")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	cfg.MaxConns = 10
	cfg.MinConns = 2
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 30 * time.Minute

	pool, err = pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return fmt.Errorf("open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	return nil
}

// GetDB returns the global pool. Panics if Connect was not called.
// IsConnected returns true if the database pool has been initialized.
func IsConnected() bool {
	return pool != nil
}

func GetDB() *pgxpool.Pool {
	if pool == nil {
		panic("store.Connect() must be called before GetDB()")
	}
	return pool
}

// Close shuts down the pool gracefully.
func Close() {
	if pool != nil {
		pool.Close()
	}
}

// Migrate applies all pending SQL migration files from the given directory.
// Migration state is tracked in the schema_migrations table.
func Migrate(ctx context.Context, migrationsDir string) error {
	db := GetDB()

	// Ensure schema_migrations exists.
	_, err := db.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			filename   TEXT        PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)
	`)
	if err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	// Load applied migrations.
	rows, err := db.Query(ctx, `SELECT filename FROM schema_migrations ORDER BY filename`)
	if err != nil {
		return fmt.Errorf("query schema_migrations: %w", err)
	}
	applied := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		applied[name] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate schema_migrations: %w", err)
	}

	// Find and sort migration files.
	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		return fmt.Errorf("read migrations dir %q: %w", migrationsDir, err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)

	// Apply pending migrations.
	for _, name := range files {
		if applied[name] {
			continue
		}
		sql, err := os.ReadFile(filepath.Join(migrationsDir, name))
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}

		// Check for "-- no-transaction" directive: some DDL statements
		// (e.g. ALTER TYPE … ADD VALUE) cannot run inside a transaction.
		noTx := hasNoTransactionDirective(string(sql))

		if noTx {
			// NOTE: -- no-transaction migrations MUST be idempotent; re-application on startup is possible if recording fails.
			// Acquire a single connection so both the migration and the recording
			// share the same conn, avoiding pool-level failures between the two calls.
			conn, err := db.Acquire(ctx)
			if err != nil {
				return fmt.Errorf("acquire conn for no-tx migration %s: %w", name, err)
			}

			if _, err := conn.Exec(ctx, string(sql)); err != nil {
				conn.Release()
				return fmt.Errorf("apply migration %s: %w", name, err)
			}

			// Best-effort insert with retry; no-tx migrations must be idempotent.
			var recordErr error
			for attempt := 1; attempt <= 3; attempt++ {
				_, recordErr = conn.Exec(ctx,
					`INSERT INTO schema_migrations(filename) VALUES($1) ON CONFLICT (filename) DO NOTHING`, name,
				)
				if recordErr == nil {
					break
				}
				if attempt < 3 {
					select {
					case <-ctx.Done():
						recordErr = ctx.Err()
						break
					case <-time.After(time.Duration(attempt) * 100 * time.Millisecond):
					}
				}
			}
			conn.Release()
			if recordErr != nil {
				return fmt.Errorf("record no-tx migration %s: %w", name, recordErr)
			}
		} else {
			tx, err := db.Begin(ctx)
			if err != nil {
				return fmt.Errorf("begin tx for %s: %w", name, err)
			}

			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				tx.Rollback(ctx) //nolint:errcheck
				return fmt.Errorf("apply migration %s: %w", name, err)
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO schema_migrations(filename) VALUES($1) ON CONFLICT (filename) DO NOTHING`, name,
			); err != nil {
				tx.Rollback(ctx) //nolint:errcheck
				return fmt.Errorf("record migration %s: %w", name, err)
			}
			if err := tx.Commit(ctx); err != nil {
				return fmt.Errorf("commit migration %s: %w", name, err)
			}
		}
		fmt.Printf("  applied: %s\n", name)
	}
	return nil
}

// hasNoTransactionDirective checks whether the first non-empty line of a
// migration file is the "-- no-transaction" directive. It handles UTF-8 BOM,
// CRLF line endings, and leading whitespace.
func hasNoTransactionDirective(content string) bool {
	// Strip UTF-8 BOM if present
	content = strings.TrimPrefix(content, "\xef\xbb\xbf")
	// Find first non-empty line
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimRight(line, "\r")
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		return line == "-- no-transaction"
	}
	return false
}

// Ping checks database connectivity.
func Ping(ctx context.Context) error {
	return GetDB().Ping(ctx)
}
