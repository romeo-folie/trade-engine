package store

import "context"

// EngineState keys. Runtime state the engine publishes for cross-process readers
// (the in-process web server and the out-of-process TUI).
const (
	// EngineStateNextCycleAt holds the next scheduled engine run-cycle time as an
	// RFC3339 string (UTC). Written by the engine loop after each schedule.
	EngineStateNextCycleAt = "next_cycle_at"

	// EngineStateBrokerDrift holds the latest broker-vs-DB reconciliation as a
	// JSON blob, written periodically by the engine so readers (TUI, web) can
	// surface drift even when they can't reconcile live themselves.
	EngineStateBrokerDrift = "broker_drift"
)

// SetEngineState upserts a runtime engine-state value by key.
func SetEngineState(ctx context.Context, key, value string) error {
	_, err := GetDB().Exec(ctx, `
		INSERT INTO engine_state (key, value, updated_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (key) DO UPDATE
		SET value = EXCLUDED.value, updated_at = NOW()
	`, key, value)
	return err
}

// GetEngineState reads a runtime engine-state value by key.
// Returns ("", false, nil) when the DB is disconnected or the key is absent.
func GetEngineState(ctx context.Context, key string) (string, bool, error) {
	if !IsConnected() {
		return "", false, nil
	}
	var value string
	err := GetDB().QueryRow(ctx,
		`SELECT value FROM engine_state WHERE key = $1`, key,
	).Scan(&value)
	if err != nil {
		if isNoRows(err) {
			return "", false, nil
		}
		return "", false, err
	}
	return value, true, nil
}
