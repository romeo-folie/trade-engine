package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/romeo-folie/trade-engine/internal/tradeprofile"
)

var (
	ErrWatchlistRequalificationRequired = errors.New("watchlist item requires pullback requalification")
	// ErrWatchlistActiveItemExists means a fallback write lost a race to an
	// already-active watchlist entry. The caller must leave that newer entry
	// untouched rather than replacing its strategy or scanner lineage.
	ErrWatchlistActiveItemExists = errors.New("active watchlist item already exists")
	// ErrWatchlistRefreshNotActive means a candidate that was active in a
	// scanner snapshot expired or was removed before its refresh transaction.
	// The caller may send it through normal admission/cap selection instead.
	ErrWatchlistRefreshNotActive = errors.New("watchlist item is no longer active for refresh")
)

// WatchlistItem represents a row in the watchlist table.
type WatchlistItem struct {
	ID            string
	Ticker        string
	Platform      string // "stock"
	Strategies    []string
	Timeframes    []string
	MinConfidence float64
	Source        string // "manual" | "config" | "scanner"
	// GraphContext records WHY a scanner candidate was sourced (graph link or
	// news-sympathy trigger, migration 038) so the engine's deep-analysis
	// prompt can carry the origin thesis. Empty for manual/config items and
	// plain screener finds.
	GraphContext string
	// DiscoveryContext is the versioned scanner provenance contract. Nil keeps
	// legacy rows on their existing graph_context/strategies compatibility path.
	DiscoveryContext *tradeprofile.DiscoveryContext
	// ExecutionProfile is the versioned bounded lens/timeframe contract for a
	// scanner promotion. It intentionally remains separate from Strategies,
	// which is retained for legacy configuration and entry tags.
	ExecutionProfile *tradeprofile.ExecutionProfile
	// DiscoveryContexts are additive, independently expiring provenance records.
	// They coexist with the legacy single scanner contract above.
	DiscoveryContexts []WatchlistDiscoveryContext
	// ScannerRunID links scanner-sourced rows to their prospective universe
	// snapshot. Empty for manual/config watchlist items.
	ScannerRunID string
	Active       bool
	ExpiresAt    *time.Time // non-nil for scanner-sourced items
	// SourceLeases are the current qualifying observations that justify scanner
	// residency. They are source-owned and must not replace the admission-time
	// execution contract when a ticker is rediscovered.
	SourceLeases []WatchlistSourceLease
	// PullbackRequalificationRequired quarantines legacy scanner rows whose
	// pullback-universe status was not persisted before migration 055.
	PullbackRequalificationRequired bool
	// PullbackRequalificationComplete is a write-only acknowledgement from the
	// scanner that all objective universe inputs were available. It permits a
	// quarantined row to be reactivated as either qualified or standard.
	PullbackRequalificationComplete bool
	CreatedAt                       time.Time
	UpdatedAt                       time.Time
}

// RefreshActiveScannerWatchlistItem updates qualifying source leases only when
// the existing scanner row is still live. It never reactivates a ticker or
// replaces its admission-time execution contract.
func RefreshActiveScannerWatchlistItem(
	ctx context.Context, item WatchlistItem, lease *ScannerCycleLease,
) (bool, error) {
	if item.Source != "scanner" || len(item.SourceLeases) == 0 {
		return false, errors.New("scanner source leases are required for refresh")
	}
	if err := validateWatchlistSourceLeases(item.SourceLeases); err != nil {
		return false, err
	}
	tx, err := GetDB().Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := lockScannerWatchlistItem(ctx, tx, item); err != nil {
		return false, err
	}
	if lease != nil {
		if err := renewScannerCycleTx(ctx, tx, *lease); err != nil {
			return false, err
		}
	}
	var id string
	err = tx.QueryRow(ctx, `
		SELECT id::text FROM watchlist w
		WHERE ticker=$1 AND platform=$2 AND source='scanner' AND active
		  AND (EXISTS (SELECT 1 FROM watchlist_source_leases l WHERE l.watchlist_id=w.id AND l.expires_at > NOW())
		       OR (NOT EXISTS (SELECT 1 FROM watchlist_source_leases l WHERE l.watchlist_id=w.id)
		           AND (expires_at IS NULL OR expires_at > NOW())))
		FOR UPDATE`, strings.ToUpper(strings.TrimSpace(item.Ticker)), item.Platform,
	).Scan(&id)
	if isNoRows(err) {
		return false, ErrWatchlistRefreshNotActive
	}
	if err != nil {
		return false, err
	}
	changed, err := refreshWatchlistSourceLeases(ctx, tx, item)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return changed, nil
}

// WatchlistSourceLease is a scanner source's current qualifying observation.
// EvidenceFingerprint is source-owned; an identical fingerprint is a retry,
// not a rediscovery, and must never extend the lease.
type WatchlistSourceLease struct {
	Source              string
	EvidenceFingerprint string
	QualifiedAt         time.Time
	ObservedAt          time.Time
	EffectiveAt         time.Time
	ExpiresAt           time.Time
	Evidence            json.RawMessage
}

// EffectiveDiscoveryContext merges the legacy scanner contract with additive
// source contexts for the analyst prompt. It never changes execution routing.
func (item WatchlistItem) EffectiveDiscoveryContext() *tradeprofile.DiscoveryContext {
	contexts := make([]*tradeprofile.DiscoveryContext, 0, 1+len(item.DiscoveryContexts))
	contexts = append(contexts, item.DiscoveryContext)
	for _, value := range item.DiscoveryContexts {
		contexts = append(contexts, value.Context)
	}
	return tradeprofile.MergeDiscoveryContexts(contexts...)
}

// AddWatchlistItem inserts or re-activates a watchlist entry.
// If the ticker+platform pair already exists and was disabled, it is re-enabled.
// For scanner-sourced items, ExpiresAt is written and updated on re-activation.
func AddWatchlistItem(ctx context.Context, item WatchlistItem) error {
	if len(item.SourceLeases) > 0 {
		return addWatchlistItemInTransaction(ctx, item, false)
	}
	return addWatchlistItem(ctx, GetDB(), item, false)
}

// AddWatchlistItemWithScannerLease fences a scanner promotion and its lease
// renewal in one transaction.
func AddWatchlistItemWithScannerLease(
	ctx context.Context, item WatchlistItem, lease ScannerCycleLease,
) error {
	return addWatchlistItemWithScannerLease(ctx, item, lease, false)
}

// AddWatchlistItemUnlessActive inserts a watchlist item, or reactivates an
// inactive/expired item, without replacing a currently active item. It is used
// after a weekly-leader claim loss, where another worker may have promoted the
// same ticker between the failed fenced promotion and the ordinary fallback.
func AddWatchlistItemUnlessActive(ctx context.Context, item WatchlistItem) error {
	if len(item.SourceLeases) > 0 {
		return addWatchlistItemInTransaction(ctx, item, true)
	}
	return addWatchlistItem(ctx, GetDB(), item, true)
}

func addWatchlistItemInTransaction(ctx context.Context, item WatchlistItem, preserveActive bool) error {
	tx, err := GetDB().Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := addWatchlistItem(ctx, tx, item, preserveActive); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// AddWatchlistItemUnlessActiveWithScannerLease is the fenced fallback variant.
func AddWatchlistItemUnlessActiveWithScannerLease(
	ctx context.Context, item WatchlistItem, lease ScannerCycleLease,
) error {
	return addWatchlistItemWithScannerLease(ctx, item, lease, true)
}

func addWatchlistItemWithScannerLease(
	ctx context.Context, item WatchlistItem, lease ScannerCycleLease,
	preserveActive bool,
) error {
	tx, err := GetDB().Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := lockScannerWatchlistItem(ctx, tx, item); err != nil {
		return err
	}
	if err := renewScannerCycleTx(ctx, tx, lease); err != nil {
		return err
	}
	if err := addWatchlistItem(ctx, tx, item, preserveActive); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// lockScannerWatchlistItem serializes scanner writes for one ticker and waits
// for an existing row lock before the cycle lease is renewed. Any contention
// therefore happens while another worker can still reclaim an expired lease.
func lockScannerWatchlistItem(ctx context.Context, tx pgx.Tx, item WatchlistItem) error {
	platform := strings.TrimSpace(item.Platform)
	if platform == "" {
		platform = "stock"
	}
	ticker := strings.ToUpper(strings.TrimSpace(item.Ticker))
	if ticker == "" {
		return fmt.Errorf("watchlist ticker is required")
	}
	if _, err := tx.Exec(ctx, `
		SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		"scanner-watchlist:"+platform+":"+ticker); err != nil {
		return err
	}
	var id string
	err := tx.QueryRow(ctx, `
		SELECT id FROM watchlist WHERE ticker=$1 AND platform=$2 FOR UPDATE`,
		ticker, platform).Scan(&id)
	if err != nil && !isNoRows(err) {
		return err
	}
	return nil
}

type watchlistExecer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func addWatchlistItem(
	ctx context.Context, db watchlistExecer, item WatchlistItem,
	preserveActive bool,
) error {
	if (item.DiscoveryContext == nil) != (item.ExecutionProfile == nil) {
		return errors.New("watchlist discovery context and execution profile must be supplied together")
	}
	if item.Platform == "" {
		item.Platform = "stock"
	}
	if item.Source == "" {
		item.Source = "manual"
	}
	if item.Source == "scanner" && item.DiscoveryContext != nil && item.ScannerRunID == "" {
		return errors.New("scanner watchlist discovery context and execution profile require scanner run ID")
	}
	if err := validateWatchlistSourceLeases(item.SourceLeases); err != nil {
		return err
	}
	discoveryContext, err := tradeprofile.MarshalDiscoveryContext(item.DiscoveryContext)
	if err != nil {
		return fmt.Errorf("marshal watchlist discovery context: %w", err)
	}
	executionProfile, err := tradeprofile.MarshalExecutionProfile(item.ExecutionProfile)
	if err != nil {
		return fmt.Errorf("marshal watchlist execution profile: %w", err)
	}
	preserveCurrentScanner := preserveActive || (item.Source == "scanner" && len(item.SourceLeases) > 0)
	tag, err := db.Exec(ctx, `
		INSERT INTO watchlist (
			ticker, platform, strategies, timeframes, min_confidence, source,
			graph_context, discovery_context, execution_profile, scanner_run_id,
			active, expires_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, true, $11)
		ON CONFLICT (ticker, platform) DO UPDATE SET
			-- A manual/config refresh of an active scanner profile must retain the
			-- scheduling fields with the rest of the scanner execution contract.
			strategies = CASE
				WHEN EXCLUDED.discovery_context IS NULL
					AND watchlist.discovery_context IS NOT NULL
					AND watchlist.execution_profile IS NOT NULL
					AND watchlist.active
					AND (watchlist.expires_at IS NULL OR watchlist.expires_at > NOW())
					THEN watchlist.strategies
				ELSE EXCLUDED.strategies
			END,
			timeframes = CASE
				WHEN EXCLUDED.discovery_context IS NULL
					AND watchlist.discovery_context IS NOT NULL
					AND watchlist.execution_profile IS NOT NULL
					AND watchlist.active
					AND (watchlist.expires_at IS NULL OR watchlist.expires_at > NOW())
					THEN watchlist.timeframes
				ELSE EXCLUDED.timeframes
			END,
			-- A manual/config refresh of an active scanner profile must retain the
			-- complete scanner execution contract, including its confidence floor.
			-- Once that contract has expired (or was deactivated), the refresh is a
			-- normal replacement and may install its own confidence value.
			min_confidence = CASE
				WHEN EXCLUDED.discovery_context IS NULL
					AND watchlist.discovery_context IS NOT NULL
					AND watchlist.execution_profile IS NOT NULL
					AND watchlist.active
					AND (watchlist.expires_at IS NULL OR watchlist.expires_at > NOW())
					THEN watchlist.min_confidence
				ELSE EXCLUDED.min_confidence
			END,
			source = CASE
				WHEN EXCLUDED.discovery_context IS NULL
					AND watchlist.discovery_context IS NOT NULL
					AND watchlist.execution_profile IS NOT NULL
					AND watchlist.active
					AND (watchlist.expires_at IS NULL OR watchlist.expires_at > NOW())
					THEN watchlist.source
				ELSE EXCLUDED.source
			END,
			graph_context = CASE
				WHEN EXCLUDED.discovery_context IS NULL
					AND watchlist.discovery_context IS NOT NULL
					AND watchlist.execution_profile IS NOT NULL
					AND watchlist.active
					AND (watchlist.expires_at IS NULL OR watchlist.expires_at > NOW())
					THEN watchlist.graph_context
				ELSE EXCLUDED.graph_context
			END,
			discovery_context = CASE
				WHEN EXCLUDED.discovery_context IS NULL
					AND watchlist.discovery_context IS NOT NULL
					AND watchlist.execution_profile IS NOT NULL
					AND watchlist.active
					AND (watchlist.expires_at IS NULL OR watchlist.expires_at > NOW())
					THEN watchlist.discovery_context
				ELSE EXCLUDED.discovery_context
			END,
			execution_profile = CASE
				WHEN EXCLUDED.discovery_context IS NULL
					AND watchlist.discovery_context IS NOT NULL
					AND watchlist.execution_profile IS NOT NULL
					AND watchlist.active
					AND (watchlist.expires_at IS NULL OR watchlist.expires_at > NOW())
					THEN watchlist.execution_profile
				ELSE EXCLUDED.execution_profile
			END,
			scanner_run_id = CASE
				WHEN EXCLUDED.discovery_context IS NULL
					AND watchlist.discovery_context IS NOT NULL
					AND watchlist.execution_profile IS NOT NULL
					AND watchlist.active
					AND (watchlist.expires_at IS NULL OR watchlist.expires_at > NOW())
					THEN watchlist.scanner_run_id
				ELSE EXCLUDED.scanner_run_id
			END,
			active         = true,
			-- A legacy/manual refresh without the complete scanner contract must
			-- preserve the whole provenance envelope. In particular, clearing the
			-- scanner TTL here would turn a bounded scanner candidate into an
			-- indefinitely-active watchlist row while its source/profile are still
			-- retained above.
			expires_at = CASE
				WHEN EXCLUDED.discovery_context IS NULL
					AND watchlist.discovery_context IS NOT NULL
					AND watchlist.execution_profile IS NOT NULL
					AND watchlist.active
					AND (watchlist.expires_at IS NULL OR watchlist.expires_at > NOW())
					THEN watchlist.expires_at
				ELSE EXCLUDED.expires_at
			END,
			pullback_requalification_required = false,
			updated_at     = NOW()
		WHERE NOT (
			watchlist.pullback_requalification_required
			AND EXCLUDED.source = 'scanner'
			AND NOT $12
		)
		AND NOT (
			$13
			AND watchlist.active
			AND (watchlist.expires_at IS NULL OR watchlist.expires_at > NOW())
		)
	`,
		item.Ticker, item.Platform, item.Strategies, item.Timeframes,
		item.MinConfidence, item.Source, nullableString(item.GraphContext),
		discoveryContext, executionProfile, nullableString(item.ScannerRunID), item.ExpiresAt,
		item.PullbackRequalificationComplete, preserveCurrentScanner,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		if preserveCurrentScanner {
			var currentlyActive bool
			err := db.QueryRow(ctx, `
				SELECT active AND (expires_at IS NULL OR expires_at > NOW())
				FROM watchlist
				WHERE ticker=$1 AND platform=$2
			`, item.Ticker, item.Platform).Scan(&currentlyActive)
			if err == nil && currentlyActive {
				if len(item.SourceLeases) > 0 {
					if _, err := refreshWatchlistSourceLeases(ctx, db, item); err != nil {
						return err
					}
				}
				return ErrWatchlistActiveItemExists
			}
		}
		return ErrWatchlistRequalificationRequired
	}
	if len(item.SourceLeases) > 0 {
		if _, err := refreshWatchlistSourceLeases(ctx, db, item); err != nil {
			return err
		}
	}
	return nil
}

func validateWatchlistSourceLeases(leases []WatchlistSourceLease) error {
	seen := make(map[string]struct{}, len(leases))
	for _, lease := range leases {
		lease.Source = strings.ToLower(strings.TrimSpace(lease.Source))
		if lease.Source == "" || strings.TrimSpace(lease.EvidenceFingerprint) == "" ||
			lease.QualifiedAt.IsZero() || lease.ObservedAt.IsZero() ||
			lease.EffectiveAt.IsZero() || lease.ExpiresAt.IsZero() ||
			!lease.ExpiresAt.After(lease.EffectiveAt) || !jsonObject(lease.Evidence) {
			return errors.New("watchlist source lease is incomplete")
		}
		if _, exists := seen[lease.Source]; exists {
			return fmt.Errorf("watchlist source lease %q is duplicated", lease.Source)
		}
		seen[lease.Source] = struct{}{}
	}
	return nil
}

func jsonObject(value json.RawMessage) bool {
	var decoded any
	return json.Unmarshal(value, &decoded) == nil && decoded != nil &&
		len(value) > 0 && value[0] == '{'
}

// refreshWatchlistSourceLeases upserts only materially changed evidence then
// recomputes the legacy expires_at projection. It is called under the same
// transaction and ticker lock as the admission/claim write.
func refreshWatchlistSourceLeases(
	ctx context.Context, db watchlistExecer, item WatchlistItem,
) (bool, error) {
	var watchlistID string
	if err := db.QueryRow(ctx, `
		SELECT id::text FROM watchlist WHERE ticker=$1 AND platform=$2`,
		strings.ToUpper(strings.TrimSpace(item.Ticker)), item.Platform,
	).Scan(&watchlistID); err != nil {
		return false, err
	}
	changed := false
	for _, lease := range item.SourceLeases {
		tag, err := db.Exec(ctx, `
			INSERT INTO watchlist_source_leases (
				watchlist_id, source, evidence_fingerprint, qualified_at,
				observed_at, effective_at, expires_at, evidence
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (watchlist_id, source) DO UPDATE SET
				evidence_fingerprint=EXCLUDED.evidence_fingerprint,
				qualified_at=EXCLUDED.qualified_at, observed_at=EXCLUDED.observed_at,
				effective_at=EXCLUDED.effective_at, expires_at=EXCLUDED.expires_at,
				evidence=EXCLUDED.evidence, updated_at=NOW()
			WHERE watchlist_source_leases.evidence_fingerprint
				IS DISTINCT FROM EXCLUDED.evidence_fingerprint`,
			watchlistID, strings.ToLower(strings.TrimSpace(lease.Source)),
			lease.EvidenceFingerprint, lease.QualifiedAt, lease.ObservedAt,
			lease.EffectiveAt, lease.ExpiresAt, lease.Evidence,
		)
		if err != nil {
			return false, fmt.Errorf("upsert watchlist source lease: %w", err)
		}
		changed = changed || tag.RowsAffected() > 0
	}
	if changed {
		discoveryContext, err := mergeRefreshedDiscoveryContext(ctx, db, watchlistID, item)
		if err != nil {
			return false, err
		}
		if _, err := db.Exec(ctx, `
			UPDATE watchlist w
			SET expires_at=(SELECT MAX(l.expires_at) FROM watchlist_source_leases l WHERE l.watchlist_id=w.id),
				discovery_context=COALESCE($2, discovery_context),
				last_rediscovered_at=NOW(), updated_at=NOW()
			WHERE w.id=$1`, watchlistID, discoveryContext); err != nil {
			return false, fmt.Errorf("project watchlist source lease expiry: %w", err)
		}
	}
	return changed, nil
}

// mergeRefreshedDiscoveryContext updates only the source evidence that was
// observed again. It preserves admission-time execution routing and older,
// unrelated provenance while making fresh evidence visible to the analyst.
func mergeRefreshedDiscoveryContext(
	ctx context.Context, db watchlistExecer, watchlistID string, item WatchlistItem,
) ([]byte, error) {
	if item.DiscoveryContext == nil {
		return nil, nil
	}
	fresh, err := item.DiscoveryContext.Canonicalize()
	if err != nil {
		return nil, fmt.Errorf("canonicalize refreshed discovery context: %w", err)
	}
	var raw []byte
	if err := db.QueryRow(ctx, `SELECT discovery_context FROM watchlist WHERE id=$1`, watchlistID).Scan(&raw); err != nil {
		return nil, err
	}
	existing, err := tradeprofile.DecodeDiscoveryContext(raw)
	if err != nil || existing == nil {
		return tradeprofile.MarshalDiscoveryContext(&fresh)
	}
	merged := *existing
	merged.Sources = append([]string(nil), existing.Sources...)
	merged.EvidenceBySource = make(map[string]json.RawMessage, len(existing.EvidenceBySource)+len(fresh.EvidenceBySource))
	for source, evidence := range existing.EvidenceBySource {
		merged.EvidenceBySource[source] = append(json.RawMessage(nil), evidence...)
	}
	for _, source := range fresh.Sources {
		if _, known := merged.EvidenceBySource[source]; !known {
			merged.Sources = append(merged.Sources, source)
		}
		merged.EvidenceBySource[source] = append(json.RawMessage(nil), fresh.EvidenceBySource[source]...)
	}
	if strings.TrimSpace(fresh.Thesis) != "" && fresh.Thesis != merged.Thesis {
		merged.Thesis = fresh.Thesis
	}
	return tradeprofile.MarshalDiscoveryContext(&merged)
}

// RemoveWatchlistItem soft-deletes a watchlist item by setting active = false.
func RemoveWatchlistItem(ctx context.Context, ticker, platform string) error {
	if platform == "" {
		platform = "stock"
	}
	_, err := GetDB().Exec(ctx, `
		UPDATE watchlist SET active = false, updated_at = NOW()
		WHERE ticker = $1 AND platform = $2
	`, ticker, platform)
	return err
}

// GetWatchlistItems returns watchlist entries. Pass activeOnly = true during engine
// execution; pass false to show all items in the CLI.
// Scanner-sourced items past their expires_at are excluded even when activeOnly = true.
func GetWatchlistItems(ctx context.Context, activeOnly bool) ([]WatchlistItem, error) {
	q := `
		SELECT id, ticker, platform, strategies, timeframes,
		       COALESCE(min_confidence, 0), source, COALESCE(graph_context, ''),
		       discovery_context, execution_profile, COALESCE(scanner_run_id::text, ''), active, expires_at,
		       pullback_requalification_required, created_at, updated_at
		FROM watchlist
	`
	if activeOnly {
		q += ` WHERE active AND (
			(source = 'earnings_watchlist' AND EXISTS (
				SELECT 1 FROM watchlist_discovery_contexts c WHERE c.watchlist_id=watchlist.id AND c.active=true AND (c.expires_at IS NULL OR c.expires_at > NOW())
			)) OR
			(source = 'scanner' AND (
				EXISTS (SELECT 1 FROM watchlist_source_leases l WHERE l.watchlist_id=watchlist.id AND l.expires_at > NOW()) OR
				(NOT EXISTS (SELECT 1 FROM watchlist_source_leases l WHERE l.watchlist_id=watchlist.id) AND (expires_at IS NULL OR expires_at > NOW())) OR
				EXISTS (SELECT 1 FROM watchlist_discovery_contexts c WHERE c.watchlist_id=watchlist.id AND c.active=true AND (c.expires_at IS NULL OR c.expires_at > NOW()))
			)) OR
			(source <> 'scanner' AND source <> 'earnings_watchlist' AND ((expires_at IS NULL OR expires_at > NOW()) OR EXISTS (
				SELECT 1 FROM watchlist_discovery_contexts c WHERE c.watchlist_id=watchlist.id AND c.active=true AND (c.expires_at IS NULL OR c.expires_at > NOW())
			)))
		)`
	}
	q += " ORDER BY created_at DESC"

	rows, err := GetDB().Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []WatchlistItem
	for rows.Next() {
		var w WatchlistItem
		var discoveryContext, executionProfile []byte
		if err := rows.Scan(
			&w.ID, &w.Ticker, &w.Platform, &w.Strategies, &w.Timeframes,
			&w.MinConfidence, &w.Source, &w.GraphContext, &discoveryContext, &executionProfile,
			&w.ScannerRunID,
			&w.Active, &w.ExpiresAt,
			&w.PullbackRequalificationRequired, &w.CreatedAt, &w.UpdatedAt,
		); err != nil {
			return nil, err
		}
		if err := decodeWatchlistContracts(&w, discoveryContext, executionProfile); err != nil {
			return nil, fmt.Errorf("decode watchlist %s/%s contracts: %w", w.Ticker, w.Platform, err)
		}
		items = append(items, w)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := loadActiveWatchlistDiscoveryContextsBatch(ctx, items); err != nil {
		return nil, fmt.Errorf("load watchlist discovery contexts: %w", err)
	}
	for i := range items {
		clearExpiredWatchlistContract(&items[i])
	}
	if activeOnly {
		items = filterExpiredContextlessScannerItems(items)
	}
	return items, nil
}

func filterExpiredContextlessScannerItems(items []WatchlistItem) []WatchlistItem {
	filtered := items[:0]
	for _, item := range items {
		if item.Source == "scanner" && item.ExpiresAt != nil && !item.ExpiresAt.After(time.Now()) && len(item.DiscoveryContexts) == 0 {
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}

// GetWatchlistItem returns a single watchlist entry by ticker and platform.
// Returns nil, nil if not found.
func GetWatchlistItem(ctx context.Context, ticker, platform string) (*WatchlistItem, error) {
	if platform == "" {
		platform = "stock"
	}
	row := GetDB().QueryRow(ctx, `
		SELECT id, ticker, platform, strategies, timeframes,
		       COALESCE(min_confidence, 0), source, COALESCE(graph_context, ''),
		       discovery_context, execution_profile, COALESCE(scanner_run_id::text, ''), active, expires_at,
		       pullback_requalification_required, created_at, updated_at
		FROM watchlist
		WHERE ticker = $1 AND platform = $2
	`, ticker, platform)

	var w WatchlistItem
	var discoveryContext, executionProfile []byte
	err := row.Scan(
		&w.ID, &w.Ticker, &w.Platform, &w.Strategies, &w.Timeframes,
		&w.MinConfidence, &w.Source, &w.GraphContext, &discoveryContext, &executionProfile, &w.ScannerRunID,
		&w.Active, &w.ExpiresAt,
		&w.PullbackRequalificationRequired, &w.CreatedAt, &w.UpdatedAt,
	)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	if err := decodeWatchlistContracts(&w, discoveryContext, executionProfile); err != nil {
		return nil, fmt.Errorf("decode watchlist %s/%s contracts: %w", w.Ticker, w.Platform, err)
	}
	if err := loadActiveWatchlistDiscoveryContexts(ctx, &w); err != nil {
		return nil, fmt.Errorf("load watchlist %s/%s discovery contexts: %w", w.Ticker, w.Platform, err)
	}
	clearExpiredWatchlistContract(&w)
	return &w, nil
}

// An independently active context may keep an expired scanner row visible.
// Its old routing contract must not survive past its owning legacy TTL.
func clearExpiredWatchlistContract(item *WatchlistItem) {
	if item.ExpiresAt != nil && !item.ExpiresAt.After(time.Now()) && len(item.DiscoveryContexts) > 0 {
		item.DiscoveryContext = nil
		item.ExecutionProfile = nil
		if item.Source == "scanner" {
			// A live additive context (such as scheduled earnings) can extend
			// eligibility, but never the scanner's old execution routing.
			item.Source = "earnings_watchlist"
			item.Strategies = []string{"trend_following", "momentum", "news_sentiment"}
			item.Timeframes = []string{"1D", "4H", "1H"}
			item.MinConfidence = 0
			item.ScannerRunID = ""
			item.GraphContext = "Scheduled earnings watchlist candidate."
		}
	}
}

func decodeWatchlistContracts(
	item *WatchlistItem, discoveryContext, executionProfile []byte,
) error {
	discovery, discoveryErr := tradeprofile.DecodeDiscoveryContext(discoveryContext)
	profile, profileErr := tradeprofile.DecodeExecutionProfile(executionProfile)
	if discoveryErr != nil || profileErr != nil || (discovery == nil) != (profile == nil) {
		// A contract can become invalid after a newer writer, a manual repair,
		// or corruption; database constraints deliberately validate only JSONB
		// shape and pairing. Do not let one unusable row abort an engine tick for
		// every other ticker. Clearing the in-memory pair returns this row to the
		// established legacy strategies/graph-context path until a scanner refresh
		// supplies a valid replacement contract.
		item.DiscoveryContext = nil
		item.ExecutionProfile = nil
		return nil
	}
	item.DiscoveryContext = discovery
	item.ExecutionProfile = profile
	return nil
}

// CountActiveScannerItems returns how many scanner-sourced items are currently
// active and unexpired on the watchlist. Used by the scanner to enforce the cap.
func CountActiveScannerItems(ctx context.Context) (int, error) {
	var count int
	err := GetDB().QueryRow(ctx, `
		SELECT COUNT(*) FROM watchlist
		WHERE source = 'scanner' AND active = true
		  AND (
			EXISTS (SELECT 1 FROM watchlist_source_leases l WHERE l.watchlist_id=watchlist.id AND l.expires_at > NOW())
			OR (NOT EXISTS (SELECT 1 FROM watchlist_source_leases l WHERE l.watchlist_id=watchlist.id)
				AND (expires_at IS NULL OR expires_at > NOW()))
		  )
	`).Scan(&count)
	return count, err
}

// ExpireScannedItems deactivates scanner-sourced watchlist items whose expires_at
// has passed. Returns the tickers that were expired.
func ExpireScannedItems(ctx context.Context) ([]string, error) {
	rows, err := GetDB().Query(ctx, `
		UPDATE watchlist
		SET active = false, updated_at = NOW()
		WHERE source = 'scanner' AND active = true
		  AND (
			(EXISTS (SELECT 1 FROM watchlist_source_leases l WHERE l.watchlist_id=watchlist.id)
			 AND NOT EXISTS (SELECT 1 FROM watchlist_source_leases l WHERE l.watchlist_id=watchlist.id AND l.expires_at > NOW()))
			OR
			(NOT EXISTS (SELECT 1 FROM watchlist_source_leases l WHERE l.watchlist_id=watchlist.id)
			 AND expires_at IS NOT NULL AND expires_at <= NOW())
		  )
		  AND NOT EXISTS (
			SELECT 1 FROM watchlist_discovery_contexts c
			WHERE c.watchlist_id=watchlist.id AND c.active=true AND (c.expires_at IS NULL OR c.expires_at > NOW())
		  )
		RETURNING ticker
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tickers []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		tickers = append(tickers, t)
	}
	return tickers, rows.Err()
}
