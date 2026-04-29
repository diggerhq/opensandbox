package controlplane

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/opensandbox/opensandbox/internal/compute"
	"github.com/opensandbox/opensandbox/internal/db"
	pb "github.com/opensandbox/opensandbox/proto/worker"
)

const (
	scaleUpThreshold   = 0.50 // Scale up when utilization > 50% (gives ~3 min runway for new worker to boot)
	scaleDownThreshold = 0.20 // Scale down when utilization < 20%
	maxWorkersPerRegion = 10  // Hard cap to prevent runaway launches
	pendingWorkerTTL    = 10 * time.Minute // How long to wait for a launched worker to register

	// Resource-based scaling thresholds (applied per-worker, trigger on ANY worker exceeding)
	resourceCPUThreshold  = 70.0 // Scale up if any worker CPU > 70%
	resourceMemThreshold  = 70.0 // Scale up if any worker memory > 70%
	resourceDiskThreshold = 60.0 // Scale up if any worker disk > 60%

	// Evacuation thresholds (per-worker, triggers live migration of sandboxes OFF the hot worker)
	evacuationCPUThreshold  = 80.0
	evacuationMemThreshold  = 80.0
	evacuationDiskThreshold = 70.0

	// Emergency thresholds — above these, hibernate sandboxes to free resources immediately
	// (no migration target needed, just dump to S3 and delete local files)
	emergencyCPUThreshold  = 95.0
	emergencyMemThreshold  = 95.0
	emergencyDiskThreshold = 90.0
	evacuationBatchSize    = 3                  // sandboxes to migrate per eval cycle per worker
	evacuationCooldown     = 60 * time.Second   // per-worker cooldown between evacuation batches
	drainTimeout           = 45 * time.Minute   // max time to drain a worker via live migration (allows 30 sandboxes × 10min each in batches of 3)

	creationFailureThreshold = 3                // consecutive failures before exponential backoff
	creationBackoffMin       = 1 * time.Minute  // initial backoff after threshold hit
	creationBackoffMax       = 10 * time.Minute // cap on exponential backoff
)

// ScalerRegistry is the interface the Scaler uses to query worker state.
// Both WorkerRegistry (NATS) and RedisWorkerRegistry satisfy this.
type ScalerRegistry interface {
	Regions() []string
	GetWorkersByRegion(region string) []*WorkerInfo
	RegionUtilization(region string) float64
	RegionResourcePressure(region string) (maxCPU, maxMem, maxDisk float64)
	GetWorkerClient(workerID string) (pb.SandboxWorkerClient, error)
	SetDraining(workerID string, draining bool)
}

// AMIRefresher is an optional interface a Pool can implement to support dynamic AMI updates.
// If the pool implements this, the scaler will periodically call RefreshAMI to check for new images.
type AMIRefresher interface {
	RefreshAMI(ctx context.Context) (amiID string, version string, err error)
}

// OrphanCleaner is an optional interface a Pool can implement to clean up orphaned resources
// (NICs, disks) left by failed VM creates. Called periodically by the scaler.
type OrphanCleaner interface {
	CleanupOrphanedResources(ctx context.Context) (int, error)
}

// ScalerConfig configures the autoscaler.
type ScalerConfig struct {
	Pool        compute.Pool
	Registry    ScalerRegistry
	Store       *db.Store     // for updating session worker_id after migration
	StateStore  ScalerStateStore // optional: persists scaler state to Redis (nil = in-memory)
	WorkerImage string
	Cooldown    time.Duration // minimum time between scale-up actions per region
	Interval    time.Duration // how often to evaluate scaling (0 = default 30s)
	MinWorkers     int        // minimum total workers per region (0 = default 1). Always kept running.
	MaxWorkers     int        // maximum workers per region (0 = default 10). Hard cap to prevent runaway launches.
	IdleReserve    int        // target idle (0 sandbox) workers for burst absorption (0 = default 1). Separate from MinWorkers.
}

// pendingLaunch tracks an EC2 instance that was launched but hasn't registered yet.
type pendingLaunch struct {
	MachineID  string    `json:"machine_id"`
	LaunchedAt time.Time `json:"launched_at"`
}

// drainState tracks a worker being drained for scale-down.
type drainState struct {
	WorkerID  string    `json:"worker_id"`
	MachineID string    `json:"machine_id"`
	Region    string    `json:"region"`
	StartedAt time.Time `json:"started_at"`
}

// Scaler manages autoscaling of workers via the compute Pool.
type Scaler struct {
	pool        compute.Pool
	registry    ScalerRegistry
	store       *db.Store
	state       ScalerStateStore // persisted state (Redis or in-memory)
	image       string
	cooldown    time.Duration
	interval    time.Duration
	minWorkers   int
	maxWorkers   int
	idleReserve  int

	mu       sync.Mutex     // protects stop/cancel
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	running  bool

	// Rolling replacement: version-aware AMI updates
	targetWorkerVersion string // desired worker version (from SSM); workers not matching this get replaced
	refreshCount        int    // tick counter for AMI refresh interval
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
	idleReserve := cfg.IdleReserve
	if idleReserve < 0 {
		idleReserve = 0
	}
	stateStore := cfg.StateStore
	if stateStore == nil {
		stateStore = NewInMemoryScalerState()
	}

	return &Scaler{
		pool:        cfg.Pool,
		registry:    cfg.Registry,
		store:       cfg.Store,
		state:       stateStore,
		image:       cfg.WorkerImage,
		cooldown:    cooldown,
		interval:    interval,
		minWorkers:  minWorkers,
		maxWorkers:  maxWorkers,
		idleReserve: idleReserve,
	}
}

// Start begins the autoscaling loop. Can be called multiple times (idempotent).
// Call Stop() first if already running.
func (s *Scaler) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.running = true

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				s.evaluate()
			case <-ctx.Done():
				return
			}
		}
	}()
	log.Printf("scaler: autoscaling controller started (interval=%s, cooldown=%s)", s.interval, s.cooldown)
}

// Stop stops the autoscaling loop. Can be called multiple times (idempotent).
func (s *Scaler) Stop() {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}
	s.cancel()
	s.running = false
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *Scaler) evaluate() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Refresh AMI from SSM/KV every ~60s. Also force a refresh on the very first
	// evaluate tick (refreshCount == 0) so targetWorkerVersion is known before
	// any scale-down decision — prevents the rolling-replace target from being
	// mistakenly drained when utilization is low.
	s.refreshCount++
	if s.refreshCount == 1 || s.refreshCount%2 == 0 {
		if refresher, ok := s.pool.(AMIRefresher); ok {
			if _, version, err := refresher.RefreshAMI(ctx); err != nil {
				log.Printf("scaler: AMI refresh failed: %v", err)
			} else if version != "" && version != s.targetWorkerVersion {
				log.Printf("scaler: target worker version updated: %q -> %q", s.targetWorkerVersion, version)
				s.targetWorkerVersion = version
			}
		}
	}

	// Clean up orphaned NICs/disks every ~5 min (every 10th tick)
	if s.refreshCount%10 == 0 {
		if cleaner, ok := s.pool.(OrphanCleaner); ok {
			if cleaned, err := cleaner.CleanupOrphanedResources(ctx); err != nil {
				log.Printf("scaler: orphan cleanup failed: %v", err)
			} else if cleaned > 0 {
				log.Printf("scaler: cleaned %d orphaned resources", cleaned)
			}
		}
	}

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
	s.mu.Lock()
	defer s.mu.Unlock()

	workers := s.registry.GetWorkersByRegion(region)
	utilization := s.registry.RegionUtilization(region)
	maxCPU, maxMem, maxDisk := s.registry.RegionResourcePressure(region)

	// Expire stale pending launches
	s.expirePending(region)

	// Phase 0: Emergency hibernate — workers in critical state, no migration needed
	s.emergencyHibernate(ctx, region, workers)

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

	// Rate-of-change scaling: if sandbox count is growing fast, scale up proactively.
	// This triggers before utilization thresholds so new workers are booting while
	// there's still headroom on existing workers.
	currentSandboxes := 0
	totalCapacity := 0
	for _, w := range workers {
		currentSandboxes += w.Current
		totalCapacity += w.Capacity
	}
	prevSandboxes, _ := s.state.GetLastSandboxCount(region)
	s.state.SetLastSandboxCount(region, currentSandboxes)
	growthRate := currentSandboxes - prevSandboxes // sandboxes added since last tick (30s)

	if !needsScaleUp && growthRate > 0 && totalCapacity > 0 {
		// Project: at this growth rate, how many ticks until we're full?
		remaining := totalCapacity - currentSandboxes
		if growthRate > 0 && remaining > 0 {
			ticksUntilFull := remaining / growthRate
			// If we'll be full within 6 ticks (~3 min, roughly how long a new worker takes to boot),
			// scale up now so the new worker is ready in time.
			if ticksUntilFull <= 6 {
				needsScaleUp = true
				reason = fmt.Sprintf("growth rate %d/tick, full in ~%d ticks (%d/%d used)",
					growthRate, ticksUntilFull, currentSandboxes, totalCapacity)
			}
		}
	}

	// Ensure minimum workers are running (pre-provisioned capacity).
	// Ignores cooldowns but respects creation failure backoff.
	totalWorkers := len(workers) + len(s.state.GetPendingLaunches(region))
	if totalWorkers < s.minWorkers {
		if until, ok := s.state.GetCreationBackoffUntil(region); ok {
			log.Printf("scaler: region %s below minimum workers (%d/%d) but creation backoff active until %s",
				region, totalWorkers, s.minWorkers, until.Format(time.RFC3339))
			return
		}
		deficit := s.minWorkers - totalWorkers
		log.Printf("scaler: region %s below minimum workers (%d/%d), launching %d",
			region, totalWorkers, s.minWorkers, deficit)
		for i := 0; i < deficit; i++ {
			s.scaleUp(ctx, region)
		}
		return
	}

	// Headroom: maintain a pool of idle workers for burst absorption.
	// Uses minWorkers as the reserve target — this is separate from the
	// minimum total workers check above. When bin-packing overflows into
	// reserve workers, we launch replacements one at a time so there's
	// always warm capacity without thrashing.
	idleWorkers := 0
	for _, w := range workers {
		if s.state.IsDraining(w.MachineID) {
			continue
		}
		if w.Current == 0 {
			idleWorkers++
		}
	}
	pendingCount := len(s.state.GetPendingLaunches(region))
	reserveTarget := s.idleReserve
	idleOrPending := idleWorkers + pendingCount
	if idleOrPending < reserveTarget && totalWorkers+pendingCount < s.maxWorkers {
		// Launch 1 at a time to avoid over-provisioning
		log.Printf("scaler: region %s reserve low (%d idle + %d pending < %d target), launching 1",
			region, idleWorkers, pendingCount, reserveTarget)
		s.scaleUp(ctx, region)
	}

	if needsScaleUp {
		// Check cooldown before scaling up
		if last, ok := s.state.GetLastScaleUp(region); ok && time.Since(last) < s.cooldown {
			log.Printf("scaler: region %s needs scale-up (%s) but cooldown active (%s remaining)",
				region, reason, s.cooldown-time.Since(last))
			return
		}

		// Don't scale up if there are already pending (unregistered) workers
		pending := s.state.GetPendingLaunches(region)
		if len(pending) > 0 {
			log.Printf("scaler: region %s needs scale-up (%s) but %d worker(s) still pending registration",
				region, reason, len(pending))
			return
		}

		// Don't exceed max workers per region (exclude draining workers from capacity)
		effectiveWorkers := 0
		for _, w := range workers {
			if !s.state.IsDraining(w.MachineID) {
				effectiveWorkers++
			}
		}
		if effectiveWorkers+len(s.state.GetPendingLaunches(region)) >= s.maxWorkers {
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

	// Phase 5: Rolling replacement of workers running old versions
	s.rollingReplace(ctx, region, workers)
}

func (s *Scaler) scaleUp(_ context.Context, region string) {
	// Check creation failure backoff
	if until, ok := s.state.GetCreationBackoffUntil(region); ok {
		log.Printf("scaler: region %s creation backoff active until %s, skipping scale-up",
			region, until.Format(time.RFC3339))
		return
	}

	// Record scale-up intent immediately (prevents duplicate launches)
	s.state.SetLastScaleUp(region, time.Now(), s.cooldown)

	// Register a placeholder pending launch so other code paths see it in-flight.
	placeholderID := fmt.Sprintf("osb-worker-pending-%d", time.Now().UnixNano())
	s.state.AddPendingLaunch(region, pendingLaunch{
		MachineID:  placeholderID,
		LaunchedAt: time.Now(),
	})

	// Run VM creation in background — Azure/EC2 can take 2-5 minutes.
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
			s.state.RemovePendingLaunch(region, placeholderID)

			failures := s.state.IncrCreationFailures(region)
			if failures >= creationFailureThreshold {
				backoff := creationBackoffMin * time.Duration(1<<(failures-creationFailureThreshold))
				if backoff > creationBackoffMax {
					backoff = creationBackoffMax
				}
				s.state.SetCreationBackoffUntil(region, time.Now().Add(backoff))
				log.Printf("scaler: region %s hit %d consecutive creation failures, backing off %s",
					region, failures, backoff)
			}
			return
		}

		// Swap placeholder for real machine ID
		s.state.RemovePendingLaunch(region, placeholderID)
		s.state.AddPendingLaunch(region, pendingLaunch{
			MachineID:  machine.ID,
			LaunchedAt: time.Now(),
		})
		s.state.ResetCreationFailures(region)
		log.Printf("scaler: created machine %s in %s (addr=%s), pending registration", machine.ID, region, machine.Addr)
	}()
}

// expirePending removes pending launches that have either registered or timed out.
func (s *Scaler) expirePending(region string) {
	pending := s.state.GetPendingLaunches(region)
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
		if registered[p.MachineID] {
			log.Printf("scaler: pending machine %s in %s has registered", p.MachineID, region)
			continue
		}
		if time.Since(p.LaunchedAt) > pendingWorkerTTL {
			log.Printf("scaler: pending machine %s in %s timed out after %s, terminating",
				p.MachineID, region, pendingWorkerTTL)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := s.pool.DestroyMachine(ctx, p.MachineID); err != nil {
				log.Printf("scaler: failed to terminate stale machine %s: %v", p.MachineID, err)
			}
			cancel()
			continue
		}
		remaining = append(remaining, p)
	}
	s.state.SetPendingLaunches(region, remaining)
}

// --- Emergency Hibernate ---

// emergencyHibernate hibernates sandboxes on workers that exceed critical thresholds.
// Unlike evacuation (which live-migrates), this dumps sandboxes to S3 and frees
// resources immediately. Used when a worker is about to run out of capacity and
// there may not be a viable migration target.
func (s *Scaler) emergencyHibernate(_ context.Context, region string, workers []*WorkerInfo) {
	for _, w := range workers {
		if w.CPUPct < emergencyCPUThreshold && w.MemPct < emergencyMemThreshold && w.DiskPct < emergencyDiskThreshold {
			continue
		}
		// Cooldown — reuse evacuation cooldown to avoid hammering
		if last, ok := s.state.GetLastEvacuation(w.ID); ok && time.Since(last) < evacuationCooldown {
			continue
		}

		log.Printf("scaler: EMERGENCY worker %s at critical levels (cpu=%.1f%% mem=%.1f%% disk=%.1f%%), hibernating sandboxes",
			w.ID, w.CPUPct, w.MemPct, w.DiskPct)

		s.state.SetLastEvacuation(w.ID, time.Now())
		go s.hibernateBatch(w.ID, evacuationBatchSize)
	}
}

// hibernateBatch hibernates up to count idle sandboxes on a worker to free resources.
// Picks sandboxes with the oldest last activity first.
func (s *Scaler) hibernateBatch(workerID string, count int) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	client, err := s.registry.GetWorkerClient(workerID)
	if err != nil {
		log.Printf("scaler: emergency: no gRPC client for %s: %v", workerID, err)
		return
	}

	listResp, err := client.ListSandboxes(ctx, &pb.ListSandboxesRequest{})
	if err != nil {
		log.Printf("scaler: emergency: ListSandboxes failed for %s: %v", workerID, err)
		return
	}

	hibernated := 0
	for _, sb := range listResp.Sandboxes {
		if hibernated >= count {
			break
		}
		if sb.Status != "running" {
			continue
		}
		_, err := client.HibernateSandbox(ctx, &pb.HibernateSandboxRequest{
			SandboxId: sb.SandboxId,
		})
		if err != nil {
			log.Printf("scaler: emergency: hibernate %s failed: %v", sb.SandboxId, err)
			continue
		}
		hibernated++
		log.Printf("scaler: emergency: hibernated %s on worker %s", sb.SandboxId, workerID)
	}

	log.Printf("scaler: emergency batch complete for %s: %d/%d hibernated",
		workerID, hibernated, count)
}

// --- Pressure Evacuation ---

// evacuateHotWorkers live-migrates sandboxes off workers that exceed critical
// thresholds.
//
// Two scheduling rules that matter under quota pressure:
//
//  1. **One source at a time.** Earlier versions spawned a goroutine per hot
//     source in parallel, which under quota constraints (e.g. eastus2 prod
//     where the Dadsv7 family is at ~60% of limit) means every source races
//     for the same one or two viable target workers. The first migration
//     wins, the others stall on capacity, and we get the imbalance pattern
//     we saw on prod: 60 sandboxes piled onto a single worker while the
//     others stayed lightly loaded. Serializing means we drain one source
//     completely before starting the next.
//
//  2. **Lightest source first.** A 5-sandbox source drains in seconds and
//     the now-empty worker becomes a viable target for subsequent drains.
//     A 60-sandbox source drained first would block all subsequent
//     evacuations behind it. Going lightest-first organically grows our
//     target pool from drained sources, so by the time we reach the
//     heaviest source we have N workers worth of target capacity instead
//     of 1.
//
// Per-sandbox target selection (inside evacuateBatch via liveMigrateSandbox)
// remains in place — it spreads the migrations within a single source's
// drain across whatever targets exist at the moment.
func (s *Scaler) evacuateHotWorkers(_ context.Context, region string, workers []*WorkerInfo) {
	// If an evacuation is already in flight, skip this tick — the active
	// drain will finish and release the lock; the next tick picks up.
	if !s.state.TryAcquireEvacuationLock() {
		return
	}
	releaseLock := true
	defer func() {
		if releaseLock {
			s.state.ReleaseEvacuationLock()
		}
	}()

	// Collect hot, non-draining workers off cooldown.
	hot := make([]*WorkerInfo, 0)
	for _, w := range workers {
		if w.CPUPct < evacuationCPUThreshold && w.MemPct < evacuationMemThreshold && w.DiskPct < evacuationDiskThreshold {
			continue
		}
		if s.state.IsDraining(w.MachineID) {
			continue
		}
		if last, ok := s.state.GetLastEvacuation(w.ID); ok && time.Since(last) < evacuationCooldown {
			continue
		}
		hot = append(hot, w)
	}
	if len(hot) == 0 {
		return
	}

	// Sort lightest first — see rule 2 in the function comment.
	sort.Slice(hot, func(i, j int) bool {
		return hot[i].Current < hot[j].Current
	})
	src := hot[0]

	// Pre-flight: confirm SOME viable target exists in the region. Pass 0 for
	// requiredMemMB; per-sandbox memory fit is checked inside liveMigrateSandbox
	// after PreCopyDrives reports the real RSS.
	if probe := s.findMigrationTarget(region, src.ID, 0); probe == nil {
		log.Printf("scaler: worker %s under pressure (cpu=%.1f%% mem=%.1f%% disk=%.1f%%, current=%d) but no migration target available — letting scale-up handle this",
			src.ID, src.CPUPct, src.MemPct, src.DiskPct, src.Current)
		return
	}

	log.Printf("scaler: worker %s selected for evacuation (lightest of %d hot, current=%d, cpu=%.1f%% mem=%.1f%% disk=%.1f%%) — draining serially",
		src.ID, len(hot), src.Current, src.CPUPct, src.MemPct, src.DiskPct)

	s.state.SetLastEvacuation(src.ID, time.Now())
	releaseLock = false // goroutine will release on its own
	go func() {
		defer s.state.ReleaseEvacuationLock()
		s.evacuateBatch(src.ID, evacuationBatchSize)
	}()
}

// findMigrationTarget returns the best worker to receive migrated sandboxes.
// Accounts for in-flight migrations so we don't pile onto the same target.
// findMigrationTarget returns the best worker to receive a migrated sandbox.
// requiredMemMB is the real RAM the migration will land (source VM RSS);
// workers whose actual used memory wouldn't leave that much headroom are
// rejected. This mirrors the worker's PrepareMigrationIncoming gate exactly
// so the scaler doesn't pick targets that will then reject the prepare.
// Pass 0 for requiredMemMB to skip the memory check (used by pre-flight
// "does ANY target exist" probes where we don't yet know the sandbox size).
//
// Scheduling on actual (not committed/configured) memory matters: a 16GB-max
// sandbox idling at 200MB RSS consumes 200MB of real host RAM, not 16GB. If
// we scheduled on the configured ceiling, a cluster at 50% committed would
// need 2x the workers of one at 50% actual — expensive dead weight for an
// idle-heavy workload like sandboxes.
func (s *Scaler) findMigrationTarget(region, excludeWorkerID string, requiredMemMB int32) *WorkerInfo {
	workers := s.registry.GetWorkersByRegion(region)

	var best *WorkerInfo
	bestScore := -1.0
	for _, w := range workers {
		if w.ID == excludeWorkerID {
			continue
		}
		if s.state.IsDraining(w.MachineID) {
			continue
		}
		// Subtract in-flight migrations from remaining capacity
		pending := s.state.GetInFlight(w.ID)
		remaining := w.Capacity - w.Current - pending
		if remaining <= 0 || w.CPUPct > 85 || w.MemPct > 85 || w.DiskPct > 85 {
			continue
		}
		// Actual-memory check. Uses host's real used RAM (derived from MemPct)
		// with a 10% safety margin — matches the worker's admission formula.
		if requiredMemMB > 0 && w.TotalMemoryMB > 0 {
			actualUsedMB := int(w.MemPct * float64(w.TotalMemoryMB) / 100)
			reserveMB := w.TotalMemoryMB / 10
			availableMB := w.TotalMemoryMB - actualUsedMB - reserveMB
			if int(requiredMemMB) > availableMB {
				continue
			}
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

// evacuateBatch live-migrates up to count sandboxes off sourceWorker, picking
// a fresh target per sandbox.
//
// Why per-sandbox target selection: an earlier version pinned all migrations
// in a batch to one target (chosen up front by evacuateHotWorkers). Result —
// during the prod event where 60 sandboxes were rehydrating, every batch
// piled onto whichever worker was least-loaded at batch start, blowing past
// it while the other targets stayed nearly empty. By passing "" to
// liveMigrateSandbox (which then calls findMigrationTarget after PreCopyDrives
// reports actual RSS), each migration sees up-to-date in-flight counts and
// the load distributes naturally across all viable targets.
func (s *Scaler) evacuateBatch(sourceWorkerID string, count int) {
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
		// Empty targetWorkerID → liveMigrateSandbox re-picks per call. This is
		// what produces the load-distribution behavior described in the
		// function comment.
		if err := s.liveMigrateSandbox(ctx, sb.SandboxId, sourceWorkerID, ""); err != nil {
			log.Printf("scaler: evacuate: migrate %s failed: %v", sb.SandboxId, err)
			continue
		}
		migrated++
	}

	log.Printf("scaler: evacuation batch complete for %s: %d/%d migrated (per-sandbox target selection)",
		sourceWorkerID, migrated, count)
}

// --- Smart Scale-Down ---

// smartScaleDown initiates draining of the least-loaded autoscaler-created worker
// by live-migrating its sandboxes to other workers before destroying the machine.
func (s *Scaler) smartScaleDown(_ context.Context, region string, workers []*WorkerInfo) {
	// Don't scale down while there are pending launches (workers still booting)
	if len(s.state.GetPendingLaunches(region)) > 0 {
		return
	}

	// If workers report a WorkerVersion but our target version isn't known yet
	// (first boot, before the initial AMI/KV refresh), don't scale down — we
	// can't tell stale from current, and blindly picking the least-loaded
	// worker will happily delete a rolling-replace target that just launched
	// empty.
	if s.targetWorkerVersion == "" {
		for _, w := range workers {
			if w.WorkerVersion != "" {
				return
			}
		}
	} else {
		// Target version known: ensure no stale workers remain before scaling down.
		for _, w := range workers {
			if w.WorkerVersion != s.targetWorkerVersion && w.WorkerVersion != "" {
				return // stale workers still exist, let rolling replace handle it
			}
		}
	}

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
		if s.state.IsDraining(w.MachineID) {
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

	s.state.SetDraining(target.MachineID, &drainState{
		WorkerID:  target.ID,
		MachineID: target.MachineID,
		Region:    region,
		StartedAt: time.Now(),
	})

	go s.drainWorker(target.ID, target.MachineID, region)
}

// rollingReplace drains workers running an old version one at a time,
// allowing the scaler to replace them with new-AMI workers.
func (s *Scaler) rollingReplace(ctx context.Context, region string, workers []*WorkerInfo) {
	if s.targetWorkerVersion == "" {
		return
	}

	var stale []*WorkerInfo
	var current int
	for _, w := range workers {
		// Skip workers already being drained
		if s.state.IsDraining(w.MachineID) {
			continue
		}
		// Skip manually provisioned workers (not autoscaler-managed)
		if w.MachineID == "" {
			continue
		}
		if !strings.HasPrefix(w.MachineID, "osb-worker-") {
			continue
		}
		if w.MachineID == "osb-worker-1" {
			continue // static worker, not autoscaled
		}
		if w.WorkerVersion == s.targetWorkerVersion {
			current++
		} else {
			stale = append(stale, w)
		}
	}

	if len(stale) == 0 {
		return
	}

	// If no current-version workers exist, try to launch a replacement first.
	// If launch fails (e.g., quota), fall back to draining one stale worker
	// to free capacity — the min-workers check will launch a replacement next tick.
	if current < 1 {
		pendingCount := len(s.state.GetPendingLaunches(region))
		if pendingCount > 0 {
			log.Printf("scaler: region %s has %d stale workers, waiting for pending replacement to register",
				region, len(stale))
			return
		}
		log.Printf("scaler: rolling replace: all %d workers stale — launching replacement with new version",
			len(stale))
		s.scaleUp(ctx, region)
		return
	}

	// Only drain one stale worker at a time (conservative rolling update)
	target := stale[0]
	for _, w := range stale[1:] {
		if w.Current < target.Current {
			target = w
		}
	}

	log.Printf("scaler: rolling replace: draining stale worker %s (version=%q, want=%q, sandboxes=%d)",
		target.ID, target.WorkerVersion, s.targetWorkerVersion, target.Current)

	s.state.SetDraining(target.MachineID, &drainState{
		WorkerID:  target.ID,
		MachineID: target.MachineID,
		Region:    region,
		StartedAt: time.Now(),
	})

	go s.drainWorker(target.ID, target.MachineID, region)
}

// drainWorker attempts to live-migrate sandboxes off a worker. If migration fails
// (e.g., S3 auth, no targets), falls back to waiting for natural expiry —
// sandboxes will timeout or be destroyed by users. No new sandboxes are routed
// to draining workers.
func (s *Scaler) drainWorker(workerID, machineID, region string) {
	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	// Mark worker as draining so routing skips it
	s.registry.SetDraining(workerID, true)

	sourceClient, err := s.registry.GetWorkerClient(workerID)
	if err != nil {
		log.Printf("scaler: drain: no gRPC client for %s, waiting for natural expiry", workerID)
		s.waitForNaturalDrain(ctx, workerID)
		return
	}

	migrationFailures := 0
	const maxMigrationFailures = 3 // after 3 failed attempts, stop trying migration

	for {
		select {
		case <-ctx.Done():
			log.Printf("scaler: drain: timeout reached for worker %s", workerID)
			return
		default:
		}

		listResp, err := sourceClient.ListSandboxes(ctx, &pb.ListSandboxesRequest{})
		if err != nil {
			log.Printf("scaler: drain: ListSandboxes failed for %s: %v, waiting for natural expiry", workerID, err)
			s.waitForNaturalDrain(ctx, workerID)
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
			// Terminal sweep: a post-QMP migration failure may leave DB rows
			// pointing at this worker even though ListSandboxes returns 0.
			// Reconcile any leftover running/migrating rows here so the row
			// reflects reality before the worker gets destroyed.
			if s.store != nil {
				sweepCtx, sweepCancel := context.WithTimeout(context.Background(), 10*time.Second)
				if n, err := s.store.MarkOrphanedOnWorker(sweepCtx, workerID, "drain completed; worker reported no sandboxes"); err != nil {
					log.Printf("scaler: drain: orphan sweep on %s failed: %v", workerID, err)
				} else if n > 0 {
					log.Printf("scaler: drain: orphan sweep on %s reconciled %d phantom row(s)", workerID, n)
				}
				sweepCancel()
			}
			return
		}

		// If too many migration failures, back off and retry rather than falling
		// into forever-wait. Drains still bound by drainTimeout (45 min). The
		// failure cause is often transient — target worker just came up, old
		// base only just got uploaded, a previous attempt left stale files —
		// and a periodic reset lets us succeed once the transient clears. If
		// migration genuinely can't complete, drainTimeout terminates the loop
		// and the next eval tick takes over.
		if migrationFailures >= maxMigrationFailures {
			log.Printf("scaler: drain: %d migration failures on %s, backing off 5min before retry (%d sandboxes remaining)",
				migrationFailures, workerID, len(running))
			select {
			case <-ctx.Done():
				log.Printf("scaler: drain: timeout reached for worker %s", workerID)
				return
			case <-time.After(5 * time.Minute):
			}
			migrationFailures = 0
			continue
		}

		// Pre-flight: is there ANY viable target in the region? We don't yet
		// know the sandbox memory sizes (those come from PreCopyDrives inside
		// liveMigrateSandbox), so pass 0 — this just confirms a non-draining
		// worker exists with slot capacity. Per-sandbox memory fit is checked
		// later by liveMigrateSandbox using the real sandbox size.
		//
		// If no target exists and we're under maxWorkers, proactively scale
		// up. This covers the case where committed memory is saturated on
		// every existing target but actual CPU/RAM usage is low, so the
		// utilization-based scale-up path in Evaluate() won't trigger.
		if probe := s.findMigrationTarget(region, workerID, 0); probe == nil {
			effective := 0
			for _, w := range s.registry.GetWorkersByRegion(region) {
				if !s.state.IsDraining(w.MachineID) {
					effective++
				}
			}
			pending := len(s.state.GetPendingLaunches(region))
			if effective+pending < s.maxWorkers {
				log.Printf("scaler: drain: no migration target for %s (%d sandboxes), triggering scale-up", workerID, len(running))
				s.scaleUp(ctx, region)
				select {
				case <-ctx.Done():
					return
				case <-time.After(60 * time.Second):
				}
				continue
			}
			log.Printf("scaler: drain: no migration target for %s (%d sandboxes) and at max workers (%d), waiting for natural expiry", workerID, len(running), s.maxWorkers)
			s.waitForNaturalDrain(ctx, workerID)
			return
		}

		// Migrate a batch — bounded parallelism to avoid overwhelming
		// network/disk on source and target workers. Each sandbox picks its
		// own target (based on its own memory footprint) inside liveMigrateSandbox.
		batch := running
		if len(batch) > evacuationBatchSize {
			batch = batch[:evacuationBatchSize]
		}

		batchFailed := false
		var wg sync.WaitGroup
		var failCount int64
		for _, sandboxID := range batch {
			wg.Add(1)
			go func(sbID string) {
				defer wg.Done()
				if err := s.liveMigrateSandbox(ctx, sbID, workerID, ""); err != nil {
					log.Printf("scaler: drain: migrate %s failed: %v", sbID, err)
					atomic.AddInt64(&failCount, 1)
				}
			}(sandboxID)
		}
		wg.Wait()
		if failCount > 0 {
			migrationFailures += int(failCount)
			batchFailed = true
		}

		if batchFailed {
			time.Sleep(5 * time.Second)
		} else {
			time.Sleep(2 * time.Second)
		}
	}
}

// waitForNaturalDrain polls until the worker has 0 sandboxes or the context expires.
// Sandboxes expire via timeout or user-initiated destroy.
func (s *Scaler) waitForNaturalDrain(ctx context.Context, workerID string) {
	for {
		select {
		case <-ctx.Done():
			log.Printf("scaler: natural drain: timeout for %s", workerID)
			return
		case <-time.After(30 * time.Second):
		}

		count := s.getDrainingWorkerSandboxCount(workerID)
		if count <= 0 {
			log.Printf("scaler: natural drain: worker %s is empty", workerID)
			return
		}
		log.Printf("scaler: natural drain: worker %s still has %d sandboxes, waiting...", workerID, count)
	}
}

// checkDrainingWorkers checks if draining workers are empty and destroys them.
func (s *Scaler) checkDrainingWorkers(ctx context.Context, region string) {
	for machineID, state := range s.state.AllDraining() {
		if state.Region != region {
			continue
		}

		// Check if drain timed out
		if time.Since(state.StartedAt) > drainTimeout {
			sandboxCount := s.getDrainingWorkerSandboxCount(state.WorkerID)
			if sandboxCount == 0 {
				// Sandboxes expired naturally — safe to destroy
				log.Printf("scaler: drain timeout for worker %s but 0 sandboxes remain, destroying", state.WorkerID)
				s.destroyDrainedMachine(machineID)
				s.state.RemoveDraining(machineID)
			} else {
				// Still has sandboxes — cancel drain, keep worker alive
				log.Printf("scaler: drain timeout for worker %s (machine=%s) with %d sandboxes — cancelling drain, keeping alive",
					state.WorkerID, machineID, sandboxCount)
				s.state.RemoveDraining(machineID)
			}
			continue
		}

		// Check if worker has 0 sandboxes
		workers := s.registry.GetWorkersByRegion(region)
		for _, w := range workers {
			if w.MachineID == machineID && w.Current == 0 {
				log.Printf("scaler: worker %s fully drained (0 sandboxes), destroying machine %s",
					state.WorkerID, machineID)
				s.destroyDrainedMachine(machineID)
				s.state.RemoveDraining(machineID)
				break
			}
		}
	}
}

// getDrainingWorkerSandboxCount returns the current sandbox count for a draining worker.
func (s *Scaler) getDrainingWorkerSandboxCount(workerID string) int {
	for _, region := range s.registry.Regions() {
		for _, w := range s.registry.GetWorkersByRegion(region) {
			if w.ID == workerID {
				return w.Current
			}
		}
	}
	return -1
}

// hibernateAllOnWorker attempts to hibernate all running sandboxes on a worker.
// Best-effort — logs failures but doesn't block.
func (s *Scaler) hibernateAllOnWorker(workerID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	client, err := s.registry.GetWorkerClient(workerID)
	if err != nil {
		log.Printf("scaler: hibernate-all: no gRPC client for %s: %v", workerID, err)
		return
	}

	listResp, err := client.ListSandboxes(ctx, &pb.ListSandboxesRequest{})
	if err != nil {
		log.Printf("scaler: hibernate-all: ListSandboxes failed for %s: %v", workerID, err)
		return
	}

	hibernated := 0
	for _, sb := range listResp.Sandboxes {
		if sb.Status != "running" {
			continue
		}
		_, err := client.HibernateSandbox(ctx, &pb.HibernateSandboxRequest{
			SandboxId: sb.SandboxId,
		})
		if err != nil {
			log.Printf("scaler: hibernate-all: hibernate %s failed: %v", sb.SandboxId, err)
			continue
		}
		hibernated++
	}

	log.Printf("scaler: hibernate-all: %d sandboxes hibernated on worker %s", hibernated, workerID)
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
//
// If targetWorkerID is empty, a target is picked after pre-copy using the
// sandbox's actual memory footprint (so an oversize sandbox doesn't get
// routed to a worker that can only reserve 4GB). Callers that need to force
// a specific destination (e.g. evacuateBatch, which uses a pre-scored
// target for the whole batch) pass it explicitly.
func (s *Scaler) liveMigrateSandbox(ctx context.Context, sandboxID, sourceWorkerID, targetWorkerID string) error {
	// Prevent double-migrate
	if !s.state.AcquireMigrationLock(sandboxID) {
		return fmt.Errorf("migration already in progress for %s", sandboxID)
	}
	defer s.state.ReleaseMigrationLock(sandboxID)

	sourceClient, err := s.registry.GetWorkerClient(sourceWorkerID)
	if err != nil {
		return fmt.Errorf("source worker %s unreachable: %w", sourceWorkerID, err)
	}

	t0 := time.Now()

	// Step 1: Pre-copy drives to S3 (source uploads thin overlay — never flattens).
	// Target worker rebases to its own base if golden versions differ.
	// Pre-copy before target selection — we need BaseMemoryMb from the
	// response to pick a target that can actually reserve that much.
	preCopyCtx, preCopyCancel := context.WithTimeout(ctx, 10*time.Minute)
	defer preCopyCancel()
	preCopyResp, err := sourceClient.PreCopyDrives(preCopyCtx, &pb.PreCopyDrivesRequest{
		SandboxId: sandboxID,
	})
	if err != nil {
		return fmt.Errorf("pre-copy drives: %w", err)
	}

	// Resolve golden version with three-tier fallback:
	// 1. PreCopyDrives response (worker in-memory)
	// 2. PG sandbox_sessions.golden_version (durable, set at creation)
	// 3. Worker heartbeat (all sandboxes on a worker share its golden base)
	sourceGoldenVersion := preCopyResp.GoldenVersion
	if sourceGoldenVersion == "" && s.store != nil {
		if session, err := s.store.GetSandboxSession(ctx, sandboxID); err == nil && session.GoldenVersion != nil && *session.GoldenVersion != "" {
			sourceGoldenVersion = *session.GoldenVersion
			log.Printf("scaler: migrate %s: goldenVersion from PG: %s", sandboxID, sourceGoldenVersion)
		}
	}
	if sourceGoldenVersion == "" {
		sourceWorker := s.getWorkerInfo(sourceWorkerID)
		if sourceWorker != nil && sourceWorker.GoldenVersion != "" {
			sourceGoldenVersion = sourceWorker.GoldenVersion
			log.Printf("scaler: migrate %s: sandbox missing goldenVersion, using worker's: %s", sandboxID, sourceGoldenVersion)
		} else {
			return fmt.Errorf("source sandbox has no goldenVersion — not in gRPC response, PG, or worker heartbeat")
		}
	}

	log.Printf("scaler: migrate %s: drives pre-copied to S3 (%dms, golden=%s)", sandboxID, time.Since(t0).Milliseconds(), sourceGoldenVersion)

	// Step 2: Prepare target (downloads thin overlay from S3, rebases if needed, starts QEMU -incoming)
	// CPU and memory come from the source worker — must match exactly for QEMU migration.
	cpuCount := preCopyResp.BaseCpuCount
	memoryMB := preCopyResp.BaseMemoryMb
	if cpuCount == 0 {
		cpuCount = 2 // fallback for old workers that don't report
	}
	if memoryMB == 0 {
		memoryMB = 1024
	}
	guestPort, template := int32(80), "default"
	if s.store != nil {
		session, err := s.store.GetSandboxSession(ctx, sandboxID)
		if err == nil && session != nil {
			if session.Template != "" {
				template = session.Template
			}
		}
	}

	// Real RAM the migration will land on the target = source VM's current
	// RSS (reported by PreCopyDrives). Fall back to the configured base
	// memory if the source is an older worker that doesn't report RSS.
	// Final safety floor: 256MB (QEMU overhead alone).
	actualMemMB := preCopyResp.ActualMemoryMb
	if actualMemMB == 0 {
		actualMemMB = memoryMB
	}
	if actualMemMB < 256 {
		actualMemMB = 256
	}

	// Target selection: if caller didn't pre-pick, find a worker with enough
	// actual-memory headroom for this sandbox's real footprint. Must happen
	// after PreCopyDrives so we know the RSS.
	if targetWorkerID == "" {
		srcInfo := s.getWorkerInfo(sourceWorkerID)
		if srcInfo == nil {
			return fmt.Errorf("source worker %s not in registry", sourceWorkerID)
		}
		target := s.findMigrationTarget(srcInfo.Region, sourceWorkerID, actualMemMB)
		if target == nil {
			return fmt.Errorf("no viable migration target in %s for %dMB actual-RSS sandbox", srcInfo.Region, actualMemMB)
		}
		targetWorkerID = target.ID
	}

	targetClient, err := s.registry.GetWorkerClient(targetWorkerID)
	if err != nil {
		return fmt.Errorf("target worker %s unreachable: %w", targetWorkerID, err)
	}

	// Mark sandbox as migrating in DB — blocks exec routing during migration.
	// Must happen after target selection because the DB row records the
	// destination worker.
	migrationCompleted := false
	qmpSucceeded := false
	if s.store != nil {
		if err := s.store.SetMigrating(ctx, sandboxID, targetWorkerID); err != nil {
			log.Printf("scaler: failed to set migrating state for %s: %v", sandboxID, err)
		}
		defer func() {
			if migrationCompleted || s.store == nil {
				return
			}
			// Recovery path depends on which phase failed:
			//   - Pre-QMP: source still has the VM. Revert to running on source.
			//   - Post-QMP: source's QEMU has shut down (state migrated). Reverting to
			//     running on source produces a phantom — DB says running, source has
			//     no QEMU, drain's ListSandboxes returns 0 and exits cleanly leaving
			//     the row stuck. Mark error instead so the sandbox is visibly broken.
			if qmpSucceeded {
				if err := s.store.FailMigrationPostQMP(ctx, sandboxID, "migration failed after QMP transfer; source VM gone, target failed to complete"); err != nil {
					log.Printf("scaler: migrate %s: FailMigrationPostQMP failed: %v", sandboxID, err)
				}
			} else {
				if err := s.store.FailMigration(ctx, sandboxID); err != nil {
					log.Printf("scaler: migrate %s: FailMigration failed: %v", sandboxID, err)
				}
			}
		}()
	}

	// Track in-flight migration to target so other evacuations don't pile on.
	s.state.IncrInFlight(targetWorkerID)
	defer s.state.DecrInFlight(targetWorkerID)

	prepCtx, prepCancel := context.WithTimeout(ctx, 10*time.Minute)
	defer prepCancel()
	prepResp, err := targetClient.PrepareMigrationIncoming(prepCtx, &pb.PrepareMigrationIncomingRequest{
		SandboxId:           sandboxID,
		CpuCount:            cpuCount,
		MemoryMb:            memoryMB,
		GuestPort:           guestPort,
		Template:            template,
		RootfsS3Key:         preCopyResp.RootfsKey,
		WorkspaceS3Key:      preCopyResp.WorkspaceKey,
		OverlayMode:         true,
		SourceGoldenVersion: sourceGoldenVersion,
		TargetMemoryMb:      actualMemMB,
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
	// QMP transfer done — source has shut down its VM. Any failure from here
	// must mark the sandbox as error rather than reverting to "running on source"
	// (the source's QEMU is gone; reverting would produce a phantom).
	qmpSucceeded = true

	log.Printf("scaler: migrate %s: QMP migration complete (%dms)", sandboxID, time.Since(t0).Milliseconds())

	// Step 4: Complete on target (reconnect agent, patch network).
	// Agent socket reconnect is bounded to 10s and patchGuestNetwork runs
	// several guest-side exec commands that can be slow on a freshly resumed
	// VM (especially post-rebase). 30s was too tight — a 3min window is
	// generous but still bounded so we eventually fail loud if something is
	// truly stuck.
	completeCtx, completeCancel := context.WithTimeout(ctx, 3*time.Minute)
	defer completeCancel()
	_, err = targetClient.CompleteMigrationIncoming(completeCtx, &pb.CompleteMigrationIncomingRequest{
		SandboxId: sandboxID,
	})
	if err != nil {
		return fmt.Errorf("complete migration: %w", err)
	}

	// Step 5: Complete migration — update DB status and worker_id atomically.
	// Use background context in case the drain context is close to expiry.
	if s.store != nil {
		dbCtx, dbCancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := s.store.CompleteMigration(dbCtx, sandboxID, targetWorkerID); err != nil {
			log.Printf("scaler: migrate %s: WARNING: CompleteMigration DB update failed: %v", sandboxID, err)
		}
		// Update golden version to target worker's — the rootfs was rebased
		// to the target's base image during migration.
		targetWorker := s.getWorkerInfo(targetWorkerID)
		if targetWorker != nil && targetWorker.GoldenVersion != "" {
			_ = s.store.SetSandboxGoldenVersion(dbCtx, sandboxID, targetWorker.GoldenVersion)
		}
		dbCancel()
	}
	migrationCompleted = true

	elapsed := time.Since(t0).Milliseconds()
	log.Printf("scaler: migrate %s: complete in %dms (source=%s target=%s)", sandboxID, elapsed, sourceWorkerID, targetWorkerID)
	return nil
}
