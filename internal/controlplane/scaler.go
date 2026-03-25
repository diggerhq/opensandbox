package controlplane

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/opensandbox/opensandbox/internal/compute"
)

const (
	scaleUpThreshold   = 0.70 // Scale up when utilization > 70%
	scaleDownThreshold = 0.30 // Scale down when utilization < 30%
	maxWorkersPerRegion = 10  // Hard cap to prevent runaway launches
	pendingWorkerTTL    = 10 * time.Minute // How long to wait for a launched worker to register

	// Resource-based scaling thresholds (applied per-worker, trigger on ANY worker exceeding)
	resourceCPUThreshold = 80.0 // Scale up if any worker CPU > 80%
	resourceMemThreshold = 85.0 // Scale up if any worker memory > 85%
)

// ScalerRegistry is the interface the Scaler uses to query worker state.
// Both WorkerRegistry (NATS) and RedisWorkerRegistry satisfy this.
type ScalerRegistry interface {
	Regions() []string
	GetWorkersByRegion(region string) []*WorkerInfo
	RegionUtilization(region string) float64
	RegionResourcePressure(region string) (maxCPU, maxMem float64)
}

// ScalerConfig configures the autoscaler.
type ScalerConfig struct {
	Pool        compute.Pool
	Registry    ScalerRegistry
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

// Scaler manages autoscaling of workers via the compute Pool.
type Scaler struct {
	pool        compute.Pool
	registry    ScalerRegistry
	image       string
	cooldown    time.Duration
	interval    time.Duration
	minWorkers  int
	maxWorkers  int
	lastScaleUp map[string]time.Time      // region -> last scale-up time
	pending     map[string][]pendingLaunch // region -> pending (unregistered) launches
	stop        chan struct{}
	wg          sync.WaitGroup
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
		pool:        cfg.Pool,
		registry:    cfg.Registry,
		image:       cfg.WorkerImage,
		cooldown:    cooldown,
		interval:    interval,
		minWorkers:  minWorkers,
		maxWorkers:  maxWorkers,
		lastScaleUp: make(map[string]time.Time),
		pending:     make(map[string][]pendingLaunch),
		stop:        make(chan struct{}),
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
	maxCPU, maxMem := s.registry.RegionResourcePressure(region)

	// Expire stale pending launches
	s.expirePending(region)

	// Determine if we need to scale up: count-based OR resource-based
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

		// Don't exceed max workers per region
		if totalWorkers >= s.maxWorkers {
			log.Printf("scaler: region %s at max workers (%d/%d), skipping scale-up", region, totalWorkers, s.maxWorkers)
			return
		}

		log.Printf("scaler: region %s %s, scaling up (cpu=%.1f%% mem=%.1f%% util=%.1f%%)",
			region, reason, maxCPU, maxMem, utilization*100)
		s.scaleUp(ctx, region)
	} else if utilization < scaleDownThreshold && len(workers) > s.minWorkers {
		log.Printf("scaler: region %s utilization %.1f%% < %.0f%%, scaling down", region, utilization*100, scaleDownThreshold*100)
		s.scaleDown(ctx, region, workers)
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

func (s *Scaler) scaleDown(ctx context.Context, region string, workers []*WorkerInfo) {
	// Find the least-loaded worker that was created by the autoscaler.
	// Only target workers whose machine ID matches the naming convention (osb-worker-*).
	// This skips manually created workers (hostname like "localhost.localdomain" or "osb-worker-1").
	var target *WorkerInfo
	for _, w := range workers {
		if w.MachineID == "" {
			continue
		}
		if !strings.HasPrefix(w.MachineID, "osb-worker-") {
			continue // not created by autoscaler
		}
		// Skip the static worker-1 (manually created, not autoscaled)
		if w.MachineID == "osb-worker-1" {
			continue
		}
		if target == nil || w.Current < target.Current {
			target = w
		}
	}

	if target == nil {
		return
	}

	machineID := target.MachineID
	log.Printf("scaler: draining worker %s (machine=%s) in %s (current=%d)", target.ID, machineID, region, target.Current)

	// Run drain+destroy in background — Azure VM deletion takes 1-3 minutes
	go func() {
		destroyCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		if err := s.pool.DrainMachine(destroyCtx, machineID); err != nil {
			log.Printf("scaler: failed to drain machine %s: %v", machineID, err)
		}

		if err := s.pool.DestroyMachine(destroyCtx, machineID); err != nil {
			log.Printf("scaler: failed to destroy machine %s: %v", machineID, err)
		} else {
			log.Printf("scaler: machine %s destroyed successfully", machineID)
		}
	}()
}
