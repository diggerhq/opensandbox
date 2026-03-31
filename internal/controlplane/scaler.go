package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/opensandbox/opensandbox/internal/compute"
	"github.com/opensandbox/opensandbox/internal/db"
	pb "github.com/opensandbox/opensandbox/proto/worker"
)

const (
	scaleUpThreshold   = 0.70 // Scale up when utilization > 70%
	scaleDownThreshold = 0.30 // Scale down when utilization < 30%
	maxWorkersPerRegion = 10  // Hard cap to prevent runaway launches
	pendingWorkerTTL    = 10 * time.Minute // How long to wait for a launched worker to register

	// Resource-based scaling thresholds (applied per-worker, trigger on ANY worker exceeding)
	resourceCPUThreshold  = 80.0 // Scale up if any worker CPU > 80%
	resourceMemThreshold  = 85.0 // Scale up if any worker memory > 85%
	resourceDiskThreshold = 75.0 // Scale up if any worker disk > 75%

	// Evacuation thresholds (per-worker, triggers live migration of sandboxes OFF the hot worker)
	evacuationCPUThreshold  = 90.0
	evacuationMemThreshold  = 90.0
	evacuationDiskThreshold = 80.0
	evacuationBatchSize    = 3                  // sandboxes to migrate per eval cycle per worker
	evacuationCooldown     = 60 * time.Second   // per-worker cooldown between evacuation batches
	drainTimeout           = 5 * time.Minute    // max time to drain a worker before force-destroying
)

// ScalerRegistry is the interface the Scaler uses to query worker state.
// Both WorkerRegistry (NATS) and RedisWorkerRegistry satisfy this.
type ScalerRegistry interface {
	Regions() []string
	GetWorkersByRegion(region string) []*WorkerInfo
	RegionUtilization(region string) float64
	RegionResourcePressure(region string) (maxCPU, maxMem, maxDisk float64)
	GetWorkerClient(workerID string) (pb.SandboxWorkerClient, error)
}

// ScalerConfig configures the autoscaler.
type ScalerConfig struct {
	Pool        compute.Pool
	Registry    ScalerRegistry
	Store       *db.Store     // for updating session worker_id after migration
	WorkerImage string
	Cooldown    time.Duration // minimum time between scale-up actions per region
	Interval    time.Duration // how often to evaluate scaling (0 = default 30s)
	MinWorkers  int           // minimum workers per region (0 = default 1). Set higher to pre-provision capacity.
	MaxWorkers  int           // maximum workers per region (0 = default 10). Hard cap to prevent runaway launches.
}

// pendingLaunch tracks an EC2 instance that was launched but hasn't registered yet.
type pendingLaunch struct {
	machineID string
	launchedAt time.Time
}

// drainState tracks a worker being drained for scale-down.
type drainState struct {
	workerID  string
	machineID string
	region    string
	startedAt time.Time
}

// Scaler manages autoscaling of workers via the compute Pool.
type Scaler struct {
	pool        compute.Pool
	registry    ScalerRegistry
	store       *db.Store
	image       string
	cooldown    time.Duration
	interval    time.Duration
	minWorkers  int
	maxWorkers  int
	lastScaleUp map[string]time.Time      // region -> last scale-up time
	pending     map[string][]pendingLaunch // region -> pending (unregistered) launches
	stop        chan struct{}
	wg          sync.WaitGroup

	// Drain & evacuation state
	draining       map[string]*drainState  // machineID -> state (workers being drained for scale-down)
	lastEvacuation map[string]time.Time    // workerID -> last evacuation time
	migrating      sync.Map               // sandboxID -> struct{} (prevent double-migrate)
}

// NewScaler creates a new autoscaling controller.
func NewScaler(cfg ScalerConfig) *Scaler {
	interval := cfg.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	cooldown := cfg.Cooldown
	if cooldown <= 0 {
		cooldown = 5 * time.Minute
	}
	minWorkers := cfg.MinWorkers
	if minWorkers <= 0 {
		minWorkers = 1
	}
	maxWorkers := cfg.MaxWorkers
	if maxWorkers <= 0 {
		maxWorkers = maxWorkersPerRegion
	}
	return &Scaler{
		pool:           cfg.Pool,
		registry:       cfg.Registry,
		store:          cfg.Store,
		image:          cfg.WorkerImage,
		cooldown:       cooldown,
		interval:       interval,
		minWorkers:     minWorkers,
		maxWorkers:     maxWorkers,
		lastScaleUp:    make(map[string]time.Time),
		pending:        make(map[string][]pendingLaunch),
		stop:           make(chan struct{}),
		draining:       make(map[string]*drainState),
		lastEvacuation: make(map[string]time.Time),
	}
}

// Start begins the autoscaling loop.
func (s *Scaler) Start() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				s.evaluate()
			case <-s.stop:
				return
			}
		}
	}()
	log.Printf("scaler: autoscaling controller started (interval=%s, cooldown=%s)", s.interval, s.cooldown)
}

// Stop stops the autoscaling loop.
func (s *Scaler) Stop() {
	close(s.stop)
	s.wg.Wait()
}

func (s *Scaler) evaluate() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Use discovered regions from workers, or fall back to the pool's region
	regions := s.registry.Regions()
	log.Printf("scaler: evaluate tick (regions=%v)", regions)
	if len(regions) == 0 {
		// No workers registered yet — use the pool's supported regions
		poolRegions, err := s.pool.SupportedRegions(ctx)
		if err == nil {
			regions = poolRegions
		}
	}
	for _, region := range regions {
		s.evaluateRegion(ctx, region)
	}
}

func (s *Scaler) evaluateRegion(ctx context.Context, region string) {
	workers := s.registry.GetWorkersByRegion(region)
	utilization := s.registry.RegionUtilization(region)
	maxCPU, maxMem, maxDisk := s.registry.RegionResourcePressure(region)

	// Expire stale pending launches
	s.expirePending(region)

	// Phase 1: Check progress on workers being drained
	s.checkDrainingWorkers(ctx, region)

	// Phase 2: Evacuate overloaded workers (live-migrate sandboxes off hot workers)
	s.evacuateHotWorkers(ctx, region, workers)

	// Phase 3: Determine if we need to scale up (count-based OR resource-based)
	needsScaleUp := false
	reason := ""

	if utilization > scaleUpThreshold {
		needsScaleUp = true
		reason = fmt.Sprintf("utilization %.1f%% > %.0f%%", utilization*100, scaleUpThreshold*100)
	}
	if !needsScaleUp && maxCPU > resourceCPUThreshold {
		needsScaleUp = true
		reason = fmt.Sprintf("CPU pressure %.1f%% > %.0f%%", maxCPU, resourceCPUThreshold)
	}
	if !needsScaleUp && maxMem > resourceMemThreshold {
		needsScaleUp = true
		reason = fmt.Sprintf("memory pressure %.1f%% > %.0f%%", maxMem, resourceMemThreshold)
	}
	if !needsScaleUp && maxDisk > resourceDiskThreshold {
		needsScaleUp = true
		reason = fmt.Sprintf("disk pressure %.1f%% > %.0f%%", maxDisk, resourceDiskThreshold)
	}

	// Ensure minimum workers are running (pre-provisioned capacity).
	// Ignores cooldowns — if we're below minimum, launch immediately.
	totalWorkers := len(workers) + len(s.pending[region])
	if totalWorkers < s.minWorkers {
		deficit := s.minWorkers - totalWorkers
		log.Printf("scaler: region %s below minimum workers (%d/%d), launching %d",
			region, totalWorkers, s.minWorkers, deficit)
		for i := 0; i < deficit; i++ {
			s.scaleUp(ctx, region)
		}
		return
	}

	if needsScaleUp {
		// Check cooldown before scaling up
		if last, ok := s.lastScaleUp[region]; ok && time.Since(last) < s.cooldown {
			log.Printf("scaler: region %s needs scale-up (%s) but cooldown active (%s remaining)",
				region, reason, s.cooldown-time.Since(last))
			return
		}

		// Don't scale up if there are already pending (unregistered) workers
		pending := s.pending[region]
		if len(pending) > 0 {
			log.Printf("scaler: region %s needs scale-up (%s) but %d worker(s) still pending registration",
				region, reason, len(pending))
			return
		}

		// Don't exceed max workers per region (exclude draining workers from capacity)
		effectiveWorkers := 0
		for _, w := range workers {
			if _, isDraining := s.draining[w.MachineID]; !isDraining {
				effectiveWorkers++
			}
		}
		if effectiveWorkers+len(s.pending[region]) >= s.maxWorkers {
			log.Printf("scaler: region %s at max workers (%d/%d), skipping scale-up", region, effectiveWorkers, s.maxWorkers)
			return
		}

		log.Printf("scaler: region %s %s, scaling up (cpu=%.1f%% mem=%.1f%% disk=%.1f%% util=%.1f%%)",
			region, reason, maxCPU, maxMem, maxDisk, utilization*100)
		s.scaleUp(ctx, region)
	} else if utilization < scaleDownThreshold && len(workers) > s.minWorkers {
		// Phase 4: Scale down via smart drain (live-migrate sandboxes, then destroy)
		log.Printf("scaler: region %s utilization %.1f%% < %.0f%%, initiating smart drain", region, utilization*100, scaleDownThreshold*100)
		s.smartScaleDown(ctx, region, workers)
	}
}

func (s *Scaler) scaleUp(_ context.Context, region string) {
	// Record scale-up intent immediately (prevents duplicate launches)
	s.lastScaleUp[region] = time.Now()

	// Run VM creation in background — Azure/EC2 can take 2-5 minutes.
	// Uses a 5-minute timeout independent of the evaluation cycle.
	go func() {
		createCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		opts := compute.MachineOpts{
			Region: region,
			Image:  s.image,
		}

		machine, err := s.pool.CreateMachine(createCtx, opts)
		if err != nil {
			log.Printf("scaler: failed to create machine in %s: %v", region, err)
			return
		}

		s.pending[region] = append(s.pending[region], pendingLaunch{
			machineID:  machine.ID,
			launchedAt: time.Now(),
		})
		log.Printf("scaler: created machine %s in %s (addr=%s), pending registration", machine.ID, region, machine.Addr)
	}()
}

// expirePending removes pending launches that have either registered or timed out.
func (s *Scaler) expirePending(region string) {
	pending := s.pending[region]
	if len(pending) == 0 {
		return
	}

	// Get currently registered worker machine IDs
	registered := make(map[string]bool)
	for _, w := range s.registry.GetWorkersByRegion(region) {
		if w.MachineID != "" {
			registered[w.MachineID] = true
		}
	}

	var remaining []pendingLaunch
	for _, p := range pending {
		if registered[p.machineID] {
			log.Printf("scaler: pending machine %s in %s has registered", p.machineID, region)
			continue
		}
		if time.Since(p.launchedAt) > pendingWorkerTTL {
			log.Printf("scaler: pending machine %s in %s timed out after %s, terminating",
				p.machineID, region, pendingWorkerTTL)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := s.pool.DestroyMachine(ctx, p.machineID); err != nil {
				log.Printf("scaler: failed to terminate stale machine %s: %v", p.machineID, err)
			}
			cancel()
			continue
		}
		remaining = append(remaining, p)
	}
	s.pending[region] = remaining
}

// --- Pressure Evacuation ---

// evacuateHotWorkers live-migrates sandboxes off workers that exceed critical thresholds.
func (s *Scaler) evacuateHotWorkers(_ context.Context, region string, workers []*WorkerInfo) {
	for _, w := range workers {
		if w.CPUPct < evacuationCPUThreshold && w.MemPct < evacuationMemThreshold && w.DiskPct < evacuationDiskThreshold {
			continue
		}
		// Skip if already being drained for scale-down
		if _, ok := s.draining[w.MachineID]; ok {
			continue
		}
		// Cooldown — don't evacuate the same worker every tick
		if last, ok := s.lastEvacuation[w.ID]; ok && time.Since(last) < evacuationCooldown {
			continue
		}

		// Find target: least-loaded worker that isn't the hot one and isn't draining
		target := s.findMigrationTarget(region, w.ID)
		if target == nil {
			log.Printf("scaler: worker %s under pressure (cpu=%.1f%% mem=%.1f%% disk=%.1f%%) but no migration target available",
				w.ID, w.CPUPct, w.MemPct, w.DiskPct)
			continue
		}

		log.Printf("scaler: worker %s under pressure (cpu=%.1f%% mem=%.1f%% disk=%.1f%%), evacuating up to %d sandboxes to %s",
			w.ID, w.CPUPct, w.MemPct, w.DiskPct, evacuationBatchSize, target.ID)

		s.lastEvacuation[w.ID] = time.Now()
		go s.evacuateBatch(w.ID, target.ID, evacuationBatchSize)
	}
}

// findMigrationTarget returns the best worker to receive migrated sandboxes.
func (s *Scaler) findMigrationTarget(region, excludeWorkerID string) *WorkerInfo {
	workers := s.registry.GetWorkersByRegion(region)
	var best *WorkerInfo
	bestScore := -1.0
	for _, w := range workers {
		if w.ID == excludeWorkerID {
			continue
		}
		if _, isDraining := s.draining[w.MachineID]; isDraining {
			continue
		}
		remaining := w.Capacity - w.Current
		if remaining <= 0 || w.CPUPct > 85 || w.MemPct > 85 || w.DiskPct > 85 {
			continue
		}
		resourceScore := (100.0 - w.CPUPct) / 100.0 * (100.0 - w.MemPct) / 100.0 * (100.0 - w.DiskPct) / 100.0
		score := float64(remaining) * resourceScore
		if score > bestScore {
			best = w
			bestScore = score
		}
	}
	return best
}

// evacuateBatch live-migrates up to count sandboxes from sourceWorker to targetWorker.
func (s *Scaler) evacuateBatch(sourceWorkerID, targetWorkerID string, count int) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	sourceClient, err := s.registry.GetWorkerClient(sourceWorkerID)
	if err != nil {
		log.Printf("scaler: evacuate: no gRPC client for source %s: %v", sourceWorkerID, err)
		return
	}

	listResp, err := sourceClient.ListSandboxes(ctx, &pb.ListSandboxesRequest{})
	if err != nil {
		log.Printf("scaler: evacuate: ListSandboxes failed for %s: %v", sourceWorkerID, err)
		return
	}

	migrated := 0
	for _, sb := range listResp.Sandboxes {
		if migrated >= count {
			break
		}
		if sb.Status != "running" {
			continue
		}
		if err := s.liveMigrateSandbox(ctx, sb.SandboxId, sourceWorkerID, targetWorkerID); err != nil {
			log.Printf("scaler: evacuate: migrate %s failed: %v", sb.SandboxId, err)
			continue
		}
		migrated++
	}

	log.Printf("scaler: evacuation batch complete for %s: %d/%d migrated to %s",
		sourceWorkerID, migrated, count, targetWorkerID)
}

// --- Smart Scale-Down ---

// smartScaleDown initiates draining of the least-loaded autoscaler-created worker
// by live-migrating its sandboxes to other workers before destroying the machine.
func (s *Scaler) smartScaleDown(_ context.Context, region string, workers []*WorkerInfo) {
	// Find the least-loaded worker that was created by the autoscaler.
	var target *WorkerInfo
	for _, w := range workers {
		if w.MachineID == "" {
			continue
		}
		if !strings.HasPrefix(w.MachineID, "osb-worker-") {
			continue // not created by autoscaler
		}
		if w.MachineID == "osb-worker-1" {
			continue // static worker, not autoscaled
		}
		// Skip workers already being drained
		if _, ok := s.draining[w.MachineID]; ok {
			continue
		}
		if target == nil || w.Current < target.Current {
			target = w
		}
	}

	if target == nil {
		return
	}

	log.Printf("scaler: initiating smart drain of worker %s (machine=%s, sandboxes=%d)",
		target.ID, target.MachineID, target.Current)

	s.draining[target.MachineID] = &drainState{
		workerID:  target.ID,
		machineID: target.MachineID,
		region:    region,
		startedAt: time.Now(),
	}

	go s.drainWorker(target.ID, target.MachineID, region)
}

// drainWorker live-migrates all sandboxes off a worker, then signals completion
// via the draining map (checkDrainingWorkers will destroy the machine).
func (s *Scaler) drainWorker(workerID, machineID, region string) {
	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	sourceClient, err := s.registry.GetWorkerClient(workerID)
	if err != nil {
		log.Printf("scaler: drain: no gRPC client for %s, will force-destroy on timeout", workerID)
		return
	}

	for {
		select {
		case <-ctx.Done():
			log.Printf("scaler: drain: timeout reached for worker %s", workerID)
			return
		default:
		}

		listResp, err := sourceClient.ListSandboxes(ctx, &pb.ListSandboxesRequest{})
		if err != nil {
			log.Printf("scaler: drain: ListSandboxes failed for %s: %v", workerID, err)
			return
		}

		// Count running sandboxes
		var running []string
		for _, sb := range listResp.Sandboxes {
			if sb.Status == "running" {
				running = append(running, sb.SandboxId)
			}
		}
		if len(running) == 0 {
			log.Printf("scaler: drain: worker %s fully drained", workerID)
			return
		}

		// Find target for this batch
		target := s.findMigrationTarget(region, workerID)
		if target == nil {
			log.Printf("scaler: drain: no migration target for %s, waiting...", workerID)
			time.Sleep(10 * time.Second)
			continue
		}

		// Migrate a batch
		batch := running
		if len(batch) > evacuationBatchSize {
			batch = batch[:evacuationBatchSize]
		}

		for _, sandboxID := range batch {
			if err := s.liveMigrateSandbox(ctx, sandboxID, workerID, target.ID); err != nil {
				log.Printf("scaler: drain: migrate %s failed: %v", sandboxID, err)
			}
		}

		// Brief pause between batches
		time.Sleep(2 * time.Second)
	}
}

// checkDrainingWorkers checks if draining workers are empty and destroys them.
func (s *Scaler) checkDrainingWorkers(ctx context.Context, region string) {
	for machineID, state := range s.draining {
		if state.region != region {
			continue
		}

		// Check if drain timed out
		if time.Since(state.startedAt) > drainTimeout {
			log.Printf("scaler: drain timeout for worker %s (machine=%s), force-destroying",
				state.workerID, machineID)
			s.destroyDrainedMachine(machineID)
			delete(s.draining, machineID)
			continue
		}

		// Check if worker has 0 sandboxes
		workers := s.registry.GetWorkersByRegion(region)
		for _, w := range workers {
			if w.MachineID == machineID && w.Current == 0 {
				log.Printf("scaler: worker %s fully drained (0 sandboxes), destroying machine %s",
					state.workerID, machineID)
				s.destroyDrainedMachine(machineID)
				delete(s.draining, machineID)
				break
			}
		}
	}
}

// destroyDrainedMachine tags and terminates a machine after drain.
func (s *Scaler) destroyDrainedMachine(machineID string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		if err := s.pool.DrainMachine(ctx, machineID); err != nil {
			log.Printf("scaler: DrainMachine tag failed for %s: %v", machineID, err)
		}

		if err := s.pool.DestroyMachine(ctx, machineID); err != nil {
			log.Printf("scaler: DestroyMachine failed for %s: %v", machineID, err)
		} else {
			log.Printf("scaler: machine %s destroyed successfully", machineID)
		}
	}()
}

// RebuildGoldenAll triggers a golden snapshot rebuild on all workers.
// Workers rebuild one at a time to avoid fleet-wide disruption.
// Returns a map of workerID → new golden version (or error string).
func (s *Scaler) RebuildGoldenAll(ctx context.Context) map[string]string {
	results := make(map[string]string)
	for _, region := range s.registry.Regions() {
		for _, w := range s.registry.GetWorkersByRegion(region) {
			client, err := s.registry.GetWorkerClient(w.ID)
			if err != nil {
				results[w.ID] = fmt.Sprintf("error: %v", err)
				continue
			}

			rebuildCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
			resp, err := client.RebuildGoldenSnapshot(rebuildCtx, &pb.RebuildGoldenSnapshotRequest{})
			cancel()

			if err != nil {
				results[w.ID] = fmt.Sprintf("error: %v", err)
				log.Printf("scaler: golden rebuild failed for worker %s: %v", w.ID, err)
			} else {
				results[w.ID] = resp.NewVersion
				log.Printf("scaler: golden rebuild complete for worker %s (%s → %s)",
					w.ID, resp.OldVersion, resp.NewVersion)
			}
		}
	}
	return results
}

// getWorkerInfo returns WorkerInfo for a specific worker by searching all regions.
func (s *Scaler) getWorkerInfo(workerID string) *WorkerInfo {
	for _, region := range s.registry.Regions() {
		for _, w := range s.registry.GetWorkersByRegion(region) {
			if w.ID == workerID {
				return w
			}
		}
	}
	return nil
}

// --- Live Migration Orchestration ---

// liveMigrateSandbox performs a full live migration of a sandbox between workers.
// Steps: pre-copy drives to S3 → prepare target → QMP migrate → complete → update DB.
func (s *Scaler) liveMigrateSandbox(ctx context.Context, sandboxID, sourceWorkerID, targetWorkerID string) error {
	// Prevent double-migrate
	if _, loaded := s.migrating.LoadOrStore(sandboxID, struct{}{}); loaded {
		return fmt.Errorf("migration already in progress for %s", sandboxID)
	}
	defer s.migrating.Delete(sandboxID)

	sourceClient, err := s.registry.GetWorkerClient(sourceWorkerID)
	if err != nil {
		return fmt.Errorf("source worker %s unreachable: %w", sourceWorkerID, err)
	}
	targetClient, err := s.registry.GetWorkerClient(targetWorkerID)
	if err != nil {
		return fmt.Errorf("target worker %s unreachable: %w", targetWorkerID, err)
	}

	t0 := time.Now()

	// Determine if source and target share the same golden snapshot version.
	// Same version → upload thin overlay, target rebases to local base image (fast, small).
	// Different version → flatten rootfs before upload, target uses it as-is (slow, large).
	sourceWorker := s.getWorkerInfo(sourceWorkerID)
	targetWorker := s.getWorkerInfo(targetWorkerID)
	sameGolden := sourceWorker != nil && targetWorker != nil &&
		sourceWorker.GoldenVersion != "" && sourceWorker.GoldenVersion == targetWorker.GoldenVersion
	flatten := !sameGolden

	// Step 1: Pre-copy drives to S3 (source uploads qcow2s)
	preCopyCtx, preCopyCancel := context.WithTimeout(ctx, 3*time.Minute)
	defer preCopyCancel()
	preCopyResp, err := sourceClient.PreCopyDrives(preCopyCtx, &pb.PreCopyDrivesRequest{
		SandboxId:    sandboxID,
		FlattenRootfs: flatten,
	})
	if err != nil {
		return fmt.Errorf("pre-copy drives: %w", err)
	}

	migrationMode := "overlay"
	if flatten {
		migrationMode = "flatten"
	}
	log.Printf("scaler: migrate %s: drives pre-copied to S3 (%dms, mode=%s)", sandboxID, time.Since(t0).Milliseconds(), migrationMode)

	// Step 2: Prepare target (downloads from S3, starts QEMU -incoming)
	// Get sandbox config from DB for CPU/memory/port
	cpuCount, memoryMB, guestPort, template := int32(1), int32(1024), int32(80), "base"
	if s.store != nil {
		session, err := s.store.GetSandboxSession(ctx, sandboxID)
		if err == nil && session != nil {
			if session.Template != "" {
				template = session.Template
			}
			// Parse CPU/memory from session config JSON
			var cfg struct {
				CPUCount int32 `json:"cpu_count"`
				MemoryMB int32 `json:"memory_mb"`
				Port     int32 `json:"port"`
			}
			if json.Unmarshal(session.Config, &cfg) == nil {
				if cfg.CPUCount > 0 {
					cpuCount = cfg.CPUCount
				}
				if cfg.MemoryMB > 0 {
					memoryMB = cfg.MemoryMB
				}
				if cfg.Port > 0 {
					guestPort = cfg.Port
				}
			}
		}
	}

	prepCtx, prepCancel := context.WithTimeout(ctx, 3*time.Minute)
	defer prepCancel()
	prepResp, err := targetClient.PrepareMigrationIncoming(prepCtx, &pb.PrepareMigrationIncomingRequest{
		SandboxId:      sandboxID,
		CpuCount:       cpuCount,
		MemoryMb:       memoryMB,
		GuestPort:      guestPort,
		Template:       template,
		RootfsS3Key:    preCopyResp.RootfsKey,
		WorkspaceS3Key: preCopyResp.WorkspaceKey,
		OverlayMode:    sameGolden,
	})
	if err != nil {
		return fmt.Errorf("prepare target: %w", err)
	}

	log.Printf("scaler: migrate %s: target prepared at %s (%dms)", sandboxID, prepResp.IncomingAddr, time.Since(t0).Milliseconds())

	// Step 3: Live migrate (source QMP → target)
	migrateCtx, migrateCancel := context.WithTimeout(ctx, 5*time.Minute)
	defer migrateCancel()
	_, err = sourceClient.LiveMigrate(migrateCtx, &pb.LiveMigrateRequest{
		SandboxId:    sandboxID,
		IncomingAddr: prepResp.IncomingAddr,
	})
	if err != nil {
		return fmt.Errorf("live migrate: %w", err)
	}

	log.Printf("scaler: migrate %s: QMP migration complete (%dms)", sandboxID, time.Since(t0).Milliseconds())

	// Step 4: Complete on target (reconnect agent, patch network)
	completeCtx, completeCancel := context.WithTimeout(ctx, 30*time.Second)
	defer completeCancel()
	_, err = targetClient.CompleteMigrationIncoming(completeCtx, &pb.CompleteMigrationIncomingRequest{
		SandboxId: sandboxID,
	})
	if err != nil {
		return fmt.Errorf("complete migration: %w", err)
	}

	// Step 5: Update DB — sandbox now on target worker
	if s.store != nil {
		_, _ = s.store.Pool().Exec(ctx, "UPDATE sandbox_sessions SET worker_id = $1 WHERE sandbox_id = $2", targetWorkerID, sandboxID)
	}

	elapsed := time.Since(t0).Milliseconds()
	log.Printf("scaler: migrate %s: complete in %dms (source=%s target=%s)", sandboxID, elapsed, sourceWorkerID, targetWorkerID)
	return nil
}
