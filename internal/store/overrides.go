package store

import (
	"context"
	"time"
)

// ConfigOverride represents a runtime config override stored in the database.
type ConfigOverride struct {
	Key       string
	Value     string
	UpdatedAt time.Time
}

// GetOverride retrieves a single config override by key.
// Returns ("", false, nil) if the key is not found.
func GetOverride(ctx context.Context, key string) (string, bool, error) {
	var value string
	err := GetDB().QueryRow(ctx,
		`SELECT value FROM config_overrides WHERE key = $1`, key,
	).Scan(&value)
	if err != nil {
		if isNoRows(err) {
			return "", false, nil
		}
		return "", false, err
	}
	return value, true, nil
}

// SetOverride inserts or updates a config override.
func SetOverride(ctx context.Context, key, value string) error {
	_, err := GetDB().Exec(ctx, `
		INSERT INTO config_overrides (key, value, updated_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (key) DO UPDATE
		SET value = EXCLUDED.value, updated_at = NOW()
	`, key, value)
	return err
}

// ListOverrides returns all stored config overrides.
func ListOverrides(ctx context.Context) ([]ConfigOverride, error) {
	rows, err := GetDB().Query(ctx,
		`SELECT key, value, updated_at FROM config_overrides ORDER BY key`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var overrides []ConfigOverride
	for rows.Next() {
		var o ConfigOverride
		if err := rows.Scan(&o.Key, &o.Value, &o.UpdatedAt); err != nil {
			return nil, err
		}
		overrides = append(overrides, o)
	}
	return overrides, rows.Err()
}

// DeleteOverride removes a config override by key.
func DeleteOverride(ctx context.Context, key string) error {
	_, err := GetDB().Exec(ctx, `DELETE FROM config_overrides WHERE key = $1`, key)
	return err
}

// isNoRows checks if the error is pgx's no-rows sentinel (unexported convenience).
func isNoRows(err error) bool { return IsNoRows(err) }

// IsNoRows reports whether err is the pgx "no rows in result set" sentinel.
// Exported for use by packages that query the store directly.
func IsNoRows(err error) bool {
	return err != nil && err.Error() == "no rows in result set"
}
