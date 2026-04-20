package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/opensandbox/opensandbox/internal/db"
	"github.com/opensandbox/opensandbox/internal/storage"
	"github.com/opensandbox/opensandbox/pkg/types"
	pb "github.com/opensandbox/opensandbox/proto/worker"
)

// ImageManifest is the parsed declarative image definition from the SDK.
type ImageManifest struct {
	Base  string      `json:"base"`
	Steps []ImageStep `json:"steps"`
	Name  string      `json:"name,omitempty"` // optional — makes image addressable as a snapshot (for patches, etc.)
}

// ImageStep is a single build step in an image manifest.
type ImageStep struct {
	Type string                 `json:"type"`
	Args map[string]interface{} `json:"args"`
}

// BuildLogFunc is a callback for streaming build log messages.
// If nil, logs are only written to the server log.
type BuildLogFunc func(step int, stepType string, message string)

// in-flight image builds — prevents duplicate builds for same hash
var inflightBuilds sync.Map // map[string]*imageBuild

type imageBuild struct {
	ready        chan struct{}
	checkpointID uuid.UUID
	err          error
}

// computeManifestHash returns a deterministic SHA-256 hash for the manifest.
// The Name field is excluded so that naming an image doesn't change its cache key.
func computeManifestHash(manifest *ImageManifest) string {
	hashInput := struct {
		Base  string      `json:"base"`
		Steps []ImageStep `json:"steps"`
	}{Base: manifest.Base, Steps: manifest.Steps}
	data, _ := json.Marshal(hashInput)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// translateStepToCommand converts an image step to a shell command.
func translateStepToCommand(step ImageStep) (string, error) {
	switch step.Type {
	case "apt_install":
		pkgs, ok := step.Args["packages"]
		if !ok {
			return "", fmt.Errorf("apt_install: missing packages")
		}
		packages, err := toStringSlice(pkgs)
		if err != nil {
			return "", fmt.Errorf("apt_install: %w", err)
		}
		return fmt.Sprintf("sudo DEBIAN_FRONTEND=noninteractive apt-get update -qq && sudo apt-get install -y -qq %s", strings.Join(packages, " ")), nil

	case "pip_install":
		pkgs, ok := step.Args["packages"]
		if !ok {
			return "", fmt.Errorf("pip_install: missing packages")
		}
		packages, err := toStringSlice(pkgs)
		if err != nil {
			return "", fmt.Errorf("pip_install: %w", err)
		}
		return fmt.Sprintf("sudo pip install -q %s", strings.Join(packages, " ")), nil

	case "run":
		cmds, ok := step.Args["commands"]
		if !ok {
			return "", fmt.Errorf("run: missing commands")
		}
		commands, err := toStringSlice(cmds)
		if err != nil {
			return "", fmt.Errorf("run: %w", err)
		}
		return strings.Join(commands, " && "), nil

	case "env":
		vars, ok := step.Args["vars"]
		if !ok {
			return "", fmt.Errorf("env: missing vars")
		}
		varsMap, ok := vars.(map[string]interface{})
		if !ok {
			return "", fmt.Errorf("env: vars must be a map")
		}
		var parts []string
		for k, v := range varsMap {
			val := fmt.Sprintf("%v", v)
			// Write to /etc/environment for persistence across sessions
			parts = append(parts, fmt.Sprintf("sudo sh -c \"echo '%s=%s' >> /etc/environment\"", k, val))
			// Also write to profile.d for interactive shells
			parts = append(parts, fmt.Sprintf("sudo sh -c \"echo 'export %s=\\\"%s\\\"' >> /etc/profile.d/osb-image.sh\"", k, val))
		}
		return strings.Join(parts, " && "), nil

	case "workdir":
		path, ok := step.Args["path"]
		if !ok {
			return "", fmt.Errorf("workdir: missing path")
		}
		pathStr, ok := path.(string)
		if !ok {
			return "", fmt.Errorf("workdir: path must be a string")
		}
		return fmt.Sprintf("sudo mkdir -p %s", pathStr), nil

	case "add_file":
		remotePath, ok := step.Args["path"]
		if !ok {
			return "", fmt.Errorf("add_file: missing path")
		}
		pathStr, ok := remotePath.(string)
		if !ok {
			return "", fmt.Errorf("add_file: path must be a string")
		}
		content, ok := step.Args["content"]
		if !ok {
			return "", fmt.Errorf("add_file: missing content")
		}
		contentStr, ok := content.(string)
		if !ok {
			return "", fmt.Errorf("add_file: content must be a string")
		}
		// Create parent directory and decode base64 content
		dir := pathStr[:strings.LastIndex(pathStr, "/")]
		return fmt.Sprintf("sudo mkdir -p %s && echo '%s' | base64 -d | sudo tee %s > /dev/null", dir, contentStr, pathStr), nil

	case "add_dir":
		remotePath, ok := step.Args["path"]
		if !ok {
			return "", fmt.Errorf("add_dir: missing path")
		}
		pathStr, ok := remotePath.(string)
		if !ok {
			return "", fmt.Errorf("add_dir: path must be a string")
		}
		filesRaw, ok := step.Args["files"]
		if !ok {
			return "", fmt.Errorf("add_dir: missing files")
		}
		filesArr, ok := filesRaw.([]interface{})
		if !ok {
			return "", fmt.Errorf("add_dir: files must be an array")
		}

		var parts []string
		parts = append(parts, fmt.Sprintf("sudo mkdir -p %s", pathStr))
		for _, fileRaw := range filesArr {
			fileMap, ok := fileRaw.(map[string]interface{})
			if !ok {
				return "", fmt.Errorf("add_dir: each file must be an object")
			}
			relPath, _ := fileMap["relativePath"].(string)
			fileContent, _ := fileMap["content"].(string)
			if relPath == "" || fileContent == "" {
				continue
			}
			fullPath := pathStr + "/" + relPath
			dir := fullPath[:strings.LastIndex(fullPath, "/")]
			parts = append(parts, fmt.Sprintf("sudo mkdir -p %s && echo '%s' | base64 -d | sudo tee %s > /dev/null", dir, fileContent, fullPath))
		}
		return strings.Join(parts, " && "), nil

	default:
		return "", fmt.Errorf("unknown step type: %s", step.Type)
	}
}

// toStringSlice converts an interface{} (expected []interface{}) to []string.
func toStringSlice(v interface{}) ([]string, error) {
	arr, ok := v.([]interface{})
	if !ok {
		return nil, fmt.Errorf("expected array, got %T", v)
	}
	result := make([]string, len(arr))
	for i, item := range arr {
		s, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("expected string at index %d, got %T", i, item)
		}
		result[i] = s
	}
	return result, nil
}

// resolveImageManifest handles image-based sandbox creation.
// Returns the checkpoint ID to create from, or an error.
// If the image is already cached, returns immediately.
// Otherwise, builds it synchronously (boots sandbox, runs steps, checkpoints).
// The optional logFn callback is called for each build step with progress info.
func (s *Server) resolveImageManifest(ctx context.Context, orgID uuid.UUID, manifestJSON json.RawMessage, logFn BuildLogFunc) (uuid.UUID, error) {
	var manifest ImageManifest
	if err := json.Unmarshal(manifestJSON, &manifest); err != nil {
		return uuid.Nil, fmt.Errorf("invalid image manifest: %w", err)
	}

	contentHash := computeManifestHash(&manifest)

	// 1. Check DB cache
	if s.store != nil {
		cached, err := s.store.GetImageCacheByHash(ctx, orgID, contentHash)
		if err == nil && cached.Status == "ready" && cached.CheckpointID != nil {
			// Cache hit — verify the checkpoint's S3 data still exists.
			// Stale entries (from dead workers or failed uploads) would cause
			// every fork to fail until the cache entry is cleared.
			cp, cpErr := s.store.GetCheckpoint(ctx, *cached.CheckpointID)
			if cpErr != nil || cp.RootfsS3Key == nil {
				log.Printf("image-builder: cache hit for hash %s but checkpoint %s is invalid, rebuilding",
					contentHash[:12], cached.CheckpointID)
				_ = s.store.DeleteImageCache(ctx, orgID, cached.ID)
			} else {
				validHit := s.checkpointStore == nil
				if !validHit {
					if exists, _ := s.checkpointStore.Exists(ctx, *cp.RootfsS3Key); exists {
						validHit = true
					} else {
						log.Printf("image-builder: cache hit for hash %s but S3 object missing for checkpoint %s, rebuilding",
							contentHash[:12], cached.CheckpointID)
						_ = s.store.DeleteImageCache(ctx, orgID, cached.ID)
					}
				}
				if validHit {
					_ = s.store.TouchImageCacheUsage(ctx, cached.ID)
					// If manifest includes a name, assign it to the cached entry (makes it addressable as a snapshot)
					if manifest.Name != "" && (cached.Name == nil || *cached.Name != manifest.Name) {
						if err := s.store.SetImageCacheName(ctx, cached.ID, orgID, manifest.Name); err != nil {
							log.Printf("image-builder: failed to set name %q on cache entry: %v", manifest.Name, err)
						} else {
							log.Printf("image-builder: named cache entry %s as %q", cached.ID, manifest.Name)
						}
					}
					log.Printf("image-builder: cache hit for hash %s (checkpoint=%s)", contentHash[:12], cached.CheckpointID)
					if logFn != nil {
						logFn(0, "cache_hit", fmt.Sprintf("Image found in cache (hash=%s)", contentHash[:12]))
					}
					return *cached.CheckpointID, nil
				}
			}
		}
		if err == nil && cached.Status == "building" {
			// Another request is building this same image — wait for it
			return s.waitForInflightBuild(contentHash)
		}
	}

	// 2. Check in-flight builds (race between DB check and build start)
	buildKey := fmt.Sprintf("%s:%s", orgID, contentHash)
	if existing, loaded := inflightBuilds.Load(buildKey); loaded {
		build := existing.(*imageBuild)
		select {
		case <-build.ready:
			if build.err != nil {
				return uuid.Nil, build.err
			}
			return build.checkpointID, nil
		case <-ctx.Done():
			return uuid.Nil, ctx.Err()
		}
	}

	// 3. Start building
	build := &imageBuild{ready: make(chan struct{})}
	if _, loaded := inflightBuilds.LoadOrStore(buildKey, build); loaded {
		// Lost the race — another goroutine started building
		existing, _ := inflightBuilds.Load(buildKey)
		build = existing.(*imageBuild)
		select {
		case <-build.ready:
			if build.err != nil {
				return uuid.Nil, build.err
			}
			return build.checkpointID, nil
		case <-ctx.Done():
			return uuid.Nil, ctx.Err()
		}
	}

	// We own the build
	defer func() {
		close(build.ready)
		inflightBuilds.Delete(buildKey)
	}()

	// Create DB record
	cacheID := uuid.New()
	if s.store != nil {
		ic := &db.ImageCache{
			ID:          cacheID,
			OrgID:       orgID,
			ContentHash: contentHash,
			Manifest:    manifestJSON,
			Status:      "building",
		}
		if err := s.store.CreateImageCache(ctx, ic); err != nil {
			// Might conflict with concurrent insert — try to read existing
			cached, readErr := s.store.GetImageCacheByHash(ctx, orgID, contentHash)
			if readErr == nil && cached.Status == "ready" && cached.CheckpointID != nil {
				build.checkpointID = *cached.CheckpointID
				return build.checkpointID, nil
			}
			log.Printf("image-builder: cache insert failed (non-fatal): %v", err)
		}
	}

	// Build the image
	checkpointID, err := s.buildImage(ctx, orgID, &manifest, logFn)
	if err != nil {
		build.err = err
		if s.store != nil {
			_ = s.store.SetImageCacheFailed(ctx, cacheID)
		}
		return uuid.Nil, fmt.Errorf("image build failed: %w", err)
	}

	// Mark cache as ready
	build.checkpointID = checkpointID
	if s.store != nil {
		_ = s.store.SetImageCacheReady(ctx, cacheID, checkpointID)
	}

	log.Printf("image-builder: built image hash=%s checkpoint=%s (%d steps)", contentHash[:12], checkpointID, len(manifest.Steps))
	return checkpointID, nil
}

// waitForInflightBuild waits for an existing in-flight build to complete.
func (s *Server) waitForInflightBuild(contentHash string) (uuid.UUID, error) {
	// Poll DB until status changes (simple approach — builds are typically 30-120s)
	for i := 0; i < 120; i++ {
		time.Sleep(1 * time.Second)
		// We can't easily wait on the in-flight build from another request
		// so just poll the DB status
	}
	return uuid.Nil, fmt.Errorf("timed out waiting for image build (hash=%s)", contentHash[:12])
}

// buildImage boots a throwaway sandbox, runs the manifest steps, and creates a checkpoint.
func (s *Server) buildImage(ctx context.Context, orgID uuid.UUID, manifest *ImageManifest, logFn BuildLogFunc) (uuid.UUID, error) {
	// Determine base template — normalize common aliases to "base"
	base := manifest.Base
	if base == "" || base == "ubuntu" || base == "default" {
		base = "base"
	}

	// Create a throwaway sandbox
	buildSandboxID := "sb-build-" + uuid.New().String()[:8]
	cfg := types.SandboxConfig{
		Template:  base,
		Timeout:   600, // 10 min max for builds
		SandboxID: buildSandboxID,
	}

	log.Printf("image-builder: creating build sandbox %s (base=%s, steps=%d)", buildSandboxID, base, len(manifest.Steps))

	var grpcClient pb.SandboxWorkerClient
	var workerID string

	if s.workerRegistry != nil {
		region := s.region
		if region == "" {
			region = "iad"
		}
		worker, client, err := s.workerRegistry.GetLeastLoadedWorker(region)
		if err != nil {
			return uuid.Nil, fmt.Errorf("no workers available: %w", err)
		}
		grpcClient = client
		workerID = worker.ID

		// Create sandbox on worker
		createCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()

		resp, err := grpcClient.CreateSandbox(createCtx, &pb.CreateSandboxRequest{
			Template:       cfg.Template,
			Timeout:        int32(cfg.Timeout),
			NetworkEnabled: true, // Need network for apt/pip
			SandboxId:      buildSandboxID,
		})
		if err != nil {
			return uuid.Nil, fmt.Errorf("failed to create build sandbox: %w", err)
		}
		_ = resp
	} else if s.manager != nil {
		cfg.NetworkEnabled = true
		sb, err := s.manager.Create(ctx, cfg)
		if err != nil {
			return uuid.Nil, fmt.Errorf("failed to create build sandbox: %w", err)
		}
		buildSandboxID = sb.ID
		workerID = s.workerID
	} else {
		return uuid.Nil, fmt.Errorf("no execution backend available")
	}

	// Cleanup: destroy the build sandbox after a delay.
	// The checkpoint cache (qcow2 files) must survive long enough for the first
	// ForkFromCheckpoint to copy them. With reflink the fork is instant, but the
	// server-side create flow is async — the fork gRPC call may arrive seconds later.
	defer func() {
		go func() {
			time.Sleep(30 * time.Second) // give forks time to copy from cache
			cleanCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if grpcClient != nil {
				_, _ = grpcClient.DestroySandbox(cleanCtx, &pb.DestroySandboxRequest{SandboxId: buildSandboxID})
			} else if s.manager != nil {
				_ = s.manager.Kill(cleanCtx, buildSandboxID)
			}
			if s.store != nil {
				_ = s.store.UpdateSandboxSessionStatus(cleanCtx, buildSandboxID, "stopped", nil)
			}
			log.Printf("image-builder: destroyed build sandbox %s (delayed)", buildSandboxID)
		}()
	}()

	// Record session in PG (so worker can find it)
	if s.store != nil {
		cfgJSON, _ := json.Marshal(cfg)
		region := s.region
		if region == "" {
			region = "local"
		}
		if workerID == "" {
			workerID = "w-local-1"
		}
		_, _ = s.store.CreateSandboxSession(ctx, buildSandboxID, orgID, nil, cfg.Template, region, workerID, cfgJSON, []byte("{}"))
	}

	// Emit build start log
	if logFn != nil {
		logFn(0, "build", fmt.Sprintf("Building image with %d steps (sandbox=%s)", len(manifest.Steps), buildSandboxID))
	}

	// Execute each step
	for i, step := range manifest.Steps {
		cmd, err := translateStepToCommand(step)
		if err != nil {
			return uuid.Nil, fmt.Errorf("step %d (%s): %w", i, step.Type, err)
		}

		log.Printf("image-builder: [%s] step %d/%d: %s → %s", buildSandboxID, i+1, len(manifest.Steps), step.Type, truncate(cmd, 80))
		if logFn != nil {
			logFn(i+1, step.Type, fmt.Sprintf("Step %d/%d: %s", i+1, len(manifest.Steps), stepDescription(step)))
		}

		if err := s.execBuildStep(ctx, grpcClient, buildSandboxID, i, step.Type, cmd); err != nil {
			return uuid.Nil, err
		}

		if logFn != nil {
			logFn(i+1, step.Type, fmt.Sprintf("Step %d/%d completed", i+1, len(manifest.Steps)))
		}
	}

	if logFn != nil {
		logFn(len(manifest.Steps)+1, "checkpoint", "Creating checkpoint...")
	}

	// Checkpoint the build sandbox
	checkpointID := uuid.New()

	log.Printf("image-builder: checkpointing build sandbox %s as %s", buildSandboxID, checkpointID)

	// Create checkpoint record in DB
	if s.store != nil {
		cfgJSON, _ := json.Marshal(cfg)
		cp := &db.Checkpoint{
			ID:           checkpointID,
			SandboxID:    buildSandboxID,
			OrgID:        orgID,
			Name:         fmt.Sprintf("_image_build_%s", checkpointID.String()[:8]),
			SandboxConfig: cfgJSON,
		}
		if err := s.store.CreateCheckpoint(ctx, cp); err != nil {
			return uuid.Nil, fmt.Errorf("failed to create checkpoint record: %w", err)
		}
	}

	// Create checkpoint on worker
	if grpcClient != nil {
		cpCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
		defer cancel()

		resp, err := grpcClient.CreateCheckpoint(cpCtx, &pb.CreateCheckpointRequest{
			SandboxId:      buildSandboxID,
			CheckpointId:   checkpointID.String(),
			PrepareGolden:  true, // prepare golden snapshot for instant template creates
		})
		if err != nil {
			return uuid.Nil, fmt.Errorf("failed to checkpoint build sandbox: %w", err)
		}

		// Update checkpoint with S3 keys
		if s.store != nil {
			_ = s.store.SetCheckpointReady(ctx, checkpointID, resp.RootfsS3Key, resp.WorkspaceS3Key, 0)
		}
	} else if s.manager != nil {
		// Combined mode: call CreateCheckpoint via reflection-free approach.
		// The concrete firecracker.Manager implements CreateCheckpoint directly.
		type checkpointer interface {
			CreateCheckpoint(ctx context.Context, sandboxID, checkpointID string, checkpointStore *storage.CheckpointStore, onReady func()) (string, string, error)
		}
		cpMgr, ok := s.manager.(checkpointer)
		if !ok {
			return uuid.Nil, fmt.Errorf("manager does not support checkpoints")
		}

		rootfsKey, workspaceKey, err := cpMgr.CreateCheckpoint(ctx, buildSandboxID, checkpointID.String(), s.checkpointStore, func() {})
		if err != nil {
			return uuid.Nil, fmt.Errorf("failed to checkpoint build sandbox: %w", err)
		}

		// Wait for S3 upload to complete before the fork (which may land on another worker)
		type uploader interface {
			WaitUploads(timeout time.Duration)
		}
		if u, ok := s.manager.(uploader); ok {
			u.WaitUploads(5 * time.Minute)
		}

		if s.store != nil {
			_ = s.store.SetCheckpointReady(ctx, checkpointID, rootfsKey, workspaceKey, 0)
		}

		// Prepare golden snapshot for combined mode
		type goldenPreparer interface {
			RegisterTemplateGoldenFromCache(checkpointID string)
		}
		if gp, ok := s.manager.(goldenPreparer); ok {
			go gp.RegisterTemplateGoldenFromCache(checkpointID.String())
		}
	}

	return checkpointID, nil
}

// resolveSnapshot looks up a named snapshot and returns its checkpoint ID.
func (s *Server) resolveSnapshot(ctx context.Context, orgID uuid.UUID, snapshotName string) (uuid.UUID, error) {
	if s.store == nil {
		return uuid.Nil, fmt.Errorf("database not configured")
	}

	cached, err := s.store.GetImageCacheByName(ctx, orgID, snapshotName)
	if err != nil {
		return uuid.Nil, fmt.Errorf("snapshot %q not found: %w", snapshotName, err)
	}

	if cached.Status != "ready" {
		return uuid.Nil, fmt.Errorf("snapshot %q is not ready (status: %s)", snapshotName, cached.Status)
	}

	if cached.CheckpointID == nil {
		return uuid.Nil, fmt.Errorf("snapshot %q has no checkpoint", snapshotName)
	}

	_ = s.store.TouchImageCacheUsage(ctx, cached.ID)
	return *cached.CheckpointID, nil
}

// execBuildStep runs a single build step command on the sandbox.
func (s *Server) execBuildStep(ctx context.Context, grpcClient pb.SandboxWorkerClient, sandboxID string, stepIdx int, stepType, cmd string) error {
	execCtx, cancel := context.WithTimeout(ctx, 300*time.Second)
	defer cancel()

	if grpcClient != nil {
		resp, err := grpcClient.ExecCommand(execCtx, &pb.ExecCommandRequest{
			SandboxId: sandboxID,
			Command:   cmd,
			Timeout:   300,
		})
		if err != nil {
			return fmt.Errorf("step %d (%s) exec failed: %w", stepIdx, stepType, err)
		}
		if resp.ExitCode != 0 {
			return fmt.Errorf("step %d (%s) failed (exit %d): %s", stepIdx, stepType, resp.ExitCode, truncate(resp.Stderr, 500))
		}
	} else if s.manager != nil {
		result, err := s.manager.Exec(execCtx, sandboxID, types.ProcessConfig{
			Command: cmd,
			Timeout: 300,
		})
		if err != nil {
			return fmt.Errorf("step %d (%s) exec failed: %w", stepIdx, stepType, err)
		}
		if result.ExitCode != 0 {
			return fmt.Errorf("step %d (%s) failed (exit %d): %s", stepIdx, stepType, result.ExitCode, truncate(result.Stderr, 500))
		}
	}
	return nil
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// stepDescription returns a human-readable description of a build step.
func stepDescription(step ImageStep) string {
	switch step.Type {
	case "apt_install":
		if pkgs, ok := step.Args["packages"]; ok {
			if arr, err := toStringSlice(pkgs); err == nil {
				return fmt.Sprintf("apt-get install %s", strings.Join(arr, " "))
			}
		}
		return "apt-get install"
	case "pip_install":
		if pkgs, ok := step.Args["packages"]; ok {
			if arr, err := toStringSlice(pkgs); err == nil {
				return fmt.Sprintf("pip install %s", strings.Join(arr, " "))
			}
		}
		return "pip install"
	case "run":
		if cmds, ok := step.Args["commands"]; ok {
			if arr, err := toStringSlice(cmds); err == nil {
				return truncate(strings.Join(arr, " && "), 120)
			}
		}
		return "run commands"
	case "env":
		return "set environment variables"
	case "workdir":
		if p, ok := step.Args["path"].(string); ok {
			return fmt.Sprintf("set workdir %s", p)
		}
		return "set workdir"
	case "add_file":
		if p, ok := step.Args["path"].(string); ok {
			return fmt.Sprintf("add file %s", p)
		}
		return "add file"
	case "add_dir":
		if p, ok := step.Args["path"].(string); ok {
			return fmt.Sprintf("add directory %s", p)
		}
		return "add directory"
	default:
		return step.Type
	}
}

