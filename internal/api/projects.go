package api

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/opensandbox/opensandbox/internal/auth"
	"github.com/opensandbox/opensandbox/internal/db"
	pb "github.com/opensandbox/opensandbox/proto/worker"
)

// ── Secret Stores ─────────────────────────────────────────────────────────────

func (s *Server) createSecretStore(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}
	var req struct {
		Name            string   `json:"name"`
		EgressAllowlist []string `json:"egressAllowlist"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}
	if req.Name == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "name is required"})
	}

	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "auth required"})
	}

	store, err := s.store.CreateSecretStore(c.Request().Context(), orgID, req.Name, req.EgressAllowlist)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusCreated, store)
}

func (s *Server) listSecretStores(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}
	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "auth required"})
	}

	stores, err := s.store.ListSecretStores(c.Request().Context(), orgID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, stores)
}

func (s *Server) getSecretStore(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}
	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "auth required"})
	}

	storeID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid store ID"})
	}

	store, err := s.store.GetSecretStore(c.Request().Context(), orgID, storeID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "secret store not found"})
	}
	return c.JSON(http.StatusOK, store)
}

func (s *Server) updateSecretStore(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}
	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "auth required"})
	}

	storeID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid store ID"})
	}

	var req struct {
		Name            *string  `json:"name"`
		EgressAllowlist []string `json:"egressAllowlist"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}

	existing, err := s.store.GetSecretStore(c.Request().Context(), orgID, storeID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "secret store not found"})
	}

	name := existing.Name
	allowlist := existing.EgressAllowlist

	if req.Name != nil {
		name = *req.Name
	}
	if req.EgressAllowlist != nil {
		allowlist = req.EgressAllowlist
	}

	store, err := s.store.UpdateSecretStore(c.Request().Context(), orgID, storeID, name, allowlist)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, store)
}

func (s *Server) deleteSecretStore(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}
	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "auth required"})
	}

	storeID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid store ID"})
	}

	if err := s.store.DeleteSecretStore(c.Request().Context(), orgID, storeID); err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
	}
	return c.NoContent(http.StatusNoContent)
}

// ── Secret Store Entries ──────────────────────────────────────────────────────

func (s *Server) setSecretEntry(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}
	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "auth required"})
	}

	storeID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid store ID"})
	}
	name := c.Param("name")
	if name == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "secret name required"})
	}

	if _, err := s.store.GetSecretStore(c.Request().Context(), orgID, storeID); err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "secret store not found"})
	}

	var req struct {
		Value        string   `json:"value"`
		AllowedHosts []string `json:"allowedHosts"`
	}
	if err := c.Bind(&req); err != nil || req.Value == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "secret value required: {\"value\": \"...\", \"allowedHosts\": [...]}"})
	}

	for _, h := range req.AllowedHosts {
		h = strings.TrimSpace(h)
		if h == "" {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": "allowedHosts entries cannot be empty"})
		}
	}

	if err := s.store.SetSecretEntry(c.Request().Context(), storeID, name, []byte(req.Value), req.AllowedHosts); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	// Fan the new value out to running sandboxes that resolve envs from this
	// store. The proxy on each affected worker updates the value its sealed
	// token resolves to — the sandbox env keeps the same `osb_sealed_xxx`
	// string, so no restart, no env re-injection, and no agent involvement
	// inside the guest. Synchronous: PUT returns only after every reachable
	// affected worker has confirmed (or hit the timeout). Per-sandbox failures
	// are logged + included in the response but don't fail the whole call —
	// the DB write is the source of truth and fresh values will propagate
	// on the next sandbox handoff (wake, fork, migrate).
	refreshed, refreshErrs := s.fanoutSecretRefresh(c.Request().Context(), storeID, name, req.Value)

	resp := map[string]any{
		"name":      name,
		"status":    "set",
		"refreshed": refreshed,
	}
	if len(refreshErrs) > 0 {
		resp["refreshErrors"] = refreshErrs
	}
	return c.JSON(http.StatusOK, resp)
}

func (s *Server) deleteSecretEntry(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}
	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "auth required"})
	}

	storeID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid store ID"})
	}
	name := c.Param("name")

	if _, err := s.store.GetSecretStore(c.Request().Context(), orgID, storeID); err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "secret store not found"})
	}

	if err := s.store.DeleteSecretEntry(c.Request().Context(), storeID, name); err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Server) listSecretEntries(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}
	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "auth required"})
	}

	storeID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid store ID"})
	}

	if _, err := s.store.GetSecretStore(c.Request().Context(), orgID, storeID); err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "secret store not found"})
	}

	entries, err := s.store.ListSecretEntries(c.Request().Context(), storeID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, entries)
}

// secretRefreshTimeout is the per-call deadline for the worker fan-out. Picked
// to comfortably exceed the worst-case gRPC RTT to a healthy worker (~1s) plus
// migration windows where source/destination both need a push. PUT /secrets
// returns within this bound regardless of how many sandboxes are affected.
const secretRefreshTimeout = 15 * time.Second

// fanoutSecretRefresh tells every worker hosting a sandbox that uses this
// SecretStore to update its proxy session's value for `name` to `newValue`.
// Synchronous: caller blocks until all in-flight RPCs complete or hit the
// timeout. Returns the count of sandboxes successfully refreshed and a slice
// of human-readable per-sandbox failures.
//
// Migration awareness: if a sandbox is mid-migration (`migrating_to_worker`
// non-empty), the update is pushed to BOTH source and destination workers in
// parallel. The "winner" depends on which worker's gRPC call lands last; both
// are correct because the proxy session on the destination is re-registered
// during PrepareIncomingMigration, which means by the time we're pushing
// updates the destination has a session to update. Idempotent on either side
// — pushing to a worker that doesn't have a session for this sandbox returns
// updated=false (transient miss, logged but not fatal).
func (s *Server) fanoutSecretRefresh(parentCtx context.Context, storeID uuid.UUID, name, newValue string) (refreshed int, failures []string) {
	if s.store == nil || s.workerRegistry == nil {
		return 0, nil
	}

	// Use a separate context with our own timeout — the parent is the HTTP
	// request, which we don't want to bleed onto. Caller ultimately waits up
	// to secretRefreshTimeout regardless of the request's deadline.
	ctx, cancel := context.WithTimeout(context.Background(), secretRefreshTimeout)
	defer cancel()
	_ = parentCtx // not propagated; we want bounded fanout time

	targets, err := s.store.ListRunningSandboxesByStore(ctx, storeID)
	if err != nil {
		log.Printf("secret-refresh: list sandboxes for store %s failed: %v", storeID, err)
		failures = append(failures, fmt.Sprintf("list affected sandboxes: %v", err))
		return 0, failures
	}
	if len(targets) == 0 {
		return 0, nil
	}

	type result struct {
		sandboxID string
		ok        bool
		err       error
	}

	// Dedup: a sandbox mid-migration appears once but spawns 2 RPC calls
	// (source + destination). pushOne handles a single (sandboxID, workerID)
	// pair so we can reuse for both halves of the dual-push.
	pushOne := func(sandboxID, workerID string) result {
		client, err := s.workerRegistry.GetWorkerClient(workerID)
		if err != nil {
			return result{sandboxID: sandboxID, err: fmt.Errorf("worker %s unreachable: %w", workerID, err)}
		}
		callCtx, callCancel := context.WithTimeout(ctx, 5*time.Second)
		defer callCancel()
		resp, err := client.UpdateSandboxSecret(callCtx, &pb.UpdateSandboxSecretRequest{
			SandboxId:  sandboxID,
			SecretName: name,
			Value:      newValue,
		})
		if err != nil {
			// Rollout safety: a worker that hasn't picked up the new binary yet
			// returns Unimplemented for this RPC. Treat as a soft skip — the
			// sandbox keeps its old value until its next handoff (wake/fork/
			// migrate) re-resolves from the store. This lets the server be
			// deployed before the worker fleet has fully rolled over without
			// every PUT /secrets call surfacing failures during the dance.
			if status.Code(err) == codes.Unimplemented {
				log.Printf("secret-refresh: worker %s on old binary (Unimplemented); skipping sandbox=%s, will refresh on next handoff", workerID, sandboxID)
				return result{sandboxID: sandboxID, ok: false}
			}
			return result{sandboxID: sandboxID, err: fmt.Errorf("worker %s rpc: %w", workerID, err)}
		}
		return result{sandboxID: sandboxID, ok: resp.Updated}
	}

	var wg sync.WaitGroup
	resultsCh := make(chan result, len(targets)*2)
	for _, t := range targets {
		wg.Add(1)
		go func(t db.SecretRefreshTarget) {
			defer wg.Done()
			// Always push to the source worker.
			resultsCh <- pushOne(t.SandboxID, t.WorkerID)
			// During an in-flight migration, also push to the destination
			// so the cutover doesn't leave it serving stale values.
			if t.MigratingToWorker != "" {
				resultsCh <- pushOne(t.SandboxID, t.MigratingToWorker)
			}
		}(t)
	}
	wg.Wait()
	close(resultsCh)

	// Roll up results. A sandbox is "refreshed" if at least one of its push
	// attempts (source or destination) returned updated=true. A failure on
	// only one side of a migration is logged but doesn't downgrade the
	// sandbox's refreshed status — the other side handled it.
	successBySandbox := make(map[string]bool, len(targets))
	for r := range resultsCh {
		if r.err != nil {
			log.Printf("secret-refresh: store=%s name=%s sandbox=%s: %v", storeID, name, r.sandboxID, r.err)
			failures = append(failures, fmt.Sprintf("%s: %v", r.sandboxID, r.err))
			continue
		}
		if r.ok {
			successBySandbox[r.sandboxID] = true
		}
	}
	for _, ok := range successBySandbox {
		if ok {
			refreshed++
		}
	}
	if refreshed > 0 {
		log.Printf("secret-refresh: store=%s name=%s refreshed=%d failures=%d", storeID, name, refreshed, len(failures))
	}
	return refreshed, failures
}

