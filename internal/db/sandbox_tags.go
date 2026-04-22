package db

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// SandboxTagSet is the materialized tag state for one sandbox.
type SandboxTagSet struct {
	Tags          map[string]string
	LastUpdatedAt *time.Time // nil when the sandbox has no tags
}

// TagKeyStats summarizes one tag key across an org — used by GET /tags.
type TagKeyStats struct {
	Key          string
	SandboxCount int
	ValueCount   int
}

// GetSandboxTags returns the tag map plus the latest updated_at across
// rows for one sandbox. Returns an empty map and nil timestamp when the
// sandbox has no tags.
func (s *Store) GetSandboxTags(ctx context.Context, sandboxID string) (SandboxTagSet, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT key, value, updated_at FROM sandbox_tags WHERE sandbox_id = $1`,
		sandboxID)
	if err != nil {
		return SandboxTagSet{}, fmt.Errorf("query sandbox tags: %w", err)
	}
	defer rows.Close()

	out := SandboxTagSet{Tags: map[string]string{}}
	for rows.Next() {
		var k, v string
		var u time.Time
		if err := rows.Scan(&k, &v, &u); err != nil {
			return SandboxTagSet{}, err
		}
		out.Tags[k] = v
		if out.LastUpdatedAt == nil || u.After(*out.LastUpdatedAt) {
			t := u
			out.LastUpdatedAt = &t
		}
	}
	return out, rows.Err()
}

// GetSandboxTagsMulti returns tag sets keyed by sandbox_id for a batch of
// sandboxes — used to hydrate GET /sandboxes and /sandboxes/{id} without
// N+1 queries. Sandboxes with no tags are absent from the result map.
func (s *Store) GetSandboxTagsMulti(ctx context.Context, sandboxIDs []string) (map[string]SandboxTagSet, error) {
	if len(sandboxIDs) == 0 {
		return map[string]SandboxTagSet{}, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT sandbox_id, key, value, updated_at FROM sandbox_tags
		 WHERE sandbox_id = ANY($1)`, sandboxIDs)
	if err != nil {
		return nil, fmt.Errorf("query multi sandbox tags: %w", err)
	}
	defer rows.Close()

	out := map[string]SandboxTagSet{}
	for rows.Next() {
		var sid, k, v string
		var u time.Time
		if err := rows.Scan(&sid, &k, &v, &u); err != nil {
			return nil, err
		}
		set, ok := out[sid]
		if !ok {
			set = SandboxTagSet{Tags: map[string]string{}}
		}
		set.Tags[k] = v
		if set.LastUpdatedAt == nil || u.After(*set.LastUpdatedAt) {
			t := u
			set.LastUpdatedAt = &t
		}
		out[sid] = set
	}
	return out, rows.Err()
}

// ReplaceSandboxTags atomically replaces the full tag set for a sandbox.
// An empty map clears all tags. updated_at on retained rows is refreshed
// only if the value changed — idempotent PUTs don't bump the timestamp.
func (s *Store) ReplaceSandboxTags(ctx context.Context, orgID uuid.UUID, sandboxID string, tags map[string]string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// Diff against current state so we only bump updated_at when something
	// actually changed. Retagging is supposed to rewrite attribution going
	// forward (see design), and tagsLastUpdatedAt is the signal dashboards
	// rely on — so we must not bump it on no-op PUTs.
	curRows, err := tx.Query(ctx,
		`SELECT key, value FROM sandbox_tags WHERE sandbox_id = $1`, sandboxID)
	if err != nil {
		return err
	}
	current := map[string]string{}
	for curRows.Next() {
		var k, v string
		if err := curRows.Scan(&k, &v); err != nil {
			curRows.Close()
			return err
		}
		current[k] = v
	}
	curRows.Close()
	if err := curRows.Err(); err != nil {
		return err
	}

	// Delete keys that are no longer present.
	for k := range current {
		if _, keep := tags[k]; !keep {
			if _, err := tx.Exec(ctx,
				`DELETE FROM sandbox_tags WHERE sandbox_id = $1 AND key = $2`,
				sandboxID, k); err != nil {
				return err
			}
		}
	}
	// Upsert changed or new keys.
	for k, v := range tags {
		if cur, ok := current[k]; ok && cur == v {
			continue
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO sandbox_tags (org_id, sandbox_id, key, value, updated_at)
			 VALUES ($1, $2, $3, $4, now())
			 ON CONFLICT (sandbox_id, key) DO UPDATE
			   SET value = EXCLUDED.value, updated_at = now(), org_id = EXCLUDED.org_id`,
			orgID, sandboxID, k, v); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// ListOrgTagKeys returns per-key aggregates for GET /tags.
func (s *Store) ListOrgTagKeys(ctx context.Context, orgID uuid.UUID) ([]TagKeyStats, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT key,
		        COUNT(DISTINCT sandbox_id),
		        COUNT(DISTINCT value)
		 FROM sandbox_tags
		 WHERE org_id = $1
		 GROUP BY key
		 ORDER BY key`, orgID)
	if err != nil {
		return nil, fmt.Errorf("list org tag keys: %w", err)
	}
	defer rows.Close()
	var out []TagKeyStats
	for rows.Next() {
		var k TagKeyStats
		if err := rows.Scan(&k.Key, &k.SandboxCount, &k.ValueCount); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}
