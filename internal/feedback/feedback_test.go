package feedback_test

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/romeo-folie/trade-engine/internal/feedback"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// Mirrors internal/store/integration_test.go's TestMain so the feedback
// suite can drive the real GetFeedbackInsights / GetLatestFeedbackInsights
// queries that GetPerformanceContext depends on. If DATABASE_URL is unset
// the suite exits cleanly — no failure, no pollution of any live DB.
//
// The DB name is intentionally distinct from internal/store's
// `tradeengine_test` so the two packages can run in parallel under
// `go test ./...` without one package's TRUNCATE/DELETE wiping rows
// the other is mid-test on.
const testDBName = "tradeengine_feedback_test"

func TestMain(m *testing.M) {
	prodDSN := os.Getenv("DATABASE_URL")
	if prodDSN == "" {
		fmt.Fprintln(os.Stderr, "DATABASE_URL not set; skipping feedback integration tests")
		os.Exit(0)
	}

	ctx := context.Background()

	maintDSN, testDSN, err := deriveDSNs(prodDSN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "derive test DSN: %v\n", err)
		os.Exit(1)
	}
	if err := ensureTestDB(ctx, maintDSN); err != nil {
		fmt.Fprintf(os.Stderr, "ensure test DB: %v\n", err)
		os.Exit(1)
	}

	os.Setenv("DATABASE_URL", testDSN)
	if err := store.Connect(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "store.Connect (test DB): %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	if err := store.Migrate(ctx, findMigrationsDir()); err != nil {
		fmt.Fprintf(os.Stderr, "migrate test DB: %v\n", err)
		os.Exit(1)
	}

	// Truncate feedback tables in our isolated DB so each `go test`
	// invocation starts from a clean, deterministic slate. This is
	// scoped to our own DB (tradeengine_feedback_test); it does not
	// touch internal/store's tradeengine_test.
	if err := truncateFeedbackTables(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "truncate feedback tables: %v\n", err)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

func truncateFeedbackTables(ctx context.Context) error {
	for _, tbl := range []string{"feedback_insights", "feedback_flags"} {
		if _, err := store.GetDB().Exec(ctx,
			fmt.Sprintf("TRUNCATE TABLE %s RESTART IDENTITY CASCADE", tbl),
		); err != nil {
			// Table may not exist in older schemas — log and continue
			// rather than fail the whole suite.
			fmt.Fprintf(os.Stderr, "truncate %s: %v\n", tbl, err)
		}
	}
	return nil
}

func deriveDSNs(prodDSN string) (string, string, error) {
	u, err := url.Parse(prodDSN)
	if err != nil {
		return "", "", fmt.Errorf("parse DSN: %w", err)
	}
	u.Path = "/postgres"
	maint := u.String()
	u.Path = "/" + testDBName
	test := u.String()
	return maint, test, nil
}

func ensureTestDB(ctx context.Context, maintDSN string) error {
	conn, err := pgx.Connect(ctx, maintDSN)
	if err != nil {
		return fmt.Errorf("connect to postgres DB: %w", err)
	}
	defer conn.Close(ctx)

	var exists bool
	if err := conn.QueryRow(ctx,
		"SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)", testDBName,
	).Scan(&exists); err != nil {
		return fmt.Errorf("check DB exists: %w", err)
	}
	if !exists {
		if _, err := conn.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s", testDBName)); err != nil {
			return fmt.Errorf("create test DB: %w", err)
		}
	}
	return nil
}

func findMigrationsDir() string {
	_, filename, _, _ := runtime.Caller(0)
	dir := filepath.Dir(filename)
	for i := 0; i < 6; i++ {
		candidate := filepath.Join(dir, "migrations")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
		dir = filepath.Dir(dir)
	}
	return "migrations"
}

// ─── Test helpers ────────────────────────────────────────────────────────────

// uniqueTicker returns a per-test ticker that won't collide with rows
// other tests in the suite may have left behind.
func uniqueTicker(prefix string) string {
	return prefix + "_" + strings.ReplaceAll(time.Now().Format("150405.000"), ".", "")
}

// today returns the date GetPerformanceContext queries against (UTC midnight).
func today() time.Time {
	return time.Now().UTC().Truncate(24 * time.Hour)
}

// cleanupInsights deletes seeded insights so each test starts clean.
func cleanupInsights(t *testing.T, ctx context.Context, scope, scopeKey string) {
	t.Helper()
	_, err := store.GetDB().Exec(ctx,
		`DELETE FROM feedback_insights WHERE scope=$1 AND scope_key=$2`,
		scope, scopeKey)
	if err != nil {
		t.Logf("cleanup feedback_insights (scope=%s key=%s): %v", scope, scopeKey, err)
	}
}

// purgeModelInsightsForDate clears every model-scope row for a given
// date so we can isolate the "model rows only" assertion from any
// rows other tests in the suite may have written.
func purgeModelInsightsForDate(t *testing.T, ctx context.Context, date time.Time) {
	t.Helper()
	_, err := store.GetDB().Exec(ctx,
		`DELETE FROM feedback_insights WHERE scope='model' AND date=$1`, date)
	if err != nil {
		t.Logf("purge model insights for %s: %v", date.Format("2006-01-02"), err)
	}
}

// ─── Tests ──────────────────────────────────────────────────────────────────

// TestGetPerformanceContext_EmptyDB pins the canonical empty value:
// when no insights exist for the ticker AND no model insights exist
// in the 7-day search window, the function must return ("", nil) so
// callers can no-op without special-casing errors.
func TestGetPerformanceContext_EmptyDB(t *testing.T) {
	ctx := context.Background()
	ticker := uniqueTicker("TEST_FB_EMPTY")

	// Nothing seeded for this ticker. The model-scope search may pick
	// up rows from other tests, so wipe a 7-day window of model rows
	// to make this assertion deterministic.
	for d := 0; d < 7; d++ {
		purgeModelInsightsForDate(t, ctx, today().AddDate(0, 0, -d))
	}

	got, err := feedback.GetPerformanceContext(ctx, ticker)
	if err != nil {
		t.Fatalf("GetPerformanceContext: %v", err)
	}
	if got != "" {
		t.Errorf("empty DB: want \"\", got %q", got)
	}
}

// TestGetPerformanceContext_OnlyModelScope verifies the model block
// renders when only model-scope rows exist for today, and crucially
// that NO instrument-specific section for the queried ticker appears.
func TestGetPerformanceContext_OnlyModelScope(t *testing.T) {
	ctx := context.Background()
	ticker := uniqueTicker("TEST_FB_MODEL")
	modelKey := "anthropic/claude-test-" + uniqueTicker("M")

	// Clean up any prior rows for this exact model key plus today's
	// model-scope window so the assertion is deterministic.
	cleanupInsights(t, ctx, "model", modelKey)
	purgeModelInsightsForDate(t, ctx, today())
	t.Cleanup(func() {
		cleanupInsights(t, ctx, "model", modelKey)
	})

	if err := store.UpsertFeedbackInsight(ctx, store.FeedbackInsight{
		Date:       today(),
		Scope:      "model",
		ScopeKey:   modelKey,
		Metric:     "win_rate",
		Value:      0.62,
		Window:     "30d",
		TradeCount: 0,
	}); err != nil {
		t.Fatalf("UpsertFeedbackInsight win_rate: %v", err)
	}
	if err := store.UpsertFeedbackInsight(ctx, store.FeedbackInsight{
		Date:       today(),
		Scope:      "model",
		ScopeKey:   modelKey,
		Metric:     "trade_count",
		Value:      0,
		Window:     "30d",
		TradeCount: 25,
	}); err != nil {
		t.Fatalf("UpsertFeedbackInsight trade_count: %v", err)
	}

	got, err := feedback.GetPerformanceContext(ctx, ticker)
	if err != nil {
		t.Fatalf("GetPerformanceContext: %v", err)
	}
	if got == "" {
		t.Fatal("only-model-rows: want non-empty, got \"\"")
	}
	if !strings.Contains(got, modelKey) {
		t.Errorf("output missing model key %q; got=%q", modelKey, got)
	}
	// No instrument section means the ticker label should not appear
	// as the leading bullet of an instrument line.
	instrLine := "- " + ticker + ":"
	if strings.Contains(got, instrLine) {
		t.Errorf("output unexpectedly contains instrument section for %q; got=%q",
			ticker, got)
	}
}

// TestGetPerformanceContext_InstrumentScope verifies that an instrument-scope
// row for the queried ticker produces an instrument-specific section AND
// that the model section still renders when both kinds exist.
func TestGetPerformanceContext_InstrumentScope(t *testing.T) {
	ctx := context.Background()
	ticker := uniqueTicker("TEST_FB_INSTR")
	modelKey := "anthropic/claude-test-" + uniqueTicker("M")

	cleanupInsights(t, ctx, "instrument", ticker)
	cleanupInsights(t, ctx, "model", modelKey)
	purgeModelInsightsForDate(t, ctx, today())
	t.Cleanup(func() {
		cleanupInsights(t, ctx, "instrument", ticker)
		cleanupInsights(t, ctx, "model", modelKey)
	})

	// Instrument-scope rows for the ticker.
	if err := store.UpsertFeedbackInsight(ctx, store.FeedbackInsight{
		Date:       today(),
		Scope:      "instrument",
		ScopeKey:   ticker,
		Metric:     "win_rate",
		Value:      0.55,
		Window:     "30d",
		TradeCount: 0,
	}); err != nil {
		t.Fatalf("UpsertFeedbackInsight instrument win_rate: %v", err)
	}
	if err := store.UpsertFeedbackInsight(ctx, store.FeedbackInsight{
		Date:       today(),
		Scope:      "instrument",
		ScopeKey:   ticker,
		Metric:     "trade_count",
		Value:      0,
		Window:     "30d",
		TradeCount: 11,
	}); err != nil {
		t.Fatalf("UpsertFeedbackInsight instrument trade_count: %v", err)
	}
	if err := store.UpsertFeedbackInsight(ctx, store.FeedbackInsight{
		Date:       today(),
		Scope:      "instrument",
		ScopeKey:   ticker,
		Metric:     "avg_pnl_pct",
		Value:      1.25,
		Window:     "30d",
		TradeCount: 0,
	}); err != nil {
		t.Fatalf("UpsertFeedbackInsight instrument avg_pnl_pct: %v", err)
	}

	// One model-scope row to confirm both sections coexist.
	if err := store.UpsertFeedbackInsight(ctx, store.FeedbackInsight{
		Date:       today(),
		Scope:      "model",
		ScopeKey:   modelKey,
		Metric:     "win_rate",
		Value:      0.70,
		Window:     "30d",
		TradeCount: 0,
	}); err != nil {
		t.Fatalf("UpsertFeedbackInsight model win_rate: %v", err)
	}
	if err := store.UpsertFeedbackInsight(ctx, store.FeedbackInsight{
		Date:       today(),
		Scope:      "model",
		ScopeKey:   modelKey,
		Metric:     "trade_count",
		Value:      0,
		Window:     "30d",
		TradeCount: 40,
	}); err != nil {
		t.Fatalf("UpsertFeedbackInsight model trade_count: %v", err)
	}

	got, err := feedback.GetPerformanceContext(ctx, ticker)
	if err != nil {
		t.Fatalf("GetPerformanceContext: %v", err)
	}
	if got == "" {
		t.Fatal("instrument+model rows: want non-empty, got \"\"")
	}
	// Instrument section.
	if !strings.Contains(got, ticker) {
		t.Errorf("output missing instrument ticker %q; got=%q", ticker, got)
	}
	if !strings.Contains(got, "55% win rate") {
		t.Errorf("output missing instrument win-rate phrasing; got=%q", got)
	}
	// Model section.
	if !strings.Contains(got, modelKey) {
		t.Errorf("output missing model key %q; got=%q", modelKey, got)
	}
}

// TestGetPerformanceContext_DBError closes the pool and asserts the
// function surfaces the underlying error rather than silently returning
// "" — important because callers ignore-on-error on the engine path,
// and a swallowed schema error would mask real outages.
func TestGetPerformanceContext_DBError(t *testing.T) {
	ctx := context.Background()
	ticker := uniqueTicker("TEST_FB_ERR")

	// Close the pool to force the underlying query to fail.
	store.Close()
	// Re-open at end so subsequent tests in the same go test invocation
	// are unaffected (Go test ordering within a package is generally
	// declared-order, but be defensive).
	t.Cleanup(func() {
		if err := store.Connect(context.Background()); err != nil {
			t.Logf("reconnect after DBError test: %v", err)
		}
	})

	_, err := feedback.GetPerformanceContext(ctx, ticker)
	if err == nil {
		t.Fatal("expected error from closed pool, got nil")
	}
}
