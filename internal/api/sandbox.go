package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/opensandbox/opensandbox/internal/auth"
	"github.com/opensandbox/opensandbox/internal/controlplane"
	"github.com/opensandbox/opensandbox/internal/db"
	"github.com/opensandbox/opensandbox/pkg/types"
	pb "github.com/opensandbox/opensandbox/proto/worker"
)


func (s *Server) createSandbox(c echo.Context) error {
	var cfg types.SandboxConfig
	if err := c.Bind(&cfg); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "invalid request body: " + err.Error(),
		})
	}

	// Validate CPU/memory against allowed tiers.
	// Allowed tiers (memoryMB → vCPU): 1024→1, 4096→1, 8192→2, 16384→4, 32768→8, 65536→16.
	if err := types.ValidateResourceTier(&cfg); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": err.Error(),
		})
	}

	ctx := c.Request().Context()

	// Check org quota and plan enforcement
	orgID, hasOrg := auth.GetOrgID(c)
	var org *db.Org
	if hasOrg && s.store != nil {
		var err error
		org, err = s.store.GetOrg(ctx, orgID)
		if err == nil {
			// Concurrent sandbox limit applies to all plans.
			count, err := s.store.CountActiveSandboxes(ctx, orgID)
			if err == nil && count >= org.MaxConcurrentSandboxes {
				return c.JSON(http.StatusTooManyRequests, map[string]string{
					"error": "concurrent sandbox limit reached",
				})
			}

			// Free-tier: trial credits gate + machine-size restriction.
			if org.Plan == "free" {
				if org.FreeCreditsRemainingCents <= 0 {
					return c.JSON(http.StatusPaymentRequired, map[string]string{
						"error": "free trial credits exhausted — upgrade to pro to create new sandboxes",
					})
				}
				if cfg.MemoryMB > 4096 || cfg.CpuCount > 1 {
					return c.JSON(http.StatusPaymentRequired, map[string]string{
						"error": "upgrade to pro for larger instances",
					})
				}
			}

			// Default to 4GB/1vCPU if not specified (all plans)
			if cfg.MemoryMB == 0 {
				cfg.MemoryMB = 4096
				cfg.CpuCount = 1
			}
		}
	}

	// Disk size validation
	if cfg.DiskMB == 0 {
		cfg.DiskMB = 20480 // default 20GB
	}
	if cfg.DiskMB < 20480 {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "diskMB must be at least 20480 (20GB)",
		})
	}
	if cfg.DiskMB > 262144 {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "diskMB cannot exceed 262144 (256GB)",
		})
	}
	if org != nil {
		if org.Plan == "free" && cfg.DiskMB > 20480 {
			return c.JSON(http.StatusPaymentRequired, map[string]string{
				"error": "upgrade to pro for larger disk sizes",
			})
		}
		maxDisk := org.MaxDiskMB
		if maxDisk == 0 {
			maxDisk = 20480
		}
		if cfg.DiskMB > maxDisk {
			return c.JSON(http.StatusForbidden, map[string]string{
				"error": fmt.Sprintf("disk size %dMB exceeds org limit of %dMB", cfg.DiskMB, maxDisk),
			})
		}
	}

	// Declarative image or named snapshot → resolve to checkpoint and use createFromCheckpoint flow
	if len(cfg.ImageManifest) > 0 || cfg.Snapshot != "" {
		if !hasOrg {
			return c.JSON(http.StatusUnauthorized, map[string]string{"error": "org context required for image/snapshot creation"})
		}
		// Check if the client wants build log streaming (SSE)
		wantsSSE := c.Request().Header.Get("Accept") == "text/event-stream"

		if wantsSSE {
			return s.createSandboxWithSSE(c, ctx, orgID, cfg)
		}

		// Non-streaming path
		var checkpointID uuid.UUID
		var err error

		if cfg.Snapshot != "" {
			checkpointID, err = s.resolveSnapshot(ctx, orgID, cfg.Snapshot)
		} else {
			checkpointID, err = s.resolveImageManifest(ctx, orgID, cfg.ImageManifest, nil)
		}
		if err != nil {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
		}

		c.SetParamNames("checkpointId")
		c.SetParamValues(checkpointID.String())
		// Forward user-supplied envs and secret store so they can be applied
		// on the fork. User envs override checkpoint's stored envs.
		// Secret store: if checkpoint has none, user can attach one at fork time.
		// If checkpoint already has one, user cannot override it.
		result, status, cpErr := s.createFromCheckpointCore(c, cfg.Envs, cfg.SecretStore, cfg.Metadata)
		if cpErr != nil {
			return c.JSON(status, map[string]string{"error": cpErr.Error()})
		}
		return c.JSON(status, result)
	}

	// Fresh-create path: resolve the secret store (if any) into cfg.SecretEnvs.
	// This is intentionally below the snapshot/image branch so the snapshot
	// path never resolves a user-supplied store — forks inherit only.
	if cfg.SecretStore != "" && hasOrg {
		if err := s.resolveSecretStoreInto(ctx, orgID, &cfg); err != nil {
			log.Printf("api: %v", err)
			return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
	}

	// Server mode with worker registry: dispatch to remote worker via gRPC
	if s.workerRegistry != nil {
		return s.createSandboxRemote(c, ctx, cfg, orgID, hasOrg)
	}

	// Combined/worker mode: create locally
	if s.manager == nil {
		return c.JSON(http.StatusServiceUnavailable, errSandboxNotAvailable)
	}

	sb, err := s.manager.Create(ctx, cfg)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": err.Error(),
		})
	}

	// Register with sandbox router for rolling timeout tracking.
	// timeout == 0 means "persistent" (no auto-hibernate). Negative values are
	// normalized to 0 for safety.
	if s.router != nil {
		timeout := cfg.Timeout
		if timeout < 0 {
			timeout = 0
		}
		s.router.Register(sb.ID, time.Duration(timeout)*time.Second)
	}

	// Initialize per-sandbox SQLite if available
	if s.sandboxDBs != nil {
		sdb, err := s.sandboxDBs.Get(sb.ID)
		if err == nil {
			_ = sdb.LogEvent("created", map[string]string{
				"sandbox_id": sb.ID,
				"template":   cfg.Template,
			})
		}
	}

	// Issue sandbox-scoped JWT for combined mode (24h TTL — independent of sandbox idle timeout)
	if s.jwtIssuer != nil {
		token, err := s.jwtIssuer.IssueSandboxToken(orgID, sb.ID, s.workerID, 24*time.Hour)
		if err == nil {
			sb.Token = token
		}
	}

	// Write session record to PG if available
	if s.store != nil && hasOrg {
		cfgJSON, _ := json.Marshal(cfgForPersistence(cfg))
		metadataJSON, _ := json.Marshal(cfg.Metadata)
		region := s.region
		if region == "" {
			region = "local"
		}
		workerID := s.workerID
		if workerID == "" {
			workerID = "w-local-1"
		}
		template := cfg.Template
		if template == "" {
			template = "default"
		}
		_, _ = s.store.CreateSandboxSession(ctx, sb.ID, orgID, auth.GetUserID(c), template, region, workerID, cfgJSON, metadataJSON)
	}

	return c.JSON(http.StatusCreated, sb)
}

// createSandboxWithSSE handles sandbox creation with SSE build log streaming.
// Streams build_log events during image build, then emits the final result event.
func (s *Server) createSandboxWithSSE(c echo.Context, ctx context.Context, orgID uuid.UUID, cfg types.SandboxConfig) error {
	flusher, ok := c.Response().Writer.(http.Flusher)
	if !ok {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "streaming not supported"})
	}

	// Set SSE headers
	c.Response().Header().Set("Content-Type", "text/event-stream")
	c.Response().Header().Set("Cache-Control", "no-cache")
	c.Response().Header().Set("Connection", "keep-alive")
	c.Response().WriteHeader(http.StatusOK)
	flusher.Flush()

	// Helper to emit SSE events
	emit := func(eventType string, payload interface{}) {
		data, _ := json.Marshal(payload)
		fmt.Fprintf(c.Response(), "event: %s\ndata: %s\n\n", eventType, data)
		flusher.Flush()
	}

	// Send SSE keepalive comments every 15s to prevent Cloudflare 524 timeouts
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

	// Build log callback — emits SSE events during image build
	logFn := BuildLogFunc(func(step int, stepType string, message string) {
		emit("build_log", map[string]interface{}{
			"step":    step,
			"type":    stepType,
			"message": message,
		})
	})

	// Resolve image or snapshot to checkpoint ID
	var checkpointID uuid.UUID
	var err error

	if cfg.Snapshot != "" {
		emit("build_log", map[string]interface{}{"step": 0, "type": "resolve", "message": "Resolving snapshot '" + cfg.Snapshot + "'..."})
		checkpointID, err = s.resolveSnapshot(ctx, orgID, cfg.Snapshot)
	} else {
		checkpointID, err = s.resolveImageManifest(ctx, orgID, cfg.ImageManifest, logFn)
	}
	if err != nil {
		emit("error", map[string]string{"error": err.Error()})
		return nil
	}

	// Create sandbox from checkpoint
	emit("build_log", map[string]interface{}{"step": 0, "type": "create", "message": "Creating sandbox from image..."})

	c.SetParamNames("checkpointId")
	c.SetParamValues(checkpointID.String())
	result, _, cpErr := s.createFromCheckpointCore(c, cfg.Envs, cfg.SecretStore, cfg.Metadata)
	if cpErr != nil {
		emit("error", map[string]string{"error": cpErr.Error()})
		return nil
	}

	emit("result", result)
	return nil
}

// resolveSecretStoreInto looks up the named secret store, decrypts its
// entries, and writes them into cfg.SecretEnvs (NOT cfg.Envs) so the
// store-derived values are kept distinct from user-supplied envs all the way
// to the worker. EgressAllowlist and SecretAllowedHosts are also derived from
// the store. cfg.Envs is left untouched.
//
// This helper is the single place where secret store name → resolved values
// happens, so the same logic runs on both fresh creates and on
// fork-from-checkpoint inheritance. If the user updates a secret between
// snapshot and fork, the fork sees the new value.
func (s *Server) resolveSecretStoreInto(ctx context.Context, orgID [16]byte, cfg *types.SandboxConfig) error {
	if cfg.SecretStore == "" || s.store == nil {
		return nil
	}
	store, err := s.store.GetSecretStoreByName(ctx, orgID, cfg.SecretStore)
	if err != nil {
		return fmt.Errorf("secret store not found: %s", cfg.SecretStore)
	}

	cfg.EgressAllowlist = store.EgressAllowlist

	secrets, err := s.store.DecryptSecretEntries(ctx, store.ID)
	if err != nil {
		return fmt.Errorf("failed to decrypt secrets for store %s: %w", cfg.SecretStore, err)
	}
	if len(secrets) == 0 {
		return nil
	}
	if cfg.SecretEnvs == nil {
		cfg.SecretEnvs = make(map[string]string, len(secrets))
	}
	for _, secret := range secrets {
		cfg.SecretEnvs[secret.Name] = secret.Value
		if len(secret.AllowedHosts) > 0 {
			if cfg.SecretAllowedHosts == nil {
				cfg.SecretAllowedHosts = make(map[string][]string)
			}
			cfg.SecretAllowedHosts[secret.Name] = secret.AllowedHosts
		}
	}
	return nil
}

// cfgForPersistence returns a copy of cfg suitable for marshaling into PG
// (cp.SandboxConfig / sandbox_sessions.config_json).
//
// SecretEnvs is json:"-" so plaintext secret values never reach PG by
// construction. We still scrub:
//   - SecretAllowedHosts entries that name store-derived keys (those names
//     are a property of the store and should be re-derived on fork)
//   - the store-level EgressAllowlist (same reason)
//
// Only the store NAME is kept on the persisted config; forks re-resolve fresh.
func cfgForPersistence(cfg types.SandboxConfig) types.SandboxConfig {
	if len(cfg.SecretEnvs) > 0 && len(cfg.SecretAllowedHosts) > 0 {
		stripped := make(map[string][]string, len(cfg.SecretAllowedHosts))
		for k, v := range cfg.SecretAllowedHosts {
			if _, isSecret := cfg.SecretEnvs[k]; !isSecret {
				stripped[k] = v
			}
		}
		if len(stripped) == 0 {
			cfg.SecretAllowedHosts = nil
		} else {
			cfg.SecretAllowedHosts = stripped
		}
	}
	cfg.EgressAllowlist = nil
	return cfg
}

// flattenSecretAllowedHosts converts the internal per-secret allowed-hosts map
// (env var → host slice) into the proto wire shape (env var → comma-joined string).
// Mirror of parseSecretAllowedHosts in internal/worker/grpc_server.go.
func flattenSecretAllowedHosts(m map[string][]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for name, hosts := range m {
		if len(hosts) > 0 {
			out[name] = strings.Join(hosts, ",")
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// createSandboxRemote dispatches sandbox creation to a remote worker via gRPC.
func (s *Server) createSandboxRemote(c echo.Context, ctx context.Context, cfg types.SandboxConfig, orgID [16]byte, hasOrg bool) error {
	// Select region (explicit header, or default to server's region)
	region := c.Request().Header.Get("Fly-Region")
	if region == "" {
		region = s.region
	}
	if region == "" {
		region = "iad"
	}

	worker, grpcClient, err := s.workerRegistry.GetLeastLoadedWorker(region)
	if err != nil {
		// No worker immediately available — poll for up to 30s
		// (scaler may be launching a new worker)
		deadline := time.After(30 * time.Second)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for err != nil {
			select {
			case <-deadline:
				return c.JSON(http.StatusServiceUnavailable, map[string]string{
					"error": "no workers available in region " + region + " (waited 30s)",
				})
			case <-ctx.Done():
				return c.JSON(http.StatusServiceUnavailable, map[string]string{
					"error": "request cancelled while waiting for capacity",
				})
			case <-ticker.C:
				worker, grpcClient, err = s.workerRegistry.GetLeastLoadedWorker(region)
			}
		}
		log.Printf("sandbox: worker became available after queuing (region=%s)", region)
	}

	// Resolve template from DB (org-scoped lookup with public fallback)
	var templateRootfsKey, templateWorkspaceKey string
	var templateID *uuid.UUID
	if s.store != nil && hasOrg {
		tmpl, err := s.store.GetTemplateByName(ctx, orgID, cfg.Template)
		if err == nil {
			templateID = &tmpl.ID
			log.Printf("sandbox: resolved template %q (type=%s, id=%s)", cfg.Template, tmpl.TemplateType, tmpl.ID)
			// Sandbox-type templates provide S3 drive keys instead of ECR image refs
			if tmpl.TemplateType == "sandbox" && tmpl.RootfsS3Key != nil && tmpl.WorkspaceS3Key != nil {
				templateRootfsKey = *tmpl.RootfsS3Key
				templateWorkspaceKey = *tmpl.WorkspaceS3Key
				log.Printf("sandbox: using snapshot template drives: rootfs=%s, workspace=%s", templateRootfsKey, templateWorkspaceKey)
			}
		} else {
			log.Printf("sandbox: template %q lookup failed: %v", cfg.Template, err)
		}
	}

	// Dispatch via persistent gRPC connection.
	// Worker uses local cache for checkpoint forks (300ms) and downloads from S3
	// only on cold starts (~30s). 60s is enough for both paths.
	grpcTimeout := 60 * time.Second
	grpcCtx, cancel := context.WithTimeout(ctx, grpcTimeout)
	defer cancel()

	// Save requested resource limits — create with defaults (golden snapshot),
	// then scale up after creation. This avoids needing a golden per CPU config.
	requestedMemoryMB := cfg.MemoryMB
	requestedCpuCount := cfg.CpuCount

	// Pre-generate sandbox ID so we can create the session in PG before the
	// gRPC call. The worker's RecordScaleEvent needs the org_id from the
	// session row, which must exist before the worker looks it up.
	sandboxID := "sb-" + uuid.New().String()[:8]

	// Create session with "pending" status before dispatching to worker.
	if s.store != nil && hasOrg {
		template := cfg.Template
		if template == "" {
			template = "default"
		}
		cfgJSON, _ := json.Marshal(cfgForPersistence(cfg))
		metadataJSON, _ := json.Marshal(cfg.Metadata)
		_, _ = s.store.CreateSandboxSessionWithStatus(ctx, sandboxID, orgID, auth.GetUserID(c), template, region, worker.ID, cfgJSON, metadataJSON, "pending")
		if worker.GoldenVersion != "" {
			_ = s.store.SetSandboxGoldenVersion(ctx, sandboxID, worker.GoldenVersion)
		}
		if templateID != nil {
			_ = s.store.UpdateSandboxSessionTemplate(ctx, sandboxID, *templateID)
		}
	}

	grpcResp, err := grpcClient.CreateSandbox(grpcCtx, &pb.CreateSandboxRequest{
		SandboxId:            sandboxID,
		Template:             cfg.Template,
		Timeout:              int32(cfg.Timeout),
		Envs:                 cfg.Envs,
		NetworkEnabled:       cfg.NetworkEnabled,
		Port:                 int32(cfg.Port),
		TemplateRootfsKey:    templateRootfsKey,
		TemplateWorkspaceKey: templateWorkspaceKey,
		EgressAllowlist:      cfg.EgressAllowlist,
		SecretAllowedHosts:   flattenSecretAllowedHosts(cfg.SecretAllowedHosts),
		SecretEnvs:           cfg.SecretEnvs,
		DiskMb:               int32(cfg.DiskMB),
	})
	if err != nil {
		// Mark session as failed so it doesn't count as active.
		if s.store != nil {
			errMsg := err.Error()
			_ = s.store.UpdateSandboxSessionStatus(ctx, sandboxID, "failed", &errMsg)
		}
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "worker create failed: " + err.Error(),
		})
	}

	// Creation succeeded — promote session to running.
	if s.store != nil {
		_ = s.store.UpdateSandboxSessionStatus(ctx, sandboxID, "running", nil)
	}

	// Scale to requested resources after creation (virtio-mem hotplug + cgroup).
	// Golden snapshot has fixed CPU/RAM — we create with defaults then scale up.
	if requestedMemoryMB > 0 || requestedCpuCount > 0 {
		scaleMB := requestedMemoryMB
		if scaleMB <= 0 {
			scaleMB = 1024
		}
		cpuCount := requestedCpuCount
		if cpuCount <= 0 {
			cpuCount = scaleMB / 4096 // 1 vCPU per 4GB
			if cpuCount < 1 {
				cpuCount = 1
			}
		}
		maxMemBytes := int64(scaleMB) * 1024 * 1024
		cpuPeriod := int64(100000)
		cpuMax := int64(cpuCount) * cpuPeriod

		scaleCtx, scaleCancel := context.WithTimeout(ctx, 10*time.Second)
		_, scaleErr := grpcClient.SetSandboxLimits(scaleCtx, &pb.SetSandboxLimitsRequest{
			SandboxId:      grpcResp.SandboxId,
			MaxMemoryBytes: maxMemBytes,
			CpuMaxUsec:     cpuMax,
			CpuPeriodUsec:  cpuPeriod,
		})
		scaleCancel()
		if scaleErr != nil {
			log.Printf("sandbox: post-create scale failed for %s: %v (continuing with defaults)", grpcResp.SandboxId, scaleErr)
		}
	}

	// Issue sandbox-scoped JWT (24h TTL — independent of sandbox idle timeout)
	var token string
	if s.jwtIssuer != nil {
		t, err := s.jwtIssuer.IssueSandboxToken(orgID, grpcResp.SandboxId, worker.ID, 24*time.Hour)
		if err != nil {
			log.Printf("sandbox: failed to issue JWT: %v", err)
		} else {
			token = t
		}
	}

	s.emitEvent("create", grpcResp.SandboxId, worker.ID, fmt.Sprintf("created on %s", worker.ID[len(worker.ID)-8:]))

	resp := map[string]interface{}{
		"sandboxID": grpcResp.SandboxId,
		"token":     token,
		"status":    grpcResp.Status,
		"region":    region,
		"workerID":  worker.ID,
	}
	if s.sandboxDomain != "" {
		resp["sandboxDomain"] = s.sandboxDomain
	}

	return c.JSON(http.StatusCreated, resp)
}

func (s *Server) getSandbox(c echo.Context) error {
	id := c.Param("id")

	// Server mode with worker registry: look up from PG and issue fresh token
	if s.workerRegistry != nil {
		return s.getSandboxRemote(c, id)
	}

	// Combined/worker mode: look up locally
	if s.manager == nil {
		return c.JSON(http.StatusServiceUnavailable, errSandboxNotAvailable)
	}

	sb, err := s.manager.Get(c.Request().Context(), id)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{
			"error": err.Error(),
		})
	}

	orgID, hasOrg := auth.GetOrgID(c)
	if s.jwtIssuer != nil {
		if hasOrg {
			token, err := s.jwtIssuer.IssueSandboxToken(orgID, id, s.workerID, 24*time.Hour)
			if err == nil {
				sb.Token = token
			}
		}
	}

	return c.JSON(http.StatusOK, s.withTagsHydrated(c, sb, id))
}

// getSandboxRemote looks up a sandbox via the PG session record and returns
// the worker's connectURL + a fresh JWT.
func (s *Server) getSandboxRemote(c echo.Context, sandboxID string) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "database not configured",
		})
	}

	session, err := s.store.GetSandboxSession(c.Request().Context(), sandboxID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{
			"error": "sandbox not found",
		})
	}

	orgID, _ := auth.GetOrgID(c)

	// Hibernated sandboxes have no worker
	if session.Status == "hibernated" {
		resp := map[string]interface{}{
			"sandboxID": sandboxID,
			"status":    "hibernated",
			"region":    session.Region,
			"template":  session.Template,
			"startedAt": session.StartedAt,
		}
		if s.sandboxDomain != "" {
			resp["sandboxDomain"] = s.sandboxDomain
		}
		s.mergeTagsInto(c.Request().Context(), orgID, resp, sandboxID)
		return c.JSON(http.StatusOK, resp)
	}

	// Issue a fresh token
	var token string
	if s.jwtIssuer != nil {
		t, err := s.jwtIssuer.IssueSandboxToken(orgID, sandboxID, session.WorkerID, 24*time.Hour)
		if err == nil {
			token = t
		}
	}

	resp := map[string]interface{}{
		"sandboxID": sandboxID,
		"token":     token,
		"status":    session.Status,
		"region":    session.Region,
		"workerID":  session.WorkerID,
		"startedAt": session.StartedAt,
		"template":  session.Template,
	}
	if s.sandboxDomain != "" {
		resp["sandboxDomain"] = s.sandboxDomain
	}
	if session.PatchError != nil {
		resp["patchError"] = *session.PatchError
	}

	s.mergeTagsInto(c.Request().Context(), orgID, resp, sandboxID)
	return c.JSON(http.StatusOK, resp)
}

func (s *Server) killSandbox(c echo.Context) error {
	id := c.Param("id")

	// Server mode with worker registry: dispatch destroy via gRPC
	if s.workerRegistry != nil {
		return s.killSandboxRemote(c, id)
	}

	// Combined/worker mode: kill locally
	if s.manager == nil {
		return c.JSON(http.StatusServiceUnavailable, errSandboxNotAvailable)
	}

	// Mark stopped immediately so it no longer counts toward concurrency limits.
	if s.store != nil {
		_ = s.store.UpdateSandboxSessionStatus(c.Request().Context(), id, "stopped", nil)
		s.cleanupPreviewURLs(c.Request().Context(), id)
	}

	if err := s.manager.Kill(c.Request().Context(), id); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": err.Error(),
		})
	}

	// Unregister from sandbox router
	if s.router != nil {
		s.router.Unregister(id)
	}

	if s.sandboxDBs != nil {
		_ = s.sandboxDBs.Remove(id)
	}

	return c.NoContent(http.StatusNoContent)
}

// killSandboxRemote dispatches sandbox destruction to the appropriate worker via gRPC.
func (s *Server) killSandboxRemote(c echo.Context, sandboxID string) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "database not configured",
		})
	}

	session, err := s.store.GetSandboxSession(c.Request().Context(), sandboxID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{
			"error": "sandbox not found",
		})
	}

	// Mark stopped immediately so it no longer counts toward concurrency limits.
	// The actual VM teardown below is best-effort.
	_ = s.store.UpdateSandboxSessionStatus(c.Request().Context(), sandboxID, "stopped", nil)
	s.cleanupPreviewURLs(c.Request().Context(), sandboxID)

	// Attempt gRPC destroy (best-effort)
	client, err := s.workerRegistry.GetWorkerClient(session.WorkerID)
	if err != nil {
		log.Printf("sandbox: worker %s unreachable for destroy: %v", session.WorkerID, err)
		return c.NoContent(http.StatusNoContent)
	}

	grpcCtx, cancel := context.WithTimeout(c.Request().Context(), 10*time.Second)
	defer cancel()

	if _, err := client.DestroySandbox(grpcCtx, &pb.DestroySandboxRequest{SandboxId: sandboxID}); err != nil {
		log.Printf("sandbox: gRPC destroy failed for %s: %v", sandboxID, err)
	}

	_ = s.store.UpdateSandboxSessionStatus(c.Request().Context(), sandboxID, "stopped", nil)
	s.cleanupPreviewURLs(c.Request().Context(), sandboxID)

	if s.sandboxAPIProxy != nil {
		s.sandboxAPIProxy.InvalidateRouteCache(sandboxID)
	}

	s.emitEvent("destroy", sandboxID, session.WorkerID, "destroyed")

	return c.NoContent(http.StatusNoContent)
}

func (s *Server) listSandboxes(c echo.Context) error {
	// Server mode with worker registry: query PG for org's running sandboxes
	if s.workerRegistry != nil {
		return s.listSandboxesRemote(c)
	}

	// Combined/worker mode: list locally
	if s.manager == nil {
		return c.JSON(http.StatusServiceUnavailable, errSandboxNotAvailable)
	}

	sandboxes, err := s.manager.List(c.Request().Context())
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": err.Error(),
		})
	}

	return c.JSON(http.StatusOK, s.withTagsHydratedList(c, sandboxes))
}

// listSandboxesRemote queries PG for the org's running sandboxes and returns
// connectURL + fresh JWT for each.
func (s *Server) listSandboxesRemote(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "database not configured",
		})
	}

	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{
			"error": "org context required",
		})
	}

	sessions, err := s.store.ListSandboxSessions(c.Request().Context(), orgID, "running", 100, 0)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": err.Error(),
		})
	}

	result := make([]map[string]interface{}, 0, len(sessions))
	ids := make([]string, 0, len(sessions))
	for _, sess := range sessions {
		entry := map[string]interface{}{
			"sandboxID": sess.SandboxID,
			"status":    sess.Status,
			"region":    sess.Region,
			"workerID":  sess.WorkerID,
			"template":  sess.Template,
			"startedAt": sess.StartedAt,
		}

		// Issue fresh JWT
		if s.jwtIssuer != nil {
			token, err := s.jwtIssuer.IssueSandboxToken(orgID, sess.SandboxID, sess.WorkerID, 24*time.Hour)
			if err == nil {
				entry["token"] = token
			}
		}

		result = append(result, entry)
		ids = append(ids, sess.SandboxID)
	}

	// One batched tag fetch for all sandboxes in the page — cheaper
	// than N GetSandboxTags round-trips. Fail-soft: if the tag table
	// is unreachable, list still returns without tag fields rather
	// than 500.
	if s.store != nil {
		if sets, err := s.store.GetSandboxTagsMulti(c.Request().Context(), orgID, ids); err == nil {
			for _, entry := range result {
				sid, _ := entry["sandboxID"].(string)
				set := sets[sid]
				if set.Tags == nil {
					set.Tags = map[string]string{}
				}
				entry["tags"] = set.Tags
				if set.LastUpdatedAt != nil {
					entry["tagsLastUpdatedAt"] = set.LastUpdatedAt.UTC().Format(time.RFC3339)
				} else {
					entry["tagsLastUpdatedAt"] = nil
				}
			}
		}
	}

	return c.JSON(http.StatusOK, result)
}

func (s *Server) setTimeout(c echo.Context) error {
	if s.router == nil {
		return c.JSON(http.StatusServiceUnavailable, errSandboxNotAvailable)
	}

	id := c.Param("id")

	var req types.TimeoutRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "invalid request body: " + err.Error(),
		})
	}

	// timeout == 0 means "persistent" (disable auto-hibernate). Negative values
	// are invalid.
	if req.Timeout < 0 {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "timeout must be non-negative (0 disables auto-hibernate)",
		})
	}

	s.router.SetTimeout(id, time.Duration(req.Timeout)*time.Second)

	return c.NoContent(http.StatusNoContent)
}

// migrateSandbox performs live migration of a sandbox to a different worker.
// POST /api/sandboxes/:id/migrate {"targetWorker": "w-azure-osb-worker-xxx"}
func (s *Server) migrateSandbox(c echo.Context) error {
	id := c.Param("id")
	ctx := c.Request().Context()

	var req struct {
		TargetWorker string `json:"targetWorker"`
	}
	if err := c.Bind(&req); err != nil || req.TargetWorker == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "targetWorker is required"})
	}

	if s.workerRegistry == nil || s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "migration requires server mode with worker registry"})
	}

	// Look up sandbox to find source worker
	session, err := s.store.GetSandboxSession(ctx, id)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "sandbox not found"})
	}
	if session.Status != "running" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "sandbox must be running to migrate"})
	}

	// Mark as migrating — blocks exec/proxy routing until migration completes
	migrationDone := false
	if s.store != nil {
		if err := s.store.SetMigrating(ctx, id, req.TargetWorker); err != nil {
			log.Printf("migrate %s: failed to set migrating state: %v", id, err)
		}
		// Guarantee we revert on failure
		defer func() {
			if !migrationDone && s.store != nil {
				s.store.FailMigration(ctx, id)
			}
		}()
	}

	// Get source and target worker gRPC clients
	sourceClient, err := s.workerRegistry.GetWorkerClient(session.WorkerID)
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "source worker unreachable: " + err.Error()})
	}
	targetClient, err := s.workerRegistry.GetWorkerClient(req.TargetWorker)
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "target worker unreachable: " + err.Error()})
	}

	t0 := time.Now()

	// Step 1: Pre-copy drives to S3 (thin overlay, never flatten).
	preCopyCtx, preCopyCancel := context.WithTimeout(ctx, 10*time.Minute)
	defer preCopyCancel()
	preCopyResp, err := sourceClient.PreCopyDrives(preCopyCtx, &pb.PreCopyDrivesRequest{
		SandboxId: id,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "pre-copy drives: " + err.Error()})
	}

	if preCopyResp.GoldenVersion == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "source sandbox has no goldenVersion — cannot live migrate safely; use hibernate/wake instead"})
	}

	log.Printf("migrate %s: drives pre-copied to S3 (%dms, golden=%s)", id, time.Since(t0).Milliseconds(), preCopyResp.GoldenVersion)

	// Step 2: Prepare target (downloads thin overlay, rebases if needed, starts QEMU -incoming).
	// CPU and memory come from the source worker — must match exactly for QEMU migration.
	cpuCount := preCopyResp.BaseCpuCount
	memoryMB := preCopyResp.BaseMemoryMb
	if cpuCount == 0 {
		cpuCount = 2
	}
	if memoryMB == 0 {
		memoryMB = 1024
	}

	prepCtx, prepCancel := context.WithTimeout(ctx, 10*time.Minute)
	defer prepCancel()
	prepResp, err := targetClient.PrepareMigrationIncoming(prepCtx, &pb.PrepareMigrationIncomingRequest{
		SandboxId:           id,
		CpuCount:            cpuCount,
		MemoryMb:            memoryMB,
		GuestPort:           80,
		Template:            session.Template,
		RootfsS3Key:         preCopyResp.RootfsKey,
		WorkspaceS3Key:      preCopyResp.WorkspaceKey,
		OverlayMode:         true,
		SourceGoldenVersion: preCopyResp.GoldenVersion,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "prepare target: " + err.Error()})
	}

	log.Printf("migrate %s: target prepared at %s (host port %d)", id, prepResp.IncomingAddr, prepResp.HostPort)

	// Step 3: Live migrate from source to target
	migrateCtx, migrateCancel := context.WithTimeout(ctx, 5*time.Minute)
	defer migrateCancel()
	_, err = sourceClient.LiveMigrate(migrateCtx, &pb.LiveMigrateRequest{
		SandboxId:    id,
		IncomingAddr: prepResp.IncomingAddr,
	})
	if err != nil {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 10*time.Second)
		targetClient.DestroySandbox(cleanCtx, &pb.DestroySandboxRequest{SandboxId: id})
		cleanCancel()
		log.Printf("migrate %s: live migrate failed, cleaned up target on %s: %v", id, req.TargetWorker, err)
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "live migrate: " + err.Error()})
	}

	log.Printf("migrate %s: QMP migration complete (%dms)", id, time.Since(t0).Milliseconds())

	// Step 4: Complete migration on target (reconnect agent, patch network)
	completeCtx, completeCancel := context.WithTimeout(ctx, 30*time.Second)
	defer completeCancel()
	_, err = targetClient.CompleteMigrationIncoming(completeCtx, &pb.CompleteMigrationIncomingRequest{
		SandboxId: id,
	})
	if err != nil {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 10*time.Second)
		targetClient.DestroySandbox(cleanCtx, &pb.DestroySandboxRequest{SandboxId: id})
		cleanCancel()
		log.Printf("migrate %s: complete failed, cleaned up target on %s: %v", id, req.TargetWorker, err)
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "complete migration: " + err.Error()})
	}

	// Step 5: Complete migration — update DB status and worker_id atomically.
	// Use background context — the request context may be close to expiry for large migrations.
	if s.store != nil {
		completeDBCtx, completeDBCancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := s.store.CompleteMigration(completeDBCtx, id, req.TargetWorker); err != nil {
			log.Printf("migrate %s: WARNING: CompleteMigration DB update failed: %v", id, err)
		}
		completeDBCancel()
	}
	migrationDone = true

	// Invalidate proxy route cache so next request routes to the new worker
	if s.sandboxAPIProxy != nil {
		s.sandboxAPIProxy.InvalidateRouteCache(id)
	}

	elapsed := time.Since(t0).Milliseconds()
	log.Printf("migrate %s: complete in %dms (source=%s target=%s)", id, elapsed, session.WorkerID, req.TargetWorker)
	s.emitEvent("migrate", id, req.TargetWorker, fmt.Sprintf("migrated from %s in %dms", session.WorkerID[len(session.WorkerID)-8:], elapsed))

	return c.JSON(http.StatusOK, map[string]interface{}{
		"sandboxID":    id,
		"sourceWorker": session.WorkerID,
		"targetWorker": req.TargetWorker,
		"elapsedMs":    elapsed,
	})
}

func (s *Server) setLimits(c echo.Context) error {
	id := c.Param("id")
	ctx := c.Request().Context()

	var req struct {
		MemoryMB   int `json:"memoryMB"`
		CPUPercent int `json:"cpuPercent"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "invalid request body: " + err.Error(),
		})
	}

	// Validate memory against allowed tiers
	if req.MemoryMB > 0 {
		vcpus, err := types.ValidateMemoryMB(req.MemoryMB)
		if err != nil {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
		if req.CPUPercent == 0 {
			req.CPUPercent = vcpus * 100
		}
	}

	// Free tier: block scaling beyond 4GB / 1 vCPU
	if orgID, hasOrg := auth.GetOrgID(c); hasOrg && s.store != nil {
		if org, err := s.store.GetOrg(ctx, orgID); err == nil && org.Plan == "free" {
			if req.MemoryMB > 4096 || req.CPUPercent > 100 {
				return c.JSON(http.StatusPaymentRequired, map[string]string{
					"error": "upgrade to pro for larger instances",
				})
			}
		}
	}

	// Convert to cgroup values
	maxMemoryBytes := int64(req.MemoryMB) * 1024 * 1024
	cpuMaxUsec := int64(req.CPUPercent) * 1000
	cpuPeriodUsec := int64(100000)

	// Server mode: dispatch to worker via gRPC
	if s.workerRegistry != nil {
		return s.setLimitsRemote(c, id, 0, maxMemoryBytes, cpuMaxUsec, cpuPeriodUsec)
	}

	// Combined mode: apply locally
	if s.manager == nil {
		return c.JSON(http.StatusServiceUnavailable, errSandboxNotAvailable)
	}

	if err := s.manager.SetResourceLimits(ctx, id, 0, maxMemoryBytes, cpuMaxUsec, cpuPeriodUsec); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": err.Error(),
		})
	}

	return c.JSON(http.StatusOK, map[string]interface{}{
		"sandboxID":  id,
		"memoryMB":   req.MemoryMB,
		"cpuPercent": req.CPUPercent,
	})
}

// scaleSandbox is a simplified scaling endpoint: POST /sandboxes/:id/scale
// Accepts {"memoryMB": 2048} and auto-calculates CPU (1 vCPU per 1GB).
func (s *Server) scaleSandbox(c echo.Context) error {
	id := c.Param("id")

	var req struct {
		MemoryMB int `json:"memoryMB"`
	}
	if err := c.Bind(&req); err != nil || req.MemoryMB <= 0 {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "memoryMB is required and must be positive"})
	}

	// Validate memory against allowed tiers
	vcpus, err := types.ValidateMemoryMB(req.MemoryMB)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}

	// Free tier: block scaling beyond 4GB / 1 vCPU
	if orgID, hasOrg := auth.GetOrgID(c); hasOrg && s.store != nil {
		if org, err := s.store.GetOrg(c.Request().Context(), orgID); err == nil && org.Plan == "free" {
			if req.MemoryMB > 4096 {
				return c.JSON(http.StatusPaymentRequired, map[string]string{
					"error": "upgrade to pro for larger instances",
				})
			}
		}
	}

	cpuPercent := vcpus * 100
	maxMemoryBytes := int64(req.MemoryMB) * 1024 * 1024
	cpuMaxUsec := int64(cpuPercent) * 1000
	cpuPeriodUsec := int64(100000)

	if s.workerRegistry != nil {
		return s.setLimitsRemote(c, id, 0, maxMemoryBytes, cpuMaxUsec, cpuPeriodUsec)
	}
	if s.manager == nil {
		return c.JSON(http.StatusServiceUnavailable, errSandboxNotAvailable)
	}
	if err := s.manager.SetResourceLimits(c.Request().Context(), id, 0, maxMemoryBytes, cpuMaxUsec, cpuPeriodUsec); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"sandboxID": id,
		"memoryMB":  req.MemoryMB,
		"cpuPercent": cpuPercent,
	})
}

func (s *Server) setLimitsRemote(c echo.Context, sandboxID string, maxPids int32, maxMemoryBytes, cpuMaxUsec, cpuPeriodUsec int64) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "database not configured",
		})
	}

	ctx := c.Request().Context()

	session, err := s.store.GetSandboxSession(ctx, sandboxID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "sandbox not found"})
	}
	if session.Status != "running" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "sandbox is not running"})
	}

	requestedMemMB := int(maxMemoryBytes / (1024 * 1024))
	requestedCPUs := int(cpuMaxUsec / 1000 / 100) // cpuPercent / 100
	if requestedCPUs < 1 {
		requestedCPUs = 1
	}

	workerID := session.WorkerID

	// Step 1: Try to apply limits on the current worker.
	client, err := s.workerRegistry.GetWorkerClient(workerID)
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "worker unreachable"})
	}

	grpcCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	_, err = client.SetSandboxLimits(grpcCtx, &pb.SetSandboxLimitsRequest{
		SandboxId:      sandboxID,
		MaxPids:        maxPids,
		MaxMemoryBytes: maxMemoryBytes,
		CpuMaxUsec:     cpuMaxUsec,
		CpuPeriodUsec:  cpuPeriodUsec,
	})
	cancel()

	// Step 2: If the worker can't fit the memory, migrate and retry.
	migrated := false
	if err != nil {
		log.Printf("scale-limits %s: SetSandboxLimits error: %v", sandboxID, err)
	}
	if err != nil && strings.Contains(err.Error(), "insufficient_capacity") {
		log.Printf("scale-migrate %s: worker %s can't fit %dMB, finding migration target", sandboxID, workerID, requestedMemMB)

		targets := s.findScaleMigrationTargets(workerID, requestedMemMB)
		if len(targets) == 0 {
			return c.JSON(http.StatusServiceUnavailable, map[string]string{
				"error": "insufficient capacity on current worker and no migration target available",
			})
		}

		var migrateErr error
		var target *controlplane.WorkerEntry
		for _, candidate := range targets {
			// Mark as migrating before starting
			if s.store != nil {
				s.store.SetMigrating(ctx, sandboxID, candidate.ID)
			}

			migrateErr = s.migrateForScale(ctx, sandboxID, session, candidate, requestedMemMB, requestedCPUs)
			if migrateErr == nil {
				target = candidate
				break
			}
			log.Printf("scale-migrate %s: target %s failed: %v, trying next", sandboxID, candidate.ID, migrateErr)
			if s.store != nil {
				s.store.FailMigration(ctx, sandboxID)
			}
			// If prep rejected due to capacity, try next candidate
			if strings.Contains(migrateErr.Error(), "insufficient_capacity") {
				continue
			}
			break // non-capacity error, don't retry
		}

		if target == nil {
			return c.JSON(http.StatusServiceUnavailable, map[string]string{
				"error": "scale migration failed on all candidates: " + migrateErr.Error(),
			})
		}

		// Migration succeeded — update state.
		// Use background context in case the request context is close to expiry.
		if s.store != nil {
			dbCtx, dbCancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := s.store.CompleteMigration(dbCtx, sandboxID, target.ID); err != nil {
				log.Printf("scale-migrate %s: WARNING: CompleteMigration DB update failed: %v", sandboxID, err)
			}
			dbCancel()
		}
		if s.sandboxAPIProxy != nil {
			s.sandboxAPIProxy.InvalidateRouteCache(sandboxID)
		}
		workerID = target.ID
		migrated = true

		// Retry limits on the new worker
		newClient, clientErr := s.workerRegistry.GetWorkerClient(workerID)
		if clientErr != nil {
			return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "new worker unreachable after migration"})
		}

		retryCtx, retryCancel := context.WithTimeout(ctx, 30*time.Second)
		_, err = newClient.SetSandboxLimits(retryCtx, &pb.SetSandboxLimitsRequest{
			SandboxId:      sandboxID,
			MaxPids:        maxPids,
			MaxMemoryBytes: maxMemoryBytes,
			CpuMaxUsec:     cpuMaxUsec,
			CpuPeriodUsec:  cpuPeriodUsec,
		})
		retryCancel()

		if err != nil {
			log.Printf("scale-migrate %s: post-migration SetLimits failed on %s: %v", sandboxID, workerID, err)
			return c.JSON(http.StatusInternalServerError, map[string]string{
				"error": "set limits failed after migration: " + err.Error(),
			})
		}

		log.Printf("scale-migrate %s: migrated to %s and scaled to %dMB", sandboxID, workerID, requestedMemMB)
	} else if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "set limits failed: " + err.Error(),
		})
	}

	if migrated {
		s.emitEvent("migrate", sandboxID, workerID, fmt.Sprintf("auto-migrated for scale to %dMB", requestedMemMB))
	}
	// Only emit scale events for non-default sizes (4096MB is the creation default)
	if requestedMemMB != 4096 || migrated {
		s.emitEvent("scale", sandboxID, workerID, fmt.Sprintf("scaled to %dMB (migrated=%v)", requestedMemMB, migrated))
	}

	return c.JSON(http.StatusOK, map[string]interface{}{
		"sandboxID":  sandboxID,
		"workerID":   workerID,
		"memoryMB":   requestedMemMB,
		"cpuPercent": int(cpuMaxUsec / 1000),
		"migrated":   migrated,
		"ok":         true,
	})
}

// findScaleMigrationTargets finds workers with enough memory headroom for a scaled-up sandbox.
// Returns candidates sorted by most available memory first. Skips the source worker.
// Uses heartbeat data as a pre-filter — the actual capacity check happens on the worker
// during PrepareMigrationIncoming (which atomically checks and reserves).
func (s *Server) findScaleMigrationTargets(sourceWorkerID string, requestedMemMB int) []*controlplane.WorkerEntry {
	workers := s.workerRegistry.GetAllWorkers()
	type candidate struct {
		w         *controlplane.WorkerEntry
		available int
	}
	var candidates []candidate

	for _, w := range workers {
		if w.ID == sourceWorkerID {
			continue
		}
		if w.Draining {
			continue
		}
		if w.CPUPct > 90 || w.MemPct > 85 {
			continue
		}
		reserveMB := w.TotalMemoryMB / 5
		availableMB := w.TotalMemoryMB - w.CommittedMemoryMB - reserveMB
		if availableMB < requestedMemMB {
			continue
		}
		candidates = append(candidates, candidate{w, availableMB})
	}

	// Sort by most available first
	for i := 0; i < len(candidates); i++ {
		for j := i + 1; j < len(candidates); j++ {
			if candidates[j].available > candidates[i].available {
				candidates[i], candidates[j] = candidates[j], candidates[i]
			}
		}
	}

	result := make([]*controlplane.WorkerEntry, len(candidates))
	for i, c := range candidates {
		result[i] = c.w
	}
	return result
}

// migrateForScale performs a live migration to accommodate a resource scale-up.
func (s *Server) migrateForScale(ctx context.Context, sandboxID string, session *db.SandboxSession, target *controlplane.WorkerEntry, memoryMB, cpuCount int) error {
	sourceClient, err := s.workerRegistry.GetWorkerClient(session.WorkerID)
	if err != nil {
		return fmt.Errorf("source worker unreachable: %w", err)
	}
	targetClient, err := s.workerRegistry.GetWorkerClient(target.ID)
	if err != nil {
		return fmt.Errorf("target worker unreachable: %w", err)
	}

	t0 := time.Now()
	template := session.Template
	if template == "" {
		template = "default"
	}

	// Step 1: Pre-copy drives to S3 (thin overlay).
	preCopyCtx, preCopyCancel := context.WithTimeout(ctx, 10*time.Minute)
	defer preCopyCancel()
	preCopyResp, err := sourceClient.PreCopyDrives(preCopyCtx, &pb.PreCopyDrivesRequest{
		SandboxId: sandboxID,
	})
	if err != nil {
		return fmt.Errorf("pre-copy drives: %w", err)
	}

	// CPU and memory from source — must match for QEMU migration.
	baseCPU := preCopyResp.BaseCpuCount
	baseMem := preCopyResp.BaseMemoryMb
	if baseCPU == 0 {
		baseCPU = 2
	}
	if baseMem == 0 {
		baseMem = 1024
	}

	log.Printf("scale-migrate %s: drives pre-copied (%dms), migrating to %s (baseMem=%dMB, targetMem=%dMB, cpu=%d)",
		sandboxID, time.Since(t0).Milliseconds(), target.ID, baseMem, memoryMB, baseCPU)

	// Step 2: Prepare target with the SOURCE's base memory (must match for migration)
	prepCtx, prepCancel := context.WithTimeout(ctx, 10*time.Minute)
	defer prepCancel()
	prepResp, err := targetClient.PrepareMigrationIncoming(prepCtx, &pb.PrepareMigrationIncomingRequest{
		SandboxId:           sandboxID,
		CpuCount:            baseCPU,
		MemoryMb:            baseMem,
		GuestPort:           80,
		Template:            template,
		RootfsS3Key:         preCopyResp.RootfsKey,
		WorkspaceS3Key:      preCopyResp.WorkspaceKey,
		OverlayMode:         true,
		SourceGoldenVersion: preCopyResp.GoldenVersion,
		TargetMemoryMb:      int32(memoryMB),
	})
	if err != nil {
		log.Printf("scale-migrate %s: prepare target failed: %v", sandboxID, err)
		return fmt.Errorf("prepare target: %w", err)
	}

	// Step 2: Live migrate
	migrateCtx, migrateCancel := context.WithTimeout(ctx, 5*time.Minute)
	defer migrateCancel()
	_, err = sourceClient.LiveMigrate(migrateCtx, &pb.LiveMigrateRequest{
		SandboxId:    sandboxID,
		IncomingAddr: prepResp.IncomingAddr,
	})
	if err != nil {
		// Clean up orphaned target QEMU — prep succeeded but migration failed
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 10*time.Second)
		targetClient.DestroySandbox(cleanCtx, &pb.DestroySandboxRequest{SandboxId: sandboxID})
		cleanCancel()
		log.Printf("scale-migrate %s: live migrate failed, cleaned up target on %s: %v", sandboxID, target.ID, err)
		return fmt.Errorf("live migrate: %w", err)
	}

	// Step 3: Complete migration on target
	completeCtx, completeCancel := context.WithTimeout(ctx, 30*time.Second)
	defer completeCancel()
	_, err = targetClient.CompleteMigrationIncoming(completeCtx, &pb.CompleteMigrationIncomingRequest{
		SandboxId: sandboxID,
	})
	if err != nil {
		// Clean up orphaned target QEMU — migration transferred but completion failed
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 10*time.Second)
		targetClient.DestroySandbox(cleanCtx, &pb.DestroySandboxRequest{SandboxId: sandboxID})
		cleanCancel()
		log.Printf("scale-migrate %s: complete failed, cleaned up target on %s: %v", sandboxID, target.ID, err)
		return fmt.Errorf("complete migration: %w", err)
	}

	// Step 4: Update DB
	// DB update is handled by CompleteMigration in the caller

	log.Printf("scale-migrate %s: complete in %dms (source=%s target=%s, new=%dMB/%dvCPU)",
		sandboxID, time.Since(t0).Milliseconds(), session.WorkerID, target.ID, memoryMB, cpuCount)

	return nil
}

func (s *Server) hibernateSandbox(c echo.Context) error {
	id := c.Param("id")
	ctx := c.Request().Context()

	// Server mode: dispatch to worker via gRPC
	if s.workerRegistry != nil {
		return s.hibernateSandboxRemote(c, id)
	}

	// Combined mode: hibernate locally
	if s.manager == nil || s.checkpointStore == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "hibernation not available",
		})
	}

	result, err := s.manager.Hibernate(ctx, id, s.checkpointStore)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": err.Error(),
		})
	}

	// Mark hibernated in sandbox router
	if s.router != nil {
		timeout := 600 // default for explicit hibernate
		s.router.MarkHibernated(id, time.Duration(timeout)*time.Second)
	}

	// Record checkpoint in PG
	orgID, hasOrg := auth.GetOrgID(c)
	if s.store != nil && hasOrg {
		session, _ := s.store.GetSandboxSession(ctx, id)
		cfg := json.RawMessage("{}")
		if session != nil {
			cfg = session.Config
		}
		template := "base"
		region := s.region
		if session != nil {
			template = session.Template
			region = session.Region
		}
		_, superseded, _ := s.store.CreateHibernation(ctx, id, orgID, result.HibernationKey, result.SizeBytes, region, template, cfg)
		s.deleteSupersededHibernation(superseded)
		_ = s.store.UpdateSandboxSessionStatus(ctx, id, "hibernated", nil)
	}

	if s.sandboxDBs != nil {
		_ = s.sandboxDBs.Remove(id)
	}

	return c.JSON(http.StatusOK, map[string]interface{}{
		"sandboxID":      id,
		"status":         "hibernated",
		"hibernationKey": result.HibernationKey,
		"sizeBytes":      result.SizeBytes,
	})
}

// deleteSupersededHibernation best-effort removes a prior hibernation archive
// from S3 when it's been replaced by a new hibernation for the same sandbox.
// We only keep one hibernation per sandbox to bound storage growth.
func (s *Server) deleteSupersededHibernation(key string) {
	if s.checkpointStore == nil || key == "" || strings.HasPrefix(key, "local://") {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.checkpointStore.Delete(ctx, key); err != nil {
		log.Printf("api: failed to delete superseded hibernation %s: %v", key, err)
	}
}

func (s *Server) hibernateSandboxRemote(c echo.Context, sandboxID string) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "database not configured",
		})
	}

	session, err := s.store.GetSandboxSession(c.Request().Context(), sandboxID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "sandbox not found"})
	}
	if session.Status != "running" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "sandbox is not running"})
	}

	client, err := s.workerRegistry.GetWorkerClient(session.WorkerID)
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "worker unreachable"})
	}

	grpcCtx, cancel := context.WithTimeout(c.Request().Context(), 60*time.Second)
	defer cancel()

	grpcResp, err := client.HibernateSandbox(grpcCtx, &pb.HibernateSandboxRequest{
		SandboxId: sandboxID,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "hibernate failed: " + err.Error(),
		})
	}

	// Record hibernation in PG
	orgID, _ := auth.GetOrgID(c)
	_, superseded, _ := s.store.CreateHibernation(c.Request().Context(), sandboxID, orgID,
		grpcResp.CheckpointKey, grpcResp.SizeBytes,
		session.Region, session.Template, session.Config)
	s.deleteSupersededHibernation(superseded)
	_ = s.store.UpdateSandboxSessionStatus(c.Request().Context(), sandboxID, "hibernated", nil)

	// Invalidate the proxy route cache: wake may land the sandbox on a
	// different worker, so subsequent data-plane requests must re-resolve
	// the routing from the DB instead of hitting the old worker.
	if s.sandboxAPIProxy != nil {
		s.sandboxAPIProxy.InvalidateRouteCache(sandboxID)
	}

	resp := map[string]interface{}{
		"sandboxID":      sandboxID,
		"status":         "hibernated",
		"hibernationKey": grpcResp.CheckpointKey,
		"sizeBytes":      grpcResp.SizeBytes,
	}

	return c.JSON(http.StatusOK, resp)
}

func (s *Server) wakeSandbox(c echo.Context) error {
	id := c.Param("id")
	ctx := c.Request().Context()

	var req types.WakeRequest
	_ = c.Bind(&req)

	// Free-tier credits gate: refuse to wake if trial credits are exhausted.
	if orgID, ok := auth.GetOrgID(c); ok && s.store != nil {
		if org, err := s.store.GetOrg(ctx, orgID); err == nil {
			if org.Plan == "free" && org.FreeCreditsRemainingCents <= 0 {
				return c.JSON(http.StatusPaymentRequired, map[string]string{
					"error": "free trial credits exhausted — upgrade to pro to resume sandboxes",
				})
			}
		}
	}

	// Server mode: pick any worker, dispatch via gRPC
	if s.workerRegistry != nil {
		return s.wakeSandboxRemote(c, id, req)
	}

	// Combined mode: wake locally
	if s.manager == nil || s.checkpointStore == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "hibernation not available",
		})
	}

	// Get checkpoint key from PG
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "database not configured",
		})
	}

	hibernation, err := s.store.GetActiveHibernation(ctx, id)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "no active hibernation found"})
	}

	sb, err := s.manager.Wake(ctx, id, hibernation.HibernationKey, s.checkpointStore, req.Timeout)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": err.Error(),
		})
	}

	// Register with sandbox router after explicit wake.
	// timeout == 0 means "persistent" (no auto-hibernate).
	if s.router != nil {
		timeout := req.Timeout
		if timeout < 0 {
			timeout = 0
		}
		s.router.Register(id, time.Duration(timeout)*time.Second)
	}

	_ = s.store.MarkHibernationRestored(ctx, id)
	_ = s.store.UpdateSandboxSessionForWake(ctx, id, s.workerID)

	// Apply pending checkpoint patches in background
	go s.applyPendingPatches(id, s.workerID)

	// Issue fresh JWT
	orgID, _ := auth.GetOrgID(c)
	if s.jwtIssuer != nil {
		token, err := s.jwtIssuer.IssueSandboxToken(orgID, id, s.workerID, 24*time.Hour)
		if err == nil {
			sb.Token = token
		}
	}

	sb.ConnectURL = s.httpAddr

	return c.JSON(http.StatusOK, sb)
}

func (s *Server) wakeSandboxRemote(c echo.Context, sandboxID string, req types.WakeRequest) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "database not configured",
		})
	}

	hibernation, err := s.store.GetActiveHibernation(c.Request().Context(), sandboxID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "no active hibernation found"})
	}

	session, err := s.store.GetSandboxSession(c.Request().Context(), sandboxID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "sandbox session not found"})
	}
	if session.Status != "hibernated" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "sandbox is not hibernated"})
	}

	// Pick ANY worker in the same region
	region := hibernation.Region
	worker, grpcClient, err := s.workerRegistry.GetLeastLoadedWorker(region)
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "no workers available in region " + region,
		})
	}

	grpcCtx, cancel := context.WithTimeout(c.Request().Context(), 60*time.Second)
	defer cancel()

	grpcResp, err := grpcClient.WakeSandbox(grpcCtx, &pb.WakeSandboxRequest{
		SandboxId:     sandboxID,
		CheckpointKey: hibernation.HibernationKey,
		Timeout:       int32(req.Timeout),
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "wake failed: " + err.Error(),
		})
	}

	// Mark hibernation as restored, update session
	_ = s.store.MarkHibernationRestored(c.Request().Context(), sandboxID)
	_ = s.store.UpdateSandboxSessionForWake(c.Request().Context(), sandboxID, worker.ID)
	if worker.GoldenVersion != "" {
		_ = s.store.SetSandboxGoldenVersion(c.Request().Context(), sandboxID, worker.GoldenVersion)
	}

	// Refresh the proxy route cache with the new worker — wake may have moved
	// the sandbox to a different worker than where it was hibernated, and any
	// stale cache entry would route data-plane requests to the wrong worker.
	if s.sandboxAPIProxy != nil {
		s.sandboxAPIProxy.InvalidateRouteCache(sandboxID)
	}

	// Apply pending checkpoint patches in background
	go s.applyPendingPatches(sandboxID, worker.ID)

	// Issue fresh JWT
	orgID, _ := auth.GetOrgID(c)
	var token string
	if s.jwtIssuer != nil {
		t, err := s.jwtIssuer.IssueSandboxToken(orgID, sandboxID, worker.ID, 24*time.Hour)
		if err == nil {
			token = t
		}
	}

	resp := map[string]interface{}{
		"sandboxID": sandboxID,
		"token":     token,
		"status":    grpcResp.Status,
		"region":    region,
		"workerID":  worker.ID,
	}
	if s.sandboxDomain != "" {
		resp["sandboxDomain"] = s.sandboxDomain
	}

	return c.JSON(http.StatusOK, resp)
}

// rebootSandbox triggers a soft, in-place guest restart on a running
// sandbox. The QEMU process, network mapping, and persistent disks all
// stay; only the guest CPU is reset and the kernel reboots. Recovers from
// in-guest wedges (zombie pile-up, OOM-killed agent, runaway processes).
//
// POST /api/sandboxes/:id/reboot
func (s *Server) rebootSandbox(c echo.Context) error {
	id := c.Param("id")

	if s.workerRegistry != nil {
		return s.rebootSandboxRemote(c, id)
	}

	if s.manager == nil {
		return c.JSON(http.StatusServiceUnavailable, errSandboxNotAvailable)
	}

	if err := s.manager.RebootSandbox(c.Request().Context(), id); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Server) rebootSandboxRemote(c echo.Context, sandboxID string) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}
	session, err := s.store.GetSandboxSession(c.Request().Context(), sandboxID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "sandbox not found"})
	}
	client, err := s.workerRegistry.GetWorkerClient(session.WorkerID)
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "worker unreachable: " + err.Error()})
	}
	grpcCtx, cancel := context.WithTimeout(c.Request().Context(), 90*time.Second)
	defer cancel()
	if _, err := client.RebootSandbox(grpcCtx, &pb.RebootSandboxRequest{SandboxId: sandboxID}); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "reboot failed: " + err.Error()})
	}
	s.emitEvent("reboot", sandboxID, session.WorkerID, "rebooted")
	return c.NoContent(http.StatusNoContent)
}

// powerCycleSandbox tears down the QEMU process and cold-boots a fresh VM
// with the existing on-disk drives. Use when the VMM itself is wedged or
// a soft reboot didn't recover. Sandbox keeps its ID, project, secrets,
// and persistent data; gets a new TAP and host port.
//
// POST /api/sandboxes/:id/power-cycle
func (s *Server) powerCycleSandbox(c echo.Context) error {
	id := c.Param("id")

	if s.workerRegistry != nil {
		return s.powerCycleSandboxRemote(c, id)
	}

	if s.manager == nil {
		return c.JSON(http.StatusServiceUnavailable, errSandboxNotAvailable)
	}

	if _, err := s.manager.PowerCycleSandbox(c.Request().Context(), id); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Server) powerCycleSandboxRemote(c echo.Context, sandboxID string) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}
	session, err := s.store.GetSandboxSession(c.Request().Context(), sandboxID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "sandbox not found"})
	}
	client, err := s.workerRegistry.GetWorkerClient(session.WorkerID)
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "worker unreachable: " + err.Error()})
	}
	grpcCtx, cancel := context.WithTimeout(c.Request().Context(), 120*time.Second)
	defer cancel()
	if _, err := client.PowerCycleSandbox(grpcCtx, &pb.PowerCycleSandboxRequest{SandboxId: sandboxID}); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "power-cycle failed: " + err.Error()})
	}

	// Worker re-allocated the TAP/host port. The CP→worker mapping is
	// unchanged so the sandbox API proxy doesn't need refreshing, but the
	// proxy's per-sandbox cache might hold a stale workerURL/JWT — drop it
	// so the next request re-resolves cleanly.
	if s.sandboxAPIProxy != nil {
		s.sandboxAPIProxy.InvalidateRouteCache(sandboxID)
	}

	s.emitEvent("power-cycle", sandboxID, session.WorkerID, "power-cycled")
	return c.NoContent(http.StatusNoContent)
}

// --- Checkpoint handlers ---

// createCheckpoint creates a named checkpoint of a running sandbox.
func (s *Server) createCheckpoint(c echo.Context) error {
	sandboxID := c.Param("id")
	ctx := c.Request().Context()

	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "database not configured",
		})
	}

	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{
			"error": "org context required",
		})
	}

	// Verify sandbox is running and belongs to org
	session, err := s.store.GetSandboxSession(ctx, sandboxID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "sandbox not found"})
	}
	if session.OrgID != orgID {
		return c.JSON(http.StatusForbidden, map[string]string{"error": "sandbox does not belong to this organization"})
	}
	if session.Status != "running" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "sandbox must be running to create a checkpoint"})
	}

	// Parse request body
	var req struct {
		Name string `json:"name"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body"})
	}
	if req.Name == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "name is required"})
	}

	// Enforce max 10 checkpoints per sandbox
	count, err := s.store.CountCheckpoints(ctx, sandboxID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to count checkpoints"})
	}
	if count >= 10 {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "maximum 10 checkpoints per sandbox"})
	}

	// Reserve a checkpoint UUID
	checkpointID := uuid.New()

	// Create DB record (status='processing')
	cp := &db.Checkpoint{
		ID:            checkpointID,
		SandboxID:     sandboxID,
		OrgID:         orgID,
		Name:          req.Name,
		Status:        "processing",
		SandboxConfig: session.Config,
	}
	if err := s.store.CreateCheckpoint(ctx, cp); err != nil {
		if strings.Contains(err.Error(), "duplicate key") || strings.Contains(err.Error(), "unique constraint") {
			return c.JSON(http.StatusConflict, map[string]string{"error": "checkpoint name already exists for this sandbox"})
		}
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to create checkpoint: " + err.Error()})
	}

	// Dispatch checkpoint creation in background — return immediately with status "processing".
	// The heavy work (SyncFS + pause + memory snapshot + drive copy + resume + S3 upload)
	// runs async. The sandbox is registered as pending so exec requests block until the VM
	// resumes, rather than failing with 500.
	pending := &pendingCreate{ready: make(chan struct{})}
	s.pendingCreates.Store(sandboxID, pending)

	if s.workerRegistry != nil {
		grpcClient, err := s.workerRegistry.GetWorkerClient(session.WorkerID)
		if err != nil {
			s.pendingCreates.Delete(sandboxID)
			return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "worker not available: " + err.Error()})
		}

		go func() {
			grpcCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()

			grpcResp, err := grpcClient.CreateCheckpoint(grpcCtx, &pb.CreateCheckpointRequest{
				SandboxId:    sandboxID,
				CheckpointId: checkpointID.String(),
			})

			// Signal that the sandbox is usable again (VM resumed, agent reconnected).
			// This unblocks any exec/file requests that arrived during the checkpoint pause.
			pending.err = err
			close(pending.ready)

			if err != nil {
				log.Printf("api: async checkpoint %s failed: %v", checkpointID, err)
				_ = s.store.SetCheckpointFailed(context.Background(), checkpointID, err.Error())
				return
			}
			// Mark ready immediately — S3 upload continues async inside CreateCheckpoint
			// but the checkpoint is locally usable now.
			_ = s.store.SetCheckpointReady(context.Background(), checkpointID, grpcResp.RootfsS3Key, grpcResp.WorkspaceS3Key, 0)
			log.Printf("api: checkpoint %s ready", checkpointID)
		}()
	} else if s.manager != nil {
		go func() {
			bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()

			rootfsKey, workspaceKey, err := s.manager.CreateCheckpoint(bgCtx, sandboxID, checkpointID.String(), s.checkpointStore, func() {})

			// Signal sandbox usable
			pending.err = err
			close(pending.ready)

			if err != nil {
				log.Printf("api: async checkpoint %s failed: %v", checkpointID, err)
				_ = s.store.SetCheckpointFailed(context.Background(), checkpointID, err.Error())
				return
			}
			_ = s.store.SetCheckpointReady(context.Background(), checkpointID, rootfsKey, workspaceKey, 0)
			log.Printf("api: checkpoint %s ready", checkpointID)
		}()
	} else {
		s.pendingCreates.Delete(sandboxID)
		return c.JSON(http.StatusServiceUnavailable, errSandboxNotAvailable)
	}

	return c.JSON(http.StatusCreated, cp)
}

// listCheckpoints returns all checkpoints for a sandbox.
func (s *Server) listCheckpoints(c echo.Context) error {
	sandboxID := c.Param("id")

	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}

	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "org context required"})
	}

	// Verify sandbox belongs to org
	session, err := s.store.GetSandboxSession(c.Request().Context(), sandboxID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "sandbox not found"})
	}
	if session.OrgID != orgID {
		return c.JSON(http.StatusForbidden, map[string]string{"error": "sandbox does not belong to this organization"})
	}

	checkpoints, err := s.store.ListCheckpoints(c.Request().Context(), sandboxID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	return c.JSON(http.StatusOK, checkpoints)
}

// restoreCheckpoint restores a sandbox to a checkpoint (in-place revert).
func (s *Server) restoreCheckpoint(c echo.Context) error {
	sandboxID := c.Param("id")
	checkpointIDStr := c.Param("checkpointId")
	ctx := c.Request().Context()

	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}

	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "org context required"})
	}

	checkpointID, err := uuid.Parse(checkpointIDStr)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid checkpoint ID"})
	}

	// Verify sandbox belongs to org and is running
	session, err := s.store.GetSandboxSession(ctx, sandboxID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "sandbox not found"})
	}
	if session.OrgID != orgID {
		return c.JSON(http.StatusForbidden, map[string]string{"error": "sandbox does not belong to this organization"})
	}
	if session.Status != "running" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "sandbox must be running to restore a checkpoint"})
	}

	// Verify checkpoint exists, belongs to this sandbox, and is ready
	cp, err := s.store.GetCheckpoint(ctx, checkpointID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "checkpoint not found"})
	}
	if cp.SandboxID != sandboxID {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "checkpoint does not belong to this sandbox"})
	}
	if cp.Status != "ready" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "checkpoint is not ready (status: " + cp.Status + ")"})
	}

	// Dispatch restore in background — return immediately.
	// Commands will block until restore completes (via pendingCreates + waitForReady).
	pending := &pendingCreate{ready: make(chan struct{})}
	s.pendingCreates.Store(sandboxID, pending)

	// Invalidate the proxy route cache so subsequent requests go through
	// the full ProxyHandler path and hit waitForReady. Without this, cached
	// routes bypass the readiness check and hit the mid-restore VM.
	if s.sandboxAPIProxy != nil {
		s.sandboxAPIProxy.InvalidateRouteCache(sandboxID)
	}

	if s.workerRegistry != nil {
		grpcClient, err := s.workerRegistry.GetWorkerClient(session.WorkerID)
		if err != nil {
			s.pendingCreates.Delete(sandboxID)
			return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "worker not available: " + err.Error()})
		}

		go func() {
			grpcCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			_, restoreErr := grpcClient.RestoreCheckpoint(grpcCtx, &pb.RestoreCheckpointRequest{
				SandboxId:    sandboxID,
				CheckpointId: checkpointID.String(),
			})
			if restoreErr != nil {
				log.Printf("api: async restore %s/%s failed: %v", sandboxID, checkpointID, restoreErr)
			}
			pending.err = restoreErr
			close(pending.ready)
		}()
	} else if s.manager != nil {
		go func() {
			restoreErr := s.manager.RestoreFromCheckpoint(context.Background(), sandboxID, checkpointID.String())
			if restoreErr != nil {
				log.Printf("api: async restore %s/%s failed: %v", sandboxID, checkpointID, restoreErr)
			}
			pending.err = restoreErr
			close(pending.ready)
		}()
	} else {
		s.pendingCreates.Delete(sandboxID)
		return c.JSON(http.StatusServiceUnavailable, errSandboxNotAvailable)
	}

	return c.JSON(http.StatusOK, map[string]string{
		"sandboxId":    sandboxID,
		"checkpointId": checkpointID.String(),
		"status":       "restoring",
	})
}

// createFromCheckpoint creates a new sandbox from an existing checkpoint (fork).
func (s *Server) createFromCheckpoint(c echo.Context) error {
	// Direct route (POST /sandboxes/from-checkpoint/:checkpointId): the
	// envs / secretStore / metadata fields are supported on the fork path;
	// everything else is inherited from the checkpoint's stored config.
	// Metadata is merged onto the checkpoint's persisted metadata
	// (caller wins) so callers can stamp identifiers like agent_id without
	// losing whatever the snapshot author originally set.
	var body struct {
		Envs        map[string]string `json:"envs"`
		SecretStore string            `json:"secretStore"`
		Metadata    map[string]string `json:"metadata"`
	}
	_ = c.Bind(&body)
	result, httpStatus, err := s.createFromCheckpointCore(c, body.Envs, body.SecretStore, body.Metadata)
	if err != nil {
		return c.JSON(httpStatus, map[string]string{"error": err.Error()})
	}
	return c.JSON(httpStatus, result)
}

// createFromCheckpointCore contains the core logic for creating a sandbox from a checkpoint.
// Returns the result map, HTTP status, or an error.
//
// Secret store handling:
//   - If the checkpoint was created WITH a secret store, the store NAME is persisted
//     in cp.SandboxConfig and the fork re-resolves it fresh against the DB. If the
//     user updated a secret between snapshot and fork, the fork sees the new value.
//     The user cannot override the inherited store (prevents accidental leaks).
//   - If the checkpoint was created WITHOUT a secret store, the user can attach one
//     at fork time via userSecretStore. This is safe because there are no pre-existing
//     sealed tokens or proxy sessions to conflict with.
//
// userEnvs (may be nil) overrides the envs from the checkpoint's stored config.
// User keys win over keys re-resolved from the secret store.
//
// userMetadata (may be nil) is merged into the checkpoint's persisted
// metadata before the sandbox session row is recorded — so callers can
// stamp request-time identifiers (e.g. agent_id) without losing whatever
// the snapshot author baked in.
func (s *Server) createFromCheckpointCore(c echo.Context, userEnvs map[string]string, userSecretStore string, userMetadata map[string]string) (map[string]interface{}, int, error) {
	checkpointIDStr := c.Param("checkpointId")
	ctx := c.Request().Context()

	if s.store == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("database not configured")
	}

	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return nil, http.StatusUnauthorized, fmt.Errorf("org context required")
	}

	checkpointID, err := uuid.Parse(checkpointIDStr)
	if err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("invalid checkpoint ID")
	}

	// Verify checkpoint exists, is accessible, and is ready.
	// Fork is the only checkpoint op relaxed for public checkpoints — patch,
	// list-patches, delete-patch, and delete-checkpoint remain owner-only.
	// See ws-gstack design 009 (publishable checkpoints).
	cp, err := s.store.GetCheckpoint(ctx, checkpointID)
	if err != nil {
		return nil, http.StatusNotFound, fmt.Errorf("checkpoint not found")
	}
	if cp.OrgID != orgID && !cp.IsPublic {
		return nil, http.StatusForbidden, fmt.Errorf("checkpoint does not belong to this organization")
	}
	// Poll for checkpoint readiness — checkpoints transition from "processing" to "ready"
	// asynchronously. Wait up to 30s so SDK/CLI users don't have to poll manually.
	if cp.Status != "ready" {
		if cp.Status == "failed" {
			return nil, http.StatusBadRequest, fmt.Errorf("checkpoint failed")
		}
		for i := 0; i < 30; i++ {
			time.Sleep(1 * time.Second)
			cp, err = s.store.GetCheckpoint(ctx, checkpointID)
			if err != nil {
				return nil, http.StatusNotFound, fmt.Errorf("checkpoint not found")
			}
			if cp.Status == "ready" {
				break
			}
			if cp.Status == "failed" {
				return nil, http.StatusBadRequest, fmt.Errorf("checkpoint failed")
			}
		}
		if cp.Status != "ready" {
			return nil, http.StatusBadRequest, fmt.Errorf("checkpoint is not ready after 30s (status: %s)", cp.Status)
		}
	}

	// Parse optional overrides from request body
	var req struct {
		Timeout int `json:"timeout"`
	}
	_ = c.Bind(&req)

	// Get S3 keys from the checkpoint
	if cp.RootfsS3Key == nil || cp.WorkspaceS3Key == nil {
		return nil, http.StatusBadRequest, fmt.Errorf("checkpoint S3 keys not available")
	}

	// Parse the original sandbox config to reuse settings
	var originalCfg types.SandboxConfig
	_ = json.Unmarshal(cp.SandboxConfig, &originalCfg)

	// Secret store resolution — supports layering:
	// Resolve stores in order: BaseSecretStore → SecretStore → user's store.
	// Each layer's secrets merge on top (later wins on collision).
	// Egress allowlists aggregate (union of all layers).
	// We collect all unique store names, resolve them in order, then persist
	// the last two as SecretStore (child) and BaseSecretStore (parent).
	var stores []string
	if originalCfg.BaseSecretStore != "" {
		stores = append(stores, originalCfg.BaseSecretStore)
	}
	if originalCfg.SecretStore != "" {
		stores = append(stores, originalCfg.SecretStore)
	}
	if userSecretStore != "" {
		stores = append(stores, userSecretStore)
	}
	// Deduplicate preserving order
	seen := make(map[string]bool)
	var uniqueStores []string
	for _, name := range stores {
		if !seen[name] {
			seen[name] = true
			uniqueStores = append(uniqueStores, name)
		}
	}
	if len(uniqueStores) > 0 {
		var allEgress []string
		for _, storeName := range uniqueStores {
			originalCfg.SecretStore = storeName
			if err := s.resolveSecretStoreInto(ctx, orgID, &originalCfg); err != nil {
				return nil, http.StatusBadRequest, err
			}
			allEgress = append(allEgress, originalCfg.EgressAllowlist...)
		}
		// Deduplicate egress
		egressSeen := make(map[string]bool)
		var merged []string
		for _, h := range allEgress {
			if !egressSeen[h] {
				egressSeen[h] = true
				merged = append(merged, h)
			}
		}
		originalCfg.EgressAllowlist = merged
		// Persist: last store = SecretStore, second-to-last = BaseSecretStore
		originalCfg.SecretStore = uniqueStores[len(uniqueStores)-1]
		originalCfg.BaseSecretStore = ""
		if len(uniqueStores) > 1 {
			originalCfg.BaseSecretStore = uniqueStores[len(uniqueStores)-2]
		}
	}

	// Merge user-supplied envs over the checkpoint's envs. User keys win.
	// Without this merge, Sandbox.create({snapshot, envs}) silently dropped
	// user envs because only originalCfg flowed down to the worker.
	if len(userEnvs) > 0 {
		if originalCfg.Envs == nil {
			originalCfg.Envs = make(map[string]string, len(userEnvs))
		}
		for k, v := range userEnvs {
			originalCfg.Envs[k] = v
		}
	}

	// Resolve timeout. With int-valued JSON fields we can't distinguish "not sent"
	// from "explicitly zero", so treat req.Timeout as authoritative. Fall back to
	// the checkpoint's original timeout only when req.Timeout is negative (which is
	// itself invalid, but could occur historically). timeout == 0 is valid and means
	// "persistent / never auto-hibernate".
	timeout := req.Timeout
	if timeout < 0 {
		timeout = originalCfg.Timeout
	}
	if timeout < 0 {
		timeout = 0
	}

	// Unified async fork: return immediately, boot VM in background.
	// First command from SDK will block until VM is ready.

	// Pre-generate sandbox ID
	sandboxID := "sb-" + uuid.New().String()[:8]

	// Determine execution target
	region := s.region
	if region == "" {
		region = "iad"
	}
	var workerID string
	var grpcClient pb.SandboxWorkerClient

	if s.workerRegistry != nil {
		// Server mode: pick a worker
		worker, client, wErr := s.workerRegistry.GetLeastLoadedWorker(region)
		if wErr != nil {
			return nil, http.StatusServiceUnavailable, fmt.Errorf("no workers available: %w", wErr)
		}
		workerID = worker.ID
		grpcClient = client
	} else if s.manager != nil {
		// Combined mode: local execution
		workerID = s.workerID
	} else {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("sandbox execution not available in server-only mode")
	}

	// Register pending create — commands will wait until ready
	pending := &pendingCreate{ready: make(chan struct{})}
	s.pendingCreates.Store(sandboxID, pending)

	// Also register with sandbox router if available (combined mode)
	if s.router != nil {
		s.router.RegisterCreating(sandboxID, time.Duration(timeout)*time.Second)
	}

	// Issue JWT immediately
	var token string
	if s.jwtIssuer != nil {
		t, jwtErr := s.jwtIssuer.IssueSandboxToken(orgID, sandboxID, workerID, 24*time.Hour)
		if jwtErr == nil {
			token = t
		}
	}

	// Boot VM synchronously so the worker's in-memory sandbox map is populated
	// before we respond or record the session. Previously this ran in a goroutine
	// with the session row written first as status=running, which allowed
	// immediate hibernate/restore/etc. to route to a worker whose m.vms[id] was
	// not yet populated and 500 with "sandbox not found".
	var createErr error
	if grpcClient != nil {
		// Use background context — the fork has its own internal timeouts
		// (30s agent connect, 10s QMP, 5s network patch). An external deadline
		// here causes orphaned VMs: the gRPC layer returns DeadlineExceeded
		// while the worker finishes creating the VM, leaving it untracked.
		_, createErr = grpcClient.CreateSandbox(context.Background(), &pb.CreateSandboxRequest{
			Template:             originalCfg.Template,
			Timeout:              int32(timeout),
			Envs:                 originalCfg.Envs,
			MemoryMb:             int32(originalCfg.MemoryMB),
			CpuCount:             int32(originalCfg.CpuCount),
			NetworkEnabled:       originalCfg.NetworkEnabled,
			Port:                 int32(originalCfg.Port),
			TemplateRootfsKey:    *cp.RootfsS3Key,
			TemplateWorkspaceKey: *cp.WorkspaceS3Key,
			CheckpointId:         checkpointID.String(),
			SandboxId:            sandboxID,
			EgressAllowlist:      originalCfg.EgressAllowlist,
			SecretAllowedHosts:   flattenSecretAllowedHosts(originalCfg.SecretAllowedHosts),
			SecretEnvs:           originalCfg.SecretEnvs,
		})
	} else {
		// Combined mode: create locally — no external timeout, same reasoning as above
		cfg := originalCfg
		cfg.Timeout = timeout
		cfg.TemplateRootfsKey = *cp.RootfsS3Key
		cfg.TemplateWorkspaceKey = *cp.WorkspaceS3Key
		cfg.SandboxID = sandboxID
		cfg.CheckpointID = checkpointID.String()

		forkMgr, hasFork := s.manager.(interface {
			ForkFromCheckpoint(ctx context.Context, checkpointID string, cfg types.SandboxConfig) (*types.Sandbox, error)
		})
		if hasFork {
			_, createErr = forkMgr.ForkFromCheckpoint(context.Background(), checkpointID.String(), cfg)
		} else {
			_, createErr = s.manager.Create(context.Background(), cfg)
		}
	}

	// Unblock any callers waiting on this pendingCreate entry with the outcome.
	pending.err = createErr
	close(pending.ready)
	if s.router != nil {
		s.router.MarkCreated(sandboxID, createErr)
	}

	if createErr != nil {
		s.pendingCreates.Delete(sandboxID)
		log.Printf("api: fork %s failed: %v", sandboxID, createErr)
		return nil, http.StatusInternalServerError, fmt.Errorf("fork from checkpoint: %w", createErr)
	}

	// Record session only after the worker has the VM registered so the session
	// row never points at a worker that does not yet own the VM.
	if s.store != nil {
		template := originalCfg.Template
		if template == "" {
			template = "default"
		}
		cfgJSON, _ := json.Marshal(cfgForPersistence(originalCfg))
		// Merge user-supplied metadata onto the snapshot's metadata (user wins)
		// so request-time labels (e.g. agent_id) end up on the session row,
		// where the billing path can find them via metadata->>'agent_id'.
		mergedMeta := map[string]string{}
		for k, v := range originalCfg.Metadata {
			mergedMeta[k] = v
		}
		for k, v := range userMetadata {
			mergedMeta[k] = v
		}
		metadataJSON, _ := json.Marshal(mergedMeta)
		_, _ = s.store.CreateSandboxSession(ctx, sandboxID, orgID, auth.GetUserID(c), template, region, workerID, cfgJSON, metadataJSON)
		// Set golden version from worker heartbeat
		if s.workerRegistry != nil {
			if w := s.workerRegistry.GetWorker(workerID); w != nil && w.GoldenVersion != "" {
				_ = s.store.SetSandboxGoldenVersion(ctx, sandboxID, w.GoldenVersion)
			}
		}
		// Track checkpoint lineage for patch system
		_ = s.store.SetSandboxCheckpointID(ctx, sandboxID, checkpointID)
	}

	s.applyPendingPatches(sandboxID, workerID)

	result := map[string]interface{}{
		"sandboxID":        sandboxID,
		"status":           "running",
		"token":            token,
		"region":           region,
		"workerID":         workerID,
		"fromCheckpointId": checkpointID.String(),
	}
	if s.sandboxDomain != "" {
		result["sandboxDomain"] = s.sandboxDomain
	}
	return result, http.StatusCreated, nil
}

// deleteCheckpoint deletes a checkpoint.
func (s *Server) deleteCheckpoint(c echo.Context) error {
	sandboxID := c.Param("id")
	checkpointIDStr := c.Param("checkpointId")
	ctx := c.Request().Context()

	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}

	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "org context required"})
	}

	checkpointID, err := uuid.Parse(checkpointIDStr)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid checkpoint ID"})
	}

	// Verify checkpoint exists and belongs to this sandbox and org
	cp, err := s.store.GetCheckpoint(ctx, checkpointID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "checkpoint not found"})
	}
	if cp.SandboxID != sandboxID {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "checkpoint does not belong to this sandbox"})
	}

	// Delete from DB (enforces org ownership)
	if err := s.store.DeleteCheckpoint(ctx, orgID, checkpointID); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	// Best-effort: delete S3 objects if checkpoint store is configured
	if s.checkpointStore != nil && cp.RootfsS3Key != nil && cp.WorkspaceS3Key != nil {
		go func() {
			bgCtx := context.Background()
			if err := s.checkpointStore.Delete(bgCtx, *cp.RootfsS3Key); err != nil {
				log.Printf("checkpoint: failed to delete S3 rootfs %s: %v", *cp.RootfsS3Key, err)
			}
			if err := s.checkpointStore.Delete(bgCtx, *cp.WorkspaceS3Key); err != nil {
				log.Printf("checkpoint: failed to delete S3 workspace %s: %v", *cp.WorkspaceS3Key, err)
			}
		}()
	}

	return c.NoContent(http.StatusNoContent)
}

// --- Checkpoint Patch handlers ---

// createCheckpointPatch creates a patch for a checkpoint and fans out to running sandboxes.
func (s *Server) createCheckpointPatch(c echo.Context) error {
	checkpointIDStr := c.Param("checkpointId")
	ctx := c.Request().Context()

	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}

	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "org context required"})
	}

	checkpointID, err := uuid.Parse(checkpointIDStr)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid checkpoint ID"})
	}

	// Verify checkpoint exists and belongs to org
	cp, err := s.store.GetCheckpoint(ctx, checkpointID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "checkpoint not found"})
	}
	if cp.OrgID != orgID {
		return c.JSON(http.StatusForbidden, map[string]string{"error": "checkpoint does not belong to this organization"})
	}
	if cp.Status != "ready" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "checkpoint is not ready"})
	}

	var req struct {
		Script      string `json:"script"`
		Description string `json:"description"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body"})
	}
	if req.Script == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "script is required"})
	}
	// Create the patch record (patches apply on next wake/boot)
	patch := &db.CheckpointPatch{
		ID:           uuid.New(),
		CheckpointID: checkpointID,
		Script:       req.Script,
		Description:  req.Description,
		Strategy:     "on_wake",
	}
	if err := s.store.CreateCheckpointPatch(ctx, patch); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to create patch: " + err.Error()})
	}

	return c.JSON(http.StatusCreated, map[string]interface{}{
		"patch": patch,
	})
}

// execPatchOnSandbox runs a patch script on a running sandbox via gRPC exec.
func (s *Server) execPatchOnSandbox(ctx context.Context, sandboxID, workerID string, patch *db.CheckpointPatch) error {
	execCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	if s.workerRegistry != nil {
		client, err := s.workerRegistry.GetWorkerClient(workerID)
		if err != nil {
			return fmt.Errorf("worker %s unreachable: %w", workerID, err)
		}
		resp, err := client.ExecCommand(execCtx, &pb.ExecCommandRequest{
			SandboxId: sandboxID,
			Command:   "bash",
			Args:      []string{"-c", patch.Script},
			Timeout:   300,
		})
		if err != nil {
			return fmt.Errorf("exec failed: %w", err)
		}
		if resp.ExitCode != 0 {
			return fmt.Errorf("patch exited with code %d: %s", resp.ExitCode, resp.Stderr)
		}
		return nil
	}

	// Combined mode: exec locally
	if s.manager != nil {
		result, err := s.manager.Exec(ctx, sandboxID, types.ProcessConfig{
			Command: "bash",
			Args:    []string{"-c", patch.Script},
			Timeout: 300,
		})
		if err != nil {
			return fmt.Errorf("exec failed: %w", err)
		}
		if result.ExitCode != 0 {
			return fmt.Errorf("patch exited with code %d: %s", result.ExitCode, result.Stderr)
		}
		return nil
	}

	return fmt.Errorf("no execution backend available")
}

// applyPendingPatches checks for and applies any pending patches after a sandbox wakes.
func (s *Server) applyPendingPatches(sandboxID, workerID string) {
	if s.store == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	session, err := s.store.GetSandboxSession(ctx, sandboxID)
	if err != nil {
		log.Printf("patches: %s: failed to get session: %v", sandboxID, err)
		return
	}
	if session.BasedOnCheckpointID == nil {
		return // Not based on a checkpoint, nothing to patch
	}

	patches, err := s.store.GetPendingPatches(ctx, *session.BasedOnCheckpointID, session.LastPatchSequence)
	if err != nil {
		log.Printf("patches: %s: failed to get pending patches: %v", sandboxID, err)
		return
	}
	if len(patches) == 0 {
		return
	}

	log.Printf("patches: %s: applying %d pending patches (from seq %d)", sandboxID, len(patches), session.LastPatchSequence+1)

	// Clear any previous patch error before applying
	_ = s.store.SetSandboxPatchError(ctx, sandboxID, nil)

	for _, patch := range patches {
		if err := s.execPatchOnSandbox(ctx, sandboxID, workerID, &patch); err != nil {
			errMsg := fmt.Sprintf("patch seq %d failed: %v — delete the bad patch and retry (DELETE /snapshots/:name/patches/:id or DELETE /sandboxes/checkpoints/:id/patches/:id)", patch.Sequence, err)
			log.Printf("patches: %s: %s", sandboxID, errMsg)
			if dbErr := s.store.SetSandboxPatchError(ctx, sandboxID, &errMsg); dbErr != nil {
				log.Printf("patches: %s: failed to save patch error to DB: %v", sandboxID, dbErr)
			}
			return // Stop on first failure
		}
		_ = s.store.UpdateSandboxPatchSequence(ctx, sandboxID, patch.Sequence)
		log.Printf("patches: %s: patch seq %d applied successfully", sandboxID, patch.Sequence)
	}

	// All patches applied — clear any stale error
	_ = s.store.SetSandboxPatchError(ctx, sandboxID, nil)
}

// listCheckpointPatches returns all patches for a checkpoint.
func (s *Server) listCheckpointPatches(c echo.Context) error {
	checkpointIDStr := c.Param("checkpointId")

	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}

	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "org context required"})
	}

	checkpointID, err := uuid.Parse(checkpointIDStr)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid checkpoint ID"})
	}

	// Verify checkpoint belongs to org
	cp, err := s.store.GetCheckpoint(c.Request().Context(), checkpointID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "checkpoint not found"})
	}
	if cp.OrgID != orgID {
		return c.JSON(http.StatusForbidden, map[string]string{"error": "checkpoint does not belong to this organization"})
	}

	patches, err := s.store.ListCheckpointPatches(c.Request().Context(), checkpointID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	if patches == nil {
		patches = []db.CheckpointPatch{}
	}

	return c.JSON(http.StatusOK, patches)
}

// deleteCheckpointPatch removes a patch from a checkpoint.
func (s *Server) deleteCheckpointPatch(c echo.Context) error {
	checkpointIDStr := c.Param("checkpointId")
	patchIDStr := c.Param("patchId")
	ctx := c.Request().Context()

	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}

	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "org context required"})
	}

	checkpointID, err := uuid.Parse(checkpointIDStr)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid checkpoint ID"})
	}

	patchID, err := uuid.Parse(patchIDStr)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid patch ID"})
	}

	// Verify checkpoint belongs to org
	cp, err := s.store.GetCheckpoint(ctx, checkpointID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "checkpoint not found"})
	}
	if cp.OrgID != orgID {
		return c.JSON(http.StatusForbidden, map[string]string{"error": "checkpoint does not belong to this organization"})
	}

	if err := s.store.DeleteCheckpointPatch(ctx, checkpointID, patchID); err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "patch not found"})
	}

	return c.NoContent(http.StatusNoContent)
}

// publishCheckpoint marks a checkpoint as publicly forkable. Owner-org only.
// Idempotent — publishing an already-public checkpoint is a no-op 200.
// See ws-gstack design 009.
func (s *Server) publishCheckpoint(c echo.Context) error {
	return s.setCheckpointPublic(c, true)
}

// unpublishCheckpoint flips is_public back to false. Owner-org only, idempotent.
// In-flight forks that already passed the auth check continue; new forks 403.
func (s *Server) unpublishCheckpoint(c echo.Context) error {
	return s.setCheckpointPublic(c, false)
}

func (s *Server) setCheckpointPublic(c echo.Context, isPublic bool) error {
	checkpointIDStr := c.Param("checkpointId")
	ctx := c.Request().Context()

	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}

	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "org context required"})
	}

	checkpointID, err := uuid.Parse(checkpointIDStr)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid checkpoint ID"})
	}

	cp, err := s.store.GetCheckpoint(ctx, checkpointID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "checkpoint not found"})
	}
	if cp.OrgID != orgID {
		return c.JSON(http.StatusForbidden, map[string]string{"error": "checkpoint does not belong to this organization"})
	}

	if err := s.store.SetCheckpointPublic(ctx, checkpointID, orgID, isPublic); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	cp.IsPublic = isPublic
	return c.JSON(http.StatusOK, cp)
}

// listSessions returns session history from PostgreSQL.
func (s *Server) listWorkers(c echo.Context) error {
	if s.workerRegistry == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "worker registry not available (server mode only)",
		})
	}
	return c.JSON(http.StatusOK, s.workerRegistry.GetAllWorkers())
}

func (s *Server) listSessions(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "session history requires database configuration",
		})
	}

	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{
			"error": "org context required",
		})
	}

	status := c.QueryParam("status")
	sessions, err := s.store.ListSandboxSessions(c.Request().Context(), orgID, status, 100, 0)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": err.Error(),
		})
	}

	return c.JSON(http.StatusOK, sessions)
}

// --- Preview URL handlers ---

// createPreviewURL creates an on-demand preview URL for a running sandbox
// targeting a specific container port. Hostname format: {sandboxID}-p{port}.{baseDomain}
func (s *Server) createPreviewURL(c echo.Context) error {
	sandboxID := c.Param("id")
	ctx := c.Request().Context()

	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "database not configured",
		})
	}

	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{
			"error": "org context required",
		})
	}

	// Parse request body — port is required
	var req struct {
		Port       int             `json:"port"`
		Domain     string          `json:"domain"`
		AuthConfig json.RawMessage `json:"authConfig"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "invalid request body: " + err.Error(),
		})
	}
	if req.Port < 1 || req.Port > 65535 {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "port must be between 1 and 65535",
		})
	}
	if req.AuthConfig == nil {
		req.AuthConfig = json.RawMessage("{}")
	}

	// Verify sandbox is running
	sandboxRunning := false
	if s.manager != nil {
		if _, err := s.manager.Get(ctx, sandboxID); err == nil {
			sandboxRunning = true
		}
	}
	if !sandboxRunning && s.store != nil {
		session, err := s.store.GetSandboxSession(ctx, sandboxID)
		if err == nil && session.Status == "running" && session.OrgID == orgID {
			sandboxRunning = true
		}
	}
	if !sandboxRunning {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "sandbox is not running or not found",
		})
	}

	// Look up org for custom domain support
	org, err := s.store.GetOrg(ctx, orgID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "failed to look up org",
		})
	}
	var customDomain string
	if org.CustomDomain != nil && *org.CustomDomain != "" {
		customDomain = *org.CustomDomain
	}

	// If preview URL already exists for this port, return it
	existing, err := s.store.GetPreviewURLByPort(ctx, sandboxID, req.Port)
	if err == nil && existing != nil {
		return c.JSON(http.StatusOK, previewURLToMap(*existing, customDomain))
	}

	// Determine hostname based on whether a custom domain was requested
	var hostname string
	var cfHostnameID *string

	if req.Domain != "" {
		// Validate the requested domain matches the org's verified custom domain
		if customDomain == "" {
			return c.JSON(http.StatusBadRequest, map[string]string{
				"error": "org has no custom domain configured",
			})
		}
		if req.Domain != customDomain {
			return c.JSON(http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("domain %q does not match org custom domain %q", req.Domain, customDomain),
			})
		}
		if org.DomainVerificationStatus != "active" {
			return c.JSON(http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("custom domain %q is not verified (status: %s)", req.Domain, org.DomainVerificationStatus),
			})
		}

		hostname = fmt.Sprintf("%s-p%d.%s", sandboxID, req.Port, req.Domain)

		// Register with Cloudflare if configured
		if s.cfClient != nil {
			cfResult, err := s.cfClient.CreateCustomHostnameHTTP(hostname)
			if err != nil {
				return c.JSON(http.StatusInternalServerError, map[string]string{
					"error": "failed to register custom hostname with Cloudflare: " + err.Error(),
				})
			}
			cfHostnameID = &cfResult.ID
		}
	} else {
		// Default: use the platform sandbox domain
		hostname = fmt.Sprintf("%s-p%d.%s", sandboxID, req.Port, s.sandboxDomain)
	}

	previewURL, err := s.store.CreatePreviewURL(ctx, sandboxID, orgID, hostname, req.Port, cfHostnameID, "active", req.AuthConfig)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": err.Error(),
		})
	}

	return c.JSON(http.StatusCreated, previewURLToMap(*previewURL, customDomain))
}

// listPreviewURLs returns all preview URLs for a sandbox.
func (s *Server) listPreviewURLs(c echo.Context) error {
	sandboxID := c.Param("id")

	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "database not configured",
		})
	}

	orgID, _ := auth.GetOrgID(c)
	customDomain := s.getOrgCustomDomain(c.Request().Context(), orgID)

	urls, err := s.store.ListPreviewURLs(c.Request().Context(), sandboxID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": err.Error(),
		})
	}

	result := make([]map[string]interface{}, len(urls))
	for i, u := range urls {
		result[i] = previewURLToMap(u, customDomain)
	}

	return c.JSON(http.StatusOK, result)
}

// deletePreviewURL removes the preview URL for a sandbox on a specific port.
func (s *Server) deletePreviewURL(c echo.Context) error {
	sandboxID := c.Param("id")
	portStr := c.Param("port")
	ctx := c.Request().Context()

	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "database not configured",
		})
	}

	port := 0
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil || port < 1 || port > 65535 {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "invalid port",
		})
	}

	previewURL, err := s.store.GetPreviewURLByPort(ctx, sandboxID, port)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{
			"error": "no preview URL for this port",
		})
	}

	// Delete from Cloudflare if applicable (for legacy custom domain URLs)
	if s.cfClient != nil && previewURL.CFHostnameID != nil && *previewURL.CFHostnameID != "" {
		if err := s.cfClient.DeleteCustomHostname(*previewURL.CFHostnameID); err != nil {
			log.Printf("preview: failed to delete CF hostname %s: %v", *previewURL.CFHostnameID, err)
		}
	}

	_ = s.store.DeletePreviewURL(ctx, previewURL.ID)

	return c.NoContent(http.StatusNoContent)
}

// previewURLToMap converts a PreviewURL to a response map, including customHostname if provided.
func previewURLToMap(u db.PreviewURL, customDomain string) map[string]interface{} {
	m := map[string]interface{}{
		"id":         u.ID,
		"sandboxId":  u.SandboxID,
		"orgId":      u.OrgID,
		"hostname":   u.Hostname,
		"port":       u.Port,
		"sslStatus":  u.SSLStatus,
		"authConfig": u.AuthConfig,
		"createdAt":  u.CreatedAt,
	}
	if u.CFHostnameID != nil {
		m["cfHostnameId"] = *u.CFHostnameID
	}
	if customDomain != "" {
		if dot := strings.Index(u.Hostname, "."); dot > 0 {
			m["customHostname"] = u.Hostname[:dot+1] + customDomain
		}
	}
	return m
}

// getOrgCustomDomain returns the org's custom domain, or "" if none.
func (s *Server) getOrgCustomDomain(ctx context.Context, orgID uuid.UUID) string {
	if s.store == nil {
		return ""
	}
	org, err := s.store.GetOrg(ctx, orgID)
	if err == nil && org.CustomDomain != nil && *org.CustomDomain != "" {
		return *org.CustomDomain
	}
	return ""
}

// cleanupPreviewURLs removes all preview URLs for a sandbox on kill (best-effort).
func (s *Server) cleanupPreviewURLs(ctx context.Context, sandboxID string) {
	if s.store == nil {
		return
	}
	urls, err := s.store.DeletePreviewURLsBySandbox(ctx, sandboxID)
	if err != nil {
		return
	}
	for _, u := range urls {
		if s.cfClient != nil && u.CFHostnameID != nil && *u.CFHostnameID != "" {
			if err := s.cfClient.DeleteCustomHostname(*u.CFHostnameID); err != nil {
				log.Printf("preview: cleanup failed for CF hostname %s: %v", *u.CFHostnameID, err)
			}
		}
	}
}
