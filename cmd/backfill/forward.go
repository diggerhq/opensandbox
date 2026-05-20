package main

// Daemon mode: keep D1 continuously in sync with cell PG for the cutover
// parallel-run window. Two mechanisms, by design:
//   - INSERT/UPDATE  → watermarked delta over the global tables (the validated
//     backfill path). Reliable because migration 041 put updated_at triggers on
//     every global table, so every write bumps the watermark.
//   - DELETE         → drained from global_sync_outbox (migration 042). The
//     watermark can't see deletes (the row is gone), so the DB-trigger outbox
//     captures them for all write paths.
//
// State (watermark + outbox delete cursor) is persisted to --state-file so the
// daemon resumes cleanly after a restart.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// The 9 global control-plane tables the daemon owns. sandboxes_index /
// checkpoints_index are intentionally excluded — the worker event stream
// (events-ingest) owns those.
var globalSyncTables = map[string]bool{
	"orgs": true, "users": true, "org_memberships": true, "api_keys": true,
	"templates": true, "secret_stores": true, "secret_store_entries": true,
	"agent_subscriptions": true, "org_subscription_items": true,
}

type syncState struct {
	Watermark float64 `json:"watermark"`  // max updated_at (unix s, fractional) synced so far
	DeleteSeq int64   `json:"delete_seq"` // max global_sync_outbox.seq drained
}

func loadState(path string) syncState {
	var s syncState
	b, err := os.ReadFile(path)
	if err != nil {
		return s // absent → zero; caller bootstraps from --since
	}
	if err := json.Unmarshal(b, &s); err != nil {
		log.Printf("warning: bad state file %s (%v) — starting fresh", path, err)
		return syncState{}
	}
	return s
}

func saveState(path string, s syncState) {
	b, _ := json.Marshal(s)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		log.Printf("warning: persist state to %s: %v", tmp, err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Printf("warning: rename state file: %v", err)
	}
}

func runDaemon(ctx context.Context, conn *pgx.Conn, d1 *d1Client, specs []tableSpec, initialSince float64, interval time.Duration, stateFile string, batchParams int) {
	st := loadState(stateFile)
	if st.Watermark == 0 && initialSince > 0 {
		st.Watermark = initialSince
	}
	log.Printf("sync daemon: watermark=%.3f deleteSeq=%d interval=%s state=%s", st.Watermark, st.DeleteSeq, interval, stateFile)

	for {
		// 1) upserts via watermarked delta over the global tables
		var upserts int
		maxWM := st.Watermark
		for _, spec := range specs {
			if !globalSyncTables[spec.d1Table] {
				continue
			}
			n, _, wm, err := syncTable(ctx, conn, d1, spec, false, st.Watermark, batchParams)
			if err != nil {
				log.Printf("daemon upsert %s: %v", spec.d1Table, err)
				continue
			}
			upserts += n
			if wm > maxWM {
				maxWM = wm
			}
		}

		// 2) deletes drained from the outbox
		deletes, maxSeq, err := drainDeletes(ctx, conn, d1, st.DeleteSeq)
		if err != nil {
			log.Printf("daemon drain deletes: %v", err)
		}

		if maxWM > st.Watermark || maxSeq > st.DeleteSeq {
			st.Watermark = maxWM
			st.DeleteSeq = maxSeq
			saveState(stateFile, st)
		}

		// Prune drained outbox rows so the table doesn't grow unbounded over a
		// multi-day parallel run. Safe: rows <= DeleteSeq are applied to D1.
		if pruned, err := pruneOutbox(ctx, conn, st.DeleteSeq); err != nil {
			log.Printf("daemon prune outbox: %v", err)
		} else if pruned > 0 {
			log.Printf("pruned %d drained outbox rows (<= seq %d)", pruned, st.DeleteSeq)
		}

		if upserts > 0 || deletes > 0 {
			log.Printf("tick: upserts=%d deletes=%d watermark=%.3f deleteSeq=%d", upserts, deletes, st.Watermark, st.DeleteSeq)
		}

		select {
		case <-ctx.Done():
			log.Printf("sync daemon stopping (watermark=%.3f deleteSeq=%d)", st.Watermark, st.DeleteSeq)
			return
		case <-time.After(interval):
		}
	}
}

// pruneOutbox deletes already-drained outbox rows (seq <= upToSeq) so the
// table stays small during a long parallel run.
func pruneOutbox(ctx context.Context, conn *pgx.Conn, upToSeq int64) (int64, error) {
	if upToSeq <= 0 {
		return 0, nil
	}
	tag, err := conn.Exec(ctx, `DELETE FROM global_sync_outbox WHERE seq <= $1`, upToSeq)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// drainDeletes applies all DELETE outbox entries with seq > fromSeq to D1, in
// order, returning the count applied and the new max seq.
func drainDeletes(ctx context.Context, conn *pgx.Conn, d1 *d1Client, fromSeq int64) (int, int64, error) {
	rows, err := conn.Query(ctx,
		`SELECT seq, table_name, row_id FROM global_sync_outbox
		  WHERE op = 'DELETE' AND seq > $1 ORDER BY seq`, fromSeq)
	if err != nil {
		return 0, fromSeq, fmt.Errorf("query outbox: %w", err)
	}
	defer rows.Close()

	type del struct {
		seq          int64
		table, rowID string
	}
	var dels []del
	for rows.Next() {
		var d del
		if err := rows.Scan(&d.seq, &d.table, &d.rowID); err != nil {
			return 0, fromSeq, err
		}
		dels = append(dels, d)
	}
	if err := rows.Err(); err != nil {
		return 0, fromSeq, err
	}

	n := 0
	maxSeq := fromSeq
	for _, d := range dels {
		if err := applyDelete(ctx, d1, d.table, d.rowID); err != nil {
			return n, maxSeq, fmt.Errorf("delete %s/%s (seq %d): %w", d.table, d.rowID, d.seq, err)
		}
		n++
		if d.seq > maxSeq {
			maxSeq = d.seq
		}
	}
	return n, maxSeq, nil
}

// applyDelete removes a row from D1, mirroring the backfill's key mapping:
// users also cascades to the derived org_memberships; org_subscription_items
// uses the composite "org_id/memory_mb" key.
func applyDelete(ctx context.Context, d1 *d1Client, table, rowID string) error {
	if !globalSyncTables[table] {
		return fmt.Errorf("refusing delete on non-global table %q", table)
	}
	if d1.dryRun {
		return nil
	}
	switch table {
	case "users":
		if err := d1.query(ctx, "DELETE FROM users WHERE id = ?", []any{rowID}); err != nil {
			return err
		}
		return d1.query(ctx, "DELETE FROM org_memberships WHERE user_id = ?", []any{rowID})
	case "org_subscription_items":
		parts := strings.SplitN(rowID, "/", 2)
		if len(parts) != 2 {
			return fmt.Errorf("bad org_subscription_items key %q", rowID)
		}
		return d1.query(ctx, "DELETE FROM org_subscription_items WHERE org_id = ? AND tier = ?", []any{parts[0], parts[1]})
	default:
		return d1.query(ctx, "DELETE FROM "+table+" WHERE id = ?", []any{rowID})
	}
}
