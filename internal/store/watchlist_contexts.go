package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/tradeprofile"
)

// WatchlistDiscoveryContext is one independently expiring reason a ticker is
// active. It is provenance only; it does not define an execution strategy.
type WatchlistDiscoveryContext struct {
	Key       string
	Source    string
	Context   *tradeprofile.DiscoveryContext
	ExpiresAt *time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// UpsertWatchlistDiscoveryContext attaches or refreshes a named context on an
// existing ticker without changing the watchlist row's owner, strategies, or
// expiry. This is what lets earnings coexist with another discovery source.
func UpsertWatchlistDiscoveryContext(
	ctx context.Context, ticker, platform, key string,
	value tradeprofile.DiscoveryContext, expiresAt *time.Time,
) error {
	if platform == "" {
		platform = "stock"
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return fmt.Errorf("watchlist discovery context key is required")
	}
	canonical, err := value.Canonicalize()
	if err != nil {
		return fmt.Errorf("canonicalize watchlist discovery context: %w", err)
	}
	raw, err := tradeprofile.MarshalDiscoveryContext(&canonical)
	if err != nil {
		return err
	}
	tag, err := GetDB().Exec(ctx, `
		INSERT INTO watchlist_discovery_contexts
			(watchlist_id, context_key, source, context, expires_at, active)
		SELECT id, $3, $4, $5::jsonb, $6, true
		FROM watchlist
		WHERE ticker=$1 AND platform=$2
		ON CONFLICT (watchlist_id, context_key) DO UPDATE SET
			source=EXCLUDED.source, context=EXCLUDED.context, expires_at=EXCLUDED.expires_at,
			active=true, updated_at=NOW()`,
		strings.ToUpper(strings.TrimSpace(ticker)), platform, key,
		canonical.Sources[0], raw, expiresAt,
	)
	if err != nil {
		return fmt.Errorf("upsert watchlist discovery context: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("watchlist %s/%s does not exist", ticker, platform)
	}
	return nil
}

func loadActiveWatchlistDiscoveryContexts(ctx context.Context, item *WatchlistItem) error {
	rows, err := GetDB().Query(ctx, `
		SELECT context_key, source, context, expires_at, created_at, updated_at
		FROM watchlist_discovery_contexts
		WHERE watchlist_id=$1 AND active=true AND (expires_at IS NULL OR expires_at > NOW())
		ORDER BY created_at ASC, context_key ASC`, item.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var value WatchlistDiscoveryContext
		var raw []byte
		if err := rows.Scan(&value.Key, &value.Source, &raw, &value.ExpiresAt, &value.CreatedAt, &value.UpdatedAt); err != nil {
			return err
		}
		decoded, err := tradeprofile.DecodeDiscoveryContext(raw)
		if err != nil {
			// The migration can enforce JSON shape but not a future contract
			// version. A bad additive context must not stop every watchlist item
			// in this engine tick; its valid siblings remain usable.
			continue
		}
		value.Context = decoded
		item.DiscoveryContexts = append(item.DiscoveryContexts, value)
	}
	return rows.Err()
}

func loadActiveWatchlistDiscoveryContextsBatch(ctx context.Context, items []WatchlistItem) error {
	if len(items) == 0 {
		return nil
	}
	ids := make([]string, 0, len(items))
	byID := make(map[string]int, len(items))
	for i := range items {
		ids = append(ids, items[i].ID)
		byID[items[i].ID] = i
	}
	rows, err := GetDB().Query(ctx, `
		SELECT watchlist_id::text, context_key, source, context, expires_at, created_at, updated_at
		FROM watchlist_discovery_contexts
		WHERE watchlist_id = ANY($1::uuid[]) AND active=true AND (expires_at IS NULL OR expires_at > NOW())
		ORDER BY created_at ASC, context_key ASC`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var value WatchlistDiscoveryContext
		var raw []byte
		if err := rows.Scan(&id, &value.Key, &value.Source, &raw, &value.ExpiresAt, &value.CreatedAt, &value.UpdatedAt); err != nil {
			return err
		}
		decoded, err := tradeprofile.DecodeDiscoveryContext(raw)
		if err != nil {
			// Isolate malformed provenance to this context rather than aborting
			// the complete active-watchlist load.
			continue
		}
		value.Context = decoded
		if index, ok := byID[id]; ok {
			items[index].DiscoveryContexts = append(items[index].DiscoveryContexts, value)
		}
	}
	return rows.Err()
}

// pruneExpiredSourceLeaseEvidenceBatch keeps persisted provenance as history
// but prevents expired source evidence from entering analyst-facing contexts.
// Lease-less legacy scanner rows retain their scalar-TTL compatibility path.
func pruneExpiredSourceLeaseEvidenceBatch(ctx context.Context, items []WatchlistItem) error {
	if len(items) == 0 {
		return nil
	}
	ids := make([]string, 0, len(items))
	byID := make(map[string]int, len(items))
	for i := range items {
		if !items[i].ScannerOrigin {
			continue
		}
		ids = append(ids, items[i].ID)
		byID[items[i].ID] = i
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := GetDB().Query(ctx, `
		SELECT watchlist_id::text, source, expires_at > NOW()
		FROM watchlist_source_leases WHERE watchlist_id = ANY($1::uuid[])`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	leaseBacked := make(map[string]bool)
	live := make(map[string]map[string]bool)
	for rows.Next() {
		var id, source string
		var active bool
		if err := rows.Scan(&id, &source, &active); err != nil {
			return err
		}
		leaseBacked[id] = true
		if active {
			if live[id] == nil {
				live[id] = make(map[string]bool)
			}
			live[id][source] = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for id, index := range byID {
		if leaseBacked[id] {
			filtered, pruned := filterDiscoveryContextSources(items[index].DiscoveryContext, live[id])
			items[index].DiscoveryContext = filtered
			if pruned {
				// The original thesis/graph wording may describe an expired source.
				// Preserve only source-scoped facts that remain live.
				items[index].GraphContext = ""
			}
		}
	}
	return nil
}

func filterDiscoveryContextSources(value *tradeprofile.DiscoveryContext, live map[string]bool) (*tradeprofile.DiscoveryContext, bool) {
	if value == nil {
		return nil, false
	}
	filtered := *value
	filtered.Sources = make([]string, 0, len(value.Sources))
	filtered.EvidenceBySource = make(map[string]json.RawMessage, len(value.EvidenceBySource))
	pruned := false
	for _, source := range value.Sources {
		if !live[source] {
			pruned = true
			continue
		}
		filtered.Sources = append(filtered.Sources, source)
		if evidence, ok := value.EvidenceBySource[source]; ok {
			filtered.EvidenceBySource[source] = append(json.RawMessage(nil), evidence...)
		}
	}
	if pruned {
		if len(filtered.Sources) == 0 {
			return nil, true
		}
		filtered.Thesis = "Active source evidence: " + strings.Join(filtered.Sources, ", ")
	}
	return &filtered, pruned
}

// ExpireWatchlistDiscoveryContexts ends due contexts. Earnings-only rows are
// deactivated once their final context expires; positions remain managed by the
// ordinary position lifecycle and are never touched here.
func ExpireWatchlistDiscoveryContexts(ctx context.Context) error {
	tx, err := GetDB().Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `
		UPDATE watchlist_discovery_contexts SET active=false, updated_at=NOW()
		WHERE active=true AND expires_at IS NOT NULL AND expires_at <= NOW()`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE watchlist w SET active=false, updated_at=NOW()
		WHERE w.active=true AND w.source='earnings_watchlist'
		AND NOT EXISTS (
			SELECT 1 FROM watchlist_discovery_contexts c
			WHERE c.watchlist_id=w.id AND c.active=true AND (c.expires_at IS NULL OR c.expires_at > NOW())
		)`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
