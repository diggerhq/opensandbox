package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/opensandbox/opensandbox/internal/auth"
	"github.com/opensandbox/opensandbox/internal/db"
)

// createSnapshot handles POST /api/snapshots — creates a pre-built named snapshot from a declarative image.
func (s *Server) createSnapshot(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}

	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "org context required"})
	}

	var req struct {
		Name  string          `json:"name"`
		Image json.RawMessage `json:"image"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body: " + err.Error()})
	}

	if req.Name == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "name is required"})
	}
	if len(req.Image) == 0 {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "image is required"})
	}

	ctx := c.Request().Context()

	// Check if name is already taken
	existing, err := s.store.GetImageCacheByName(ctx, orgID, req.Name)
	if err == nil && existing != nil {
		return c.JSON(http.StatusConflict, map[string]string{"error": "snapshot name already exists"})
	}

	// SSE streaming path
	if c.Request().Header.Get("Accept") == "text/event-stream" {
		return s.createSnapshotWithSSE(c, ctx, orgID, req.Name, req.Image)
	}

	// Non-streaming path
	ic, err := s.createSnapshotCore(ctx, orgID, req.Name, req.Image, nil)
	if err != nil {
		log.Printf("snapshot: build failed for %q: %v", req.Name, err)
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	log.Printf("snapshot: created %q", req.Name)
	return c.JSON(http.StatusCreated, ic)
}

// createSnapshotCore contains the shared snapshot creation logic.
func (s *Server) createSnapshotCore(ctx context.Context, orgID uuid.UUID, name string, imageJSON json.RawMessage, logFn BuildLogFunc) (*db.ImageCache, error) {
	checkpointID, err := s.resolveImageManifest(ctx, orgID, imageJSON, logFn)
	if err != nil {
		return nil, fmt.Errorf("image build failed: %w", err)
	}

	// Parse manifest for hash computation
	var manifest ImageManifest
	_ = json.Unmarshal(imageJSON, &manifest)
	contentHash := computeManifestHash(&manifest)

	// Check if an unnamed cache entry already exists with this hash — if so, just name it
	cached, err := s.store.GetImageCacheByHash(ctx, orgID, contentHash)
	if err == nil && cached.Name == nil {
		n := name
		ic := &db.ImageCache{
			ID:           uuid.New(),
			OrgID:        orgID,
			ContentHash:  contentHash + ":named:" + name,
			CheckpointID: &checkpointID,
			Name:         &n,
			Manifest:     imageJSON,
			Status:       "ready",
		}
		if createErr := s.store.CreateImageCache(ctx, ic); createErr != nil {
			return nil, fmt.Errorf("failed to save snapshot: %w", createErr)
		}
		return ic, nil
	}

	// Create a new named entry
	n := name
	ic := &db.ImageCache{
		ID:           uuid.New(),
		OrgID:        orgID,
		ContentHash:  contentHash,
		CheckpointID: &checkpointID,
		Name:         &n,
		Manifest:     imageJSON,
		Status:       "ready",
	}
	if createErr := s.store.CreateImageCache(ctx, ic); createErr != nil {
		// Might already exist from the resolveImageManifest call
		existing, readErr := s.store.GetImageCacheByHash(ctx, orgID, contentHash)
		if readErr == nil {
			return existing, nil
		}
		return nil, fmt.Errorf("failed to save snapshot: %w", createErr)
	}

	log.Printf("snapshot: created %q (checkpoint=%s)", name, checkpointID)
	return ic, nil
}

// createSnapshotWithSSE handles snapshot creation with SSE build log streaming.
func (s *Server) createSnapshotWithSSE(c echo.Context, ctx context.Context, orgID uuid.UUID, name string, imageJSON json.RawMessage) error {
	flusher, ok := c.Response().Writer.(http.Flusher)
	if !ok {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "streaming not supported"})
	}

	c.Response().Header().Set("Content-Type", "text/event-stream")
	c.Response().Header().Set("Cache-Control", "no-cache")
	c.Response().Header().Set("Connection", "keep-alive")
	c.Response().WriteHeader(http.StatusOK)
	flusher.Flush()

	emit := func(eventType string, payload interface{}) {
		data, _ := json.Marshal(payload)
		fmt.Fprintf(c.Response(), "event: %s\ndata: %s\n\n", eventType, data)
		flusher.Flush()
	}

	// Send SSE keepalive comments every 15s to prevent Cloudflare 524 timeouts
	// during long build steps (e.g., installing Rust takes ~3 minutes with no output).
	keepaliveDone := make(chan struct{})
	defer close(keepaliveDone)
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				fmt.Fprintf(c.Response(), ": keepalive\n\n")
				flusher.Flush()
			case <-keepaliveDone:
				return
			case <-ctx.Done():
				return
			}
		}
	}()

	logFn := BuildLogFunc(func(step int, stepType string, message string) {
		emit("build_log", map[string]interface{}{
			"step":    step,
			"type":    stepType,
			"message": message,
		})
	})

	ic, err := s.createSnapshotCore(ctx, orgID, name, imageJSON, logFn)
	if err != nil {
		emit("error", map[string]string{"error": err.Error()})
		return nil
	}

	emit("result", ic)
	return nil
}

// listSnapshots handles GET /api/snapshots — lists all named snapshots for the org.
func (s *Server) listSnapshots(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}

	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "org context required"})
	}

	snapshots, err := s.store.ListImageCacheByOrg(c.Request().Context(), orgID, true)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	if snapshots == nil {
		snapshots = []db.ImageCache{}
	}

	return c.JSON(http.StatusOK, snapshots)
}

// getSnapshot handles GET /api/snapshots/:name — gets a snapshot by name.
func (s *Server) getSnapshot(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}

	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "org context required"})
	}

	name := c.Param("name")
	snapshot, err := s.store.GetImageCacheByName(c.Request().Context(), orgID, name)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "snapshot not found"})
	}

	return c.JSON(http.StatusOK, snapshot)
}

// deleteSnapshot handles DELETE /api/snapshots/:name — deletes a named snapshot.
func (s *Server) deleteSnapshot(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}

	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "org context required"})
	}

	name := c.Param("name")
	if err := s.store.DeleteImageCacheByName(c.Request().Context(), orgID, name); err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
	}

	return c.NoContent(http.StatusNoContent)
}
