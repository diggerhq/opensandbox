package controlplane

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/opensandbox/opensandbox/internal/compute"
)

const (
	scaleUpThreshold    = 0.70 // Scale up when utilization > 70%
	scaleDownThreshold  = 0.30 // Scale down when utilization < 30%
	minWorkersPerRegion = 1
	maxWorkersPerRegion = 10   // Hard cap to prevent runaway launches
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
	return &Scaler{
		pool:        cfg.Pool,
		registry:    cfg.Registry,
		image:       cfg.WorkerImage,
		cooldown:    cooldown,
		interval:    interval,
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

	regions := s.registry.Regions()
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
		totalWorkers := len(workers) + len(pending)
		if totalWorkers >= maxWorkersPerRegion {
			log.Printf("scaler: region %s at max workers (%d), skipping scale-up", region, totalWorkers)
			return
		}

		log.Printf("scaler: region %s %s, scaling up (cpu=%.1f%% mem=%.1f%% util=%.1f%%)",
			region, reason, maxCPU, maxMem, utilization*100)
		s.scaleUp(ctx, region)
	} else if utilization < scaleDownThreshold && len(workers) > minWorkersPerRegion {
		log.Printf("scaler: region %s utilization %.1f%% < %.0f%%, scaling down", region, utilization*100, scaleDownThreshold*100)
		s.scaleDown(ctx, region, workers)
	}
}

func (s *Scaler) scaleUp(ctx context.Context, region string) {
	opts := compute.MachineOpts{
		Region: region,
		Image:  s.image,
	}

	machine, err := s.pool.CreateMachine(ctx, opts)
	if err != nil {
		log.Printf("scaler: failed to create machine in %s: %v", region, err)
		return
	}

	s.lastScaleUp[region] = time.Now()
	s.pending[region] = append(s.pending[region], pendingLaunch{
		machineID:  machine.ID,
		launchedAt: time.Now(),
	})
	log.Printf("scaler: created machine %s in %s (addr=%s), pending registration", machine.ID, region, machine.Addr)
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
	// Find the least-loaded worker to drain
	var target *WorkerInfo
	for _, w := range workers {
		if target == nil || w.Current < target.Current {
			target = w
		}
	}

	if target == nil {
		return
	}

	// Use EC2 instance ID (MachineID) for drain/destroy operations.
	// If the worker hasn't reported its machine ID, skip the drain.
	machineID := target.MachineID
	if machineID == "" {
		log.Printf("scaler: worker %s has no machine ID, skipping drain", target.ID)
		return
	}

	log.Printf("scaler: draining worker %s (machine=%s) in %s (current=%d)", target.ID, machineID, region, target.Current)

	if err := s.pool.DrainMachine(ctx, machineID); err != nil {
		log.Printf("scaler: failed to drain machine %s: %v", machineID, err)
		return
	}

	if err := s.pool.DestroyMachine(ctx, machineID); err != nil {
		log.Printf("scaler: failed to destroy machine %s: %v", machineID, err)
	}
}
