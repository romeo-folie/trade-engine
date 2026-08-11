package store_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/store"
)

func TestMigration081BackfillsOnlyUnambiguousFuturesSessions(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()
	db := store.GetDB()
	base := time.Date(2091, 1, 1, 0, 0, 0, 0, time.UTC)
	type fixture struct {
		name       string
		day        int
		candidates string
		selected   []string
		want       string
		collision  bool
		nullSymbol bool
		withTrade  bool
	}
	fixtures := []fixture{
		{name: "selected index", day: 0, selected: []string{"MNQ", "MYM"}, want: "futures_index_orb"},
		{name: "candidate metals", day: 1, candidates: `[{"gap":{"Ticker":"MGC"}}]`, want: "futures_metals_orb"},
		{name: "mixed", day: 2, selected: []string{"MNQ", "MGC"}, want: "equity_orb"},
		{name: "empty", day: 3, want: "equity_orb"},
		{name: "target owner collision", day: 4, selected: []string{"MNQ"}, want: "equity_orb", collision: true},
		{name: "recognized plus blank candidate", day: 5, selected: []string{"MNQ"}, candidates: `[{"score":{"summary":"missing gap identity"}}]`, want: "equity_orb"},
		{name: "recognized plus null selected", day: 6, selected: []string{"MNQ"}, want: "equity_orb", nullSymbol: true},
		{name: "session with trade", day: 7, selected: []string{"MNQ"}, want: "equity_orb", withTrade: true},
	}
	ids := make(map[string]string, len(fixtures))
	cleanupIDs := make([]string, 0, len(fixtures)+1)
	t.Cleanup(func() {
		if len(cleanupIDs) == 0 || !store.IsConnected() {
			return
		}
		if _, err := store.GetDB().Exec(context.Background(),
			`DELETE FROM orb_sessions WHERE id = ANY($1::uuid[])`, cleanupIDs,
		); err != nil {
			t.Errorf("cleanup migration 081 fixtures: %v", err)
		}
	})
	for _, f := range fixtures {
		date := base.AddDate(0, 0, f.day)
		if f.collision {
			var collisionID string
			if err := db.QueryRow(ctx, `
				INSERT INTO orb_sessions (date,status,session_name,candidates,selected,asset_class,owner_scope)
				VALUES ($1,'done','futures','[]'::jsonb,ARRAY['MNQ'],'futures','futures_index_orb')
				RETURNING id`, date).Scan(&collisionID); err != nil {
				t.Fatalf("%s: seed collision: %v", f.name, err)
			}
			cleanupIDs = append(cleanupIDs, collisionID)
		}
		var id string
		if err := db.QueryRow(ctx, `
			INSERT INTO orb_sessions (date,status,session_name,candidates,selected,asset_class,owner_scope)
			VALUES ($1,'done','futures',COALESCE(NULLIF($2,''),'[]')::jsonb,$3,'futures','equity_orb')
			RETURNING id`, date, f.candidates, f.selected).Scan(&id); err != nil {
			t.Fatalf("%s: seed session: %v", f.name, err)
		}
		ids[f.name] = id
		cleanupIDs = append(cleanupIDs, id)
		if f.nullSymbol {
			if _, err := db.Exec(ctx, `UPDATE orb_sessions SET selected=ARRAY['MNQ',NULL]::text[] WHERE id=$1`, id); err != nil {
				t.Fatalf("%s: add null selected evidence: %v", f.name, err)
			}
		}
		if f.withTrade {
			if _, err := db.Exec(ctx, `
				INSERT INTO orb_trades (session_id,ticker,side,status,asset_class,owner_scope)
				VALUES ($1,'MNQ','buy','pending','futures','equity_orb')`, id); err != nil {
				t.Fatalf("%s: seed existing trade: %v", f.name, err)
			}
		}
	}

	sql, err := os.ReadFile(filepath.Join(findMigrationsDir(), "081_orb_session_owner_backfill.sql"))
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	for run := 1; run <= 2; run++ {
		if _, err := db.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("migration replay %d: %v", run, err)
		}
	}
	for _, f := range fixtures {
		var got string
		if err := db.QueryRow(ctx, `SELECT owner_scope FROM orb_sessions WHERE id=$1`, ids[f.name]).Scan(&got); err != nil {
			t.Fatalf("%s: read owner: %v", f.name, err)
		}
		if got != f.want {
			t.Errorf("%s owner_scope = %q, want %q", f.name, got, f.want)
		}
	}
}
