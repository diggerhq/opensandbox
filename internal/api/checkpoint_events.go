package api

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/opensandbox/opensandbox/internal/db"
)

// publishCheckpointEvent XADDs a checkpoint lifecycle event to the cell's
// local events:{cell_id} Redis stream so the forwarder + events-ingest
// Worker can keep D1's checkpoints_index in sync. Same envelope shape as
// the sandbox lifecycle events emitted by the worker per-sandbox SQLite +
// the cell_capacity events emitted by controlplane.CapacityReporter.
//
// Event types:
//   checkpoint_ready    — checkpoint upload finished; UPSERT row in D1
//   checkpoint_deleted  — checkpoint dropped from cell PG; DELETE row in D1
//
// Best-effort: failure to XADD is logged but doesn't fail the caller. The
// dashboard cross-cell view runs ~seconds behind cell PG truth as a result;
// per-sandbox listings hit the cell directly via /api/sandboxes/{id}/
// checkpoints so they're always authoritative.
func (s *Server) publishCheckpointEvent(
	ctx context.Context,
	eventType string,
	checkpointID uuid.UUID,
	sandboxID string,
	orgID uuid.UUID,
	workerID string,
	payload map[string]any,
) {
	if s.redisClient == nil || s.cellID == "" {
		return
	}
	envelope := map[string]any{
		"id":         uuid.NewString(),
		"type":       eventType,
		"sandbox_id": sandboxID,
		"org_id":     orgID.String(),
		"worker_id":  workerID,
		"cell_id":    s.cellID,
		"payload":    map[string]any{},
		"timestamp":  time.Now().UTC().Format(time.RFC3339Nano),
	}
	// Merge in the checkpoint-specific payload + always include the
	// checkpoint ID so events-ingest can key on it.
	out := envelope["payload"].(map[string]any)
	out["checkpoint_id"] = checkpointID.String()
	for k, v := range payload {
		out[k] = v
	}

	body, err := json.Marshal(envelope)
	if err != nil {
		log.Printf("publishCheckpointEvent: marshal: %v", err)
		return
	}

	streamKey := "events:" + s.cellID
	xaddCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := s.redisClient.XAdd(xaddCtx, &redis.XAddArgs{
		Stream: streamKey,
		MaxLen: 100000,
		Approx: true,
		Values: map[string]any{"event": string(body)},
	}).Err(); err != nil {
		log.Printf("publishCheckpointEvent: XADD %s: %v", eventType, err)
	}
}

// publishSandboxLifecycleEvent emits a sandbox lifecycle event ("stopped",
// "hibernated", etc) from the CP when the canonical emit path (worker's
// per-sandbox SQLite via sdb.LogEvent) didn't run — typically because the
// gRPC call to the worker failed and the worker never reached the LogEvent
// line. Without this fallback, cell PG can flip to status='stopped' while
// D1 sandboxes_index stays at 'running' indefinitely.
func (s *Server) publishSandboxLifecycleEvent(
	ctx context.Context,
	eventType string,
	sandboxID string,
	orgID uuid.UUID,
	workerID string,
	reason string,
) {
	if s.redisClient == nil || s.cellID == "" || sandboxID == "" {
		return
	}
	envelope := map[string]any{
		"id":         uuid.NewString(),
		"type":       eventType,
		"sandbox_id": sandboxID,
		"org_id":     orgID.String(),
		"worker_id":  workerID,
		"cell_id":    s.cellID,
		"payload":    map[string]any{"reason": reason},
		"timestamp":  time.Now().UTC().Format(time.RFC3339Nano),
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		log.Printf("publishSandboxLifecycleEvent: marshal: %v", err)
		return
	}
	streamKey := "events:" + s.cellID
	xaddCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := s.redisClient.XAdd(xaddCtx, &redis.XAddArgs{
		Stream: streamKey,
		MaxLen: 100000,
		Approx: true,
		Values: map[string]any{"event": string(body)},
	}).Err(); err != nil {
		log.Printf("publishSandboxLifecycleEvent: XADD %s: %v", eventType, err)
	}
}

// publishImageCacheReadyFrom packs the relevant fields from a db.ImageCache
// row and emits an image_cache_ready event. Sugar over publishImageCacheEvent
// so callers can just pass the row they already have in hand.
func (s *Server) publishImageCacheReadyFrom(ctx context.Context, ic *db.ImageCache) {
	if ic == nil {
		return
	}
	payload := map[string]any{
		"content_hash": ic.ContentHash,
		"manifest":     string(ic.Manifest),
		"status":       ic.Status,
		"created_at":   ic.CreatedAt.Unix(),
		"last_used_at": ic.LastUsedAt.Unix(),
	}
	if ic.CheckpointID != nil {
		payload["checkpoint_id"] = ic.CheckpointID.String()
	}
	if ic.Name != nil {
		payload["name"] = *ic.Name
	}
	s.publishImageCacheEvent(ctx, "image_cache_ready", ic.ID, ic.OrgID, payload)
}

// publishImageCacheEvent mirrors publishCheckpointEvent for the image_cache
// table. The same event-→D1 sync pattern keeps the dashboard's cross-cell
// Images view in sync without proxying per-cell at request time.
//
// Event types:
//   image_cache_ready    — image is in ready state; UPSERT into D1 images_index
//   image_cache_deleted  — image dropped from cell PG; DELETE from D1
func (s *Server) publishImageCacheEvent(
	ctx context.Context,
	eventType string,
	imageID uuid.UUID,
	orgID uuid.UUID,
	payload map[string]any,
) {
	if s.redisClient == nil || s.cellID == "" {
		return
	}
	envelope := map[string]any{
		"id":         uuid.NewString(),
		"type":       eventType,
		"sandbox_id": "", // not a sandbox event
		"org_id":     orgID.String(),
		"worker_id":  "",
		"cell_id":    s.cellID,
		"payload":    map[string]any{},
		"timestamp":  time.Now().UTC().Format(time.RFC3339Nano),
	}
	out := envelope["payload"].(map[string]any)
	out["image_id"] = imageID.String()
	for k, v := range payload {
		out[k] = v
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		log.Printf("publishImageCacheEvent: marshal: %v", err)
		return
	}
	streamKey := "events:" + s.cellID
	xaddCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := s.redisClient.XAdd(xaddCtx, &redis.XAddArgs{
		Stream: streamKey,
		MaxLen: 100000,
		Approx: true,
		Values: map[string]any{"event": string(body)},
	}).Err(); err != nil {
		log.Printf("publishImageCacheEvent: XADD %s: %v", eventType, err)
	}
}
