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
	drainTimeout           = 15 * time.Minute   // max time to drain a worker via live migration
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

// AMIRefresher is an optional interface a Pool can implement to support dynamic AMI updates.
// If the pool implements this, the scaler will periodically call RefreshAMI to check for new images.
type AMIRefresher interface {
	RefreshAMI(ctx context.Context) (amiID string, version string, err error)
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
	if idleReserve <= 0 {
		idleReserve = 1
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

	// Refresh AMI from SSM every ~60s (every 2nd tick at 30s interval)
	s.refreshCount++
	if s.refreshCount%2 == 0 {
		if refresher, ok := s.pool.(AMIRefresher); ok {
			if _, version, err := refresher.RefreshAMI(ctx); err != nil {
				log.Printf("scaler: AMI refresh failed: %v", err)
			} else if version != "" && version != s.targetWorkerVersion {
				log.Printf("scaler: target worker version updated: %q -> %q", s.targetWorkerVersion, version)
				s.targetWorkerVersion = version
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
	// Ignores cooldowns — if we're below minimum, launch immediately.
	totalWorkers := len(workers) + len(s.state.GetPendingLaunches(region))
	if totalWorkers < s.minWorkers {
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
	// Record scale-up intent immediately (prevents duplicate launches)
	s.state.SetLastScaleUp(region, time.Now(), s.cooldown)

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

		s.state.AddPendingLaunch(region, pendingLaunch{
			machineID:  machine.ID,
			launchedAt: time.Now(),
		})
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

// evacuateHotWorkers live-migrates sandboxes off workers that exceed critical thresholds.
func (s *Scaler) evacuateHotWorkers(_ context.Context, region string, workers []*WorkerInfo) {
	for _, w := range workers {
		if w.CPUPct < evacuationCPUThreshold && w.MemPct < evacuationMemThreshold && w.DiskPct < evacuationDiskThreshold {
			continue
		}
		// Skip if already being drained for scale-down
		if s.state.IsDraining(w.MachineID) {
			continue
		}
		// Cooldown — don't evacuate the same worker every tick
		if last, ok := s.state.GetLastEvacuation(w.ID); ok && time.Since(last) < evacuationCooldown {
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

		s.state.SetLastEvacuation(w.ID, time.Now())
		go s.evacuateBatch(w.ID, target.ID, evacuationBatchSize)
	}
}

// findMigrationTarget returns the best worker to receive migrated sandboxes.
// Accounts for in-flight migrations so we don't pile onto the same target.
func (s *Scaler) findMigrationTarget(region, excludeWorkerID string) *WorkerInfo {
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
		workerID:  target.ID,
		machineID: target.MachineID,
		region:    region,
		startedAt: time.Now(),
	})

	go s.drainWorker(target.ID, target.MachineID, region)
}

// rollingReplace drains workers running an old version one at a time,
// allowing the scaler to replace them with new-AMI workers.
func (s *Scaler) rollingReplace(_ context.Context, region string, workers []*WorkerInfo) {
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

	// Don't drain if no current-version workers exist yet — scale up first
	if current < 1 {
		log.Printf("scaler: region %s has %d stale workers but 0 current-version, waiting for new workers to register",
			region, len(stale))
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
		workerID:  target.ID,
		machineID: target.MachineID,
		region:    region,
		startedAt: time.Now(),
	})

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
				log.Printf("scaler: drain: migrate %s to %s failed: %v, will retry next cycle", sandboxID, target.ID, err)
				// Don't abort — try remaining sandboxes and retry failed ones next cycle
			}
		}

		// Brief pause between batches
		time.Sleep(2 * time.Second)
	}
}

// checkDrainingWorkers checks if draining workers are empty and destroys them.
func (s *Scaler) checkDrainingWorkers(ctx context.Context, region string) {
	for machineID, state := range s.state.AllDraining() {
		if state.region != region {
			continue
		}

		// Check if drain timed out
		if time.Since(state.startedAt) > drainTimeout {
			sandboxCount := s.getDrainingWorkerSandboxCount(state.workerID)
			if sandboxCount == 0 {
				// Sandboxes expired naturally — safe to destroy
				log.Printf("scaler: drain timeout for worker %s but 0 sandboxes remain, destroying", state.workerID)
				s.destroyDrainedMachine(machineID)
				s.state.RemoveDraining(machineID)
			} else {
				// Still has sandboxes — cancel drain, keep worker alive
				log.Printf("scaler: drain timeout for worker %s (machine=%s) with %d sandboxes — cancelling drain, keeping alive",
					state.workerID, machineID, sandboxCount)
				s.state.RemoveDraining(machineID)
			}
			continue
		}

		// Check if worker has 0 sandboxes
		workers := s.registry.GetWorkersByRegion(region)
		for _, w := range workers {
			if w.MachineID == machineID && w.Current == 0 {
				log.Printf("scaler: worker %s fully drained (0 sandboxes), destroying machine %s",
					state.workerID, machineID)
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
func (s *Scaler) liveMigrateSandbox(ctx context.Context, sandboxID, sourceWorkerID, targetWorkerID string) error {
	// Prevent double-migrate
	if !s.state.AcquireMigrationLock(sandboxID) {
		return fmt.Errorf("migration already in progress for %s", sandboxID)
	}
	defer s.state.ReleaseMigrationLock(sandboxID)

	// Mark sandbox as migrating in DB — blocks exec routing during migration
	migrationCompleted := false
	if s.store != nil {
		if err := s.store.SetMigrating(ctx, sandboxID, targetWorkerID); err != nil {
			log.Printf("scaler: failed to set migrating state for %s: %v", sandboxID, err)
		}
		defer func() {
			if !migrationCompleted && s.store != nil {
				s.store.FailMigration(ctx, sandboxID)
			}
		}()
	}

	// Track in-flight migration to target so other evacuations don't pile on
	s.state.IncrInFlight(targetWorkerID)
	defer s.state.DecrInFlight(targetWorkerID)

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

	// Step 5: Complete migration — update DB status and worker_id atomically
	if s.store != nil {
		s.store.CompleteMigration(ctx, sandboxID, targetWorkerID)
	}
	migrationCompleted = true

	elapsed := time.Since(t0).Milliseconds()
	log.Printf("scaler: migrate %s: complete in %dms (source=%s target=%s)", sandboxID, elapsed, sourceWorkerID, targetWorkerID)
	return nil
}
