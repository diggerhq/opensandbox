package db

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// SecretRefreshTarget is one running sandbox affected by a secret-store update.
// MigratingToWorker is empty when the sandbox is steady-state on WorkerID;
// non-empty during a migration in flight, in which case the refresh fanout
// must push to BOTH workers (source and destination) to avoid a window where
// one of them serves outbound HTTPS with the stale value after the cutover.
type SecretRefreshTarget struct {
	SandboxID         string
	WorkerID          string
	MigratingToWorker string
}

// ListRunningSandboxesByStore returns the running sandboxes that resolve their
// envs from the given SecretStore. Used by the secret-store-update flow to
// fanout UpdateSandboxSecret RPCs to the right workers.
//
// Filters: status='running' (no-op for stopped/hibernated; they'll get fresh
// values when the customer next wakes/recreates them).
func (s *Store) ListRunningSandboxesByStore(ctx context.Context, storeID uuid.UUID) ([]SecretRefreshTarget, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT sandbox_id, worker_id, COALESCE(migrating_to_worker, '')
		FROM sandbox_sessions
		WHERE secret_store_id = $1 AND status = 'running'
	`, storeID)
	if err != nil {
		return nil, fmt.Errorf("list sandboxes by store %s: %w", storeID, err)
	}
	defer rows.Close()

	var out []SecretRefreshTarget
	for rows.Next() {
		var t SecretRefreshTarget
		if err := rows.Scan(&t.SandboxID, &t.WorkerID, &t.MigratingToWorker); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
