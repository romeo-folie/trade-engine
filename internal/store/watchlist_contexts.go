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

// RenewDiscoveryResidencyAfterTrade extends still-live earnings and political-
// disclosure reasons through the third market-session close containing the
// execution. A reason already expired when recovery runs is deliberately not
// reactivated; an open position remains monitored independently.
func RenewDiscoveryResidencyAfterTrade(ctx context.Context, ticker string, expiresAt time.Time) error {
	ticker = strings.ToUpper(strings.TrimSpace(ticker))
	if ticker == "" || expiresAt.IsZero() {
		return fmt.Errorf("discovery trade renewal requires ticker and expiry")
	}
	tx, err := GetDB().Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := lockScannerWatchlistItem(ctx, tx, WatchlistItem{Ticker: ticker, Platform: "stock"}); err != nil {
		return err
	}
	if err := lockResearchQueueTx(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE watchlist_discovery_contexts c
		SET expires_at=GREATEST(c.expires_at, $2), updated_at=NOW()
		FROM watchlist w
		WHERE c.watchlist_id=w.id AND w.ticker=$1 AND w.platform='stock' AND w.active
		  AND c.source='earnings_scheduled' AND c.active AND c.expires_at > NOW()`, ticker, expiresAt); err != nil {
		return fmt.Errorf("renew live earnings discovery context after trade: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE watchlist_source_leases l
		SET expires_at=GREATEST(l.expires_at, $2), updated_at=NOW()
		FROM watchlist w
		WHERE l.watchlist_id=w.id AND w.ticker=$1 AND w.platform='stock' AND w.active
		  AND l.source='political_disclosure' AND l.expires_at > NOW()`, ticker, expiresAt); err != nil {
		return fmt.Errorf("renew live political-disclosure lease after trade: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE watchlist w
		SET expires_at=(SELECT MAX(l.expires_at) FROM watchlist_source_leases l WHERE l.watchlist_id=w.id),
		    updated_at=NOW()
		WHERE w.ticker=$1 AND w.platform='stock' AND w.active AND w.source='scanner'
		  AND EXISTS (SELECT 1 FROM watchlist_source_leases l WHERE l.watchlist_id=w.id)`, ticker); err != nil {
		return fmt.Errorf("refresh watchlist lease expiry after trade: %w", err)
	}
	if err := ReclassifyResearchQueueMemberships(ctx, tx); err != nil {
		return fmt.Errorf("reclassify research queue after trade renewal: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit discovery trade renewal: %w", err)
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
				// Discovery and execution are a durable pair. Once the final
				// lease-backed source is gone, do not expose execution routing with
				// no discovery contract to justify it.
				if filtered == nil {
					items[index].ExecutionProfile = nil
				}
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
		filtered.Thesis = mergedDiscoveryThesis("", "", filtered.Sources, filtered.EvidenceBySource)
		if filtered.Thesis == "" {
			filtered.Thesis = "Active source evidence: " + strings.Join(filtered.Sources, ", ")
		}
	}
	return &filtered, pruned
}

// ExpireWatchlistDiscoveryContexts ends due contexts. A shared scanner/earnings
// row remains active while it has either live scanner evidence or a live
// discovery context; it is deactivated only after its final retaining reason
// expires. Positions and manual/config watchlist rows are never touched here.
func ExpireWatchlistDiscoveryContexts(ctx context.Context) error {
	tx, err := GetDB().Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := expireWatchlistDiscoveryContextsTx(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	// Do this after the expiry writes commit so queue serialization never waits
	// while holding watchlist/context row locks. Reclassification also releases
	// inactive memberships under that lock, preventing an admission from
	// observing a membership that expiry deletes concurrently.
	queueTx, err := GetDB().Begin(ctx)
	if err != nil {
		return err
	}
	defer queueTx.Rollback(ctx) //nolint:errcheck
	if err := ReclassifyResearchQueueMemberships(ctx, queueTx); err != nil {
		return err
	}
	return queueTx.Commit(ctx)
}

// expireWatchlistDiscoveryContextsTx performs expiry's row/context phase. It
// deliberately leaves research-queue membership release to the post-commit,
// queue-serialized reclassification phase in ExpireWatchlistDiscoveryContexts.
func expireWatchlistDiscoveryContextsTx(ctx context.Context, tx watchlistExecer) error {
	if _, err := tx.Exec(ctx, `
		UPDATE watchlist_discovery_contexts SET active=false, updated_at=NOW()
		WHERE active=true AND expires_at IS NOT NULL AND expires_at <= NOW()`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE watchlist w SET active=false, updated_at=NOW()
		WHERE w.active=true AND w.source IN ('scanner', 'earnings_watchlist')
		AND (
			w.source='earnings_watchlist'
			OR (
				(EXISTS (SELECT 1 FROM watchlist_source_leases l WHERE l.watchlist_id=w.id)
				 AND NOT EXISTS (SELECT 1 FROM watchlist_source_leases l WHERE l.watchlist_id=w.id AND l.expires_at > NOW()))
				OR
				(NOT EXISTS (SELECT 1 FROM watchlist_source_leases l WHERE l.watchlist_id=w.id)
				 AND w.expires_at IS NOT NULL AND w.expires_at <= NOW())
			)
		)
		AND NOT EXISTS (
			SELECT 1 FROM watchlist_discovery_contexts c
			WHERE c.watchlist_id=w.id AND c.active=true AND (c.expires_at IS NULL OR c.expires_at > NOW())
		)`); err != nil {
		return err
	}
	return nil
}
