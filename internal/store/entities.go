package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ── Types ─────────────────────────────────────────────────────────────────────

// Entity mirrors the entities table row.
type Entity struct {
	ID          string
	EntityType  string // company | sector | person | country | commodity | event | prediction_contract | index
	Name        string
	Ticker      string
	Description string
	Sector      string
	Country     string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ── CRUD ──────────────────────────────────────────────────────────────────────

// UpsertEntity inserts or updates an entity.
// Conflict key: (name, entity_type). On conflict the description, sector, country,
// ticker, and updated_at are overwritten with the supplied values.
func UpsertEntity(ctx context.Context, e Entity) (string, error) {
	var id string
	// ON CONFLICT: use COALESCE so an auto-generated upsert (with sparse fields)
	// never overwrites rich seed/manual data with NULLs.  The new value wins only
	// when the existing column is already NULL.
	err := GetDB().QueryRow(ctx, `
		INSERT INTO entities (entity_type, name, ticker, description, sector, country)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (name, entity_type) DO UPDATE
		  SET ticker      = COALESCE(entities.ticker,      EXCLUDED.ticker),
		      description = COALESCE(entities.description, EXCLUDED.description),
		      sector      = COALESCE(entities.sector,      EXCLUDED.sector),
		      country     = COALESCE(entities.country,     EXCLUDED.country),
		      updated_at  = NOW()
		RETURNING id`,
		e.EntityType,
		e.Name,
		nullableString(e.Ticker),
		nullableString(e.Description),
		nullableString(e.Sector),
		nullableString(e.Country),
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("upsert entity %q: %w", e.Name, err)
	}
	return id, nil
}

// GetEntityByTicker returns the entity whose ticker column matches (case-insensitive).
func GetEntityByTicker(ctx context.Context, ticker string) (*Entity, error) {
	row := GetDB().QueryRow(ctx, `
		SELECT id, entity_type, name, COALESCE(ticker,''), COALESCE(description,''),
		       COALESCE(sector,''), COALESCE(country,''), created_at, updated_at
		FROM entities WHERE UPPER(ticker) = UPPER($1)`, ticker)
	e, err := scanEntity(row)
	if err != nil {
		if IsNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get entity by ticker %q: %w", ticker, err)
	}
	return e, nil
}

// GetEntityByID returns the entity with the given UUID.
func GetEntityByID(ctx context.Context, id string) (*Entity, error) {
	row := GetDB().QueryRow(ctx, `
		SELECT id, entity_type, name, COALESCE(ticker,''), COALESCE(description,''),
		       COALESCE(sector,''), COALESCE(country,''), created_at, updated_at
		FROM entities WHERE id = $1`, id)
	e, err := scanEntity(row)
	if err != nil {
		if IsNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get entity by id %q: %w", id, err)
	}
	return e, nil
}

// ListEntities returns all entities, optionally filtered by entity_type.
// Pass an empty string to list all types.
func ListEntities(ctx context.Context, entityType string) ([]Entity, error) {
	var rows pgx.Rows
	var err error
	if entityType == "" {
		rows, err = GetDB().Query(ctx, `
			SELECT id, entity_type, name, COALESCE(ticker,''), COALESCE(description,''),
			       COALESCE(sector,''), COALESCE(country,''), created_at, updated_at
			FROM entities ORDER BY entity_type, name`)
	} else {
		rows, err = GetDB().Query(ctx, `
			SELECT id, entity_type, name, COALESCE(ticker,''), COALESCE(description,''),
			       COALESCE(sector,''), COALESCE(country,''), created_at, updated_at
			FROM entities WHERE entity_type = $1 ORDER BY name`, entityType)
	}
	if err != nil {
		return nil, fmt.Errorf("list entities: %w", err)
	}
	defer rows.Close()
	return collectEntities(rows)
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func scanEntity(row pgx.Row) (*Entity, error) {
	var e Entity
	if err := row.Scan(
		&e.ID, &e.EntityType, &e.Name, &e.Ticker, &e.Description,
		&e.Sector, &e.Country, &e.CreatedAt, &e.UpdatedAt,
	); err != nil {
		return nil, err
	}
	return &e, nil
}

func collectEntities(rows pgx.Rows) ([]Entity, error) {
	var out []Entity
	for rows.Next() {
		var e Entity
		if err := rows.Scan(
			&e.ID, &e.EntityType, &e.Name, &e.Ticker, &e.Description,
			&e.Sector, &e.Country, &e.CreatedAt, &e.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan entity: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
