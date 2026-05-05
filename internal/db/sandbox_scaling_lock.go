package db

import (
	"context"
	"fmt"
)

// SetScalingLock toggles the per-sandbox scaling lock. When true, the sandbox
// is exempt from both manual /scale calls and the per-sandbox autoscaler.
// The handler that calls this should also clear autoscale_enabled when
// locking — done at the API layer, not here, so this function is a pure
// column setter.
func (s *Store) SetScalingLock(ctx context.Context, sandboxID string, locked bool) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE sandbox_sessions
		SET scaling_locked = $1
		WHERE sandbox_id = $2
	`, locked, sandboxID)
	if err != nil {
		return fmt.Errorf("set scaling lock on %s: %w", sandboxID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("sandbox %s not found", sandboxID)
	}
	return nil
}

// GetScalingLock returns the current lock state for a sandbox. Returns false
// (and no error) if the sandbox doesn't exist — the caller is expected to
// have already validated the sandbox via GetSandboxSession.
func (s *Store) GetScalingLock(ctx context.Context, sandboxID string) (bool, error) {
	var locked bool
	err := s.pool.QueryRow(ctx, `
		SELECT scaling_locked FROM sandbox_sessions WHERE sandbox_id = $1
	`, sandboxID).Scan(&locked)
	if err != nil {
		return false, err
	}
	return locked, nil
}
