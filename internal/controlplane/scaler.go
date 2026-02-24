package controlplane

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/opensandbox/opensandbox/internal/compute"
)

const (
	scaleUpThreshold    = 0.70 // Scale up when utilization > 70%
	scaleDownThreshold  = 0.30 // Scale down when utilization < 30%
	minWorkersPerRegion = 1
)

// ScalerRegistry is the interface the Scaler uses to query worker state.
// Both WorkerRegistry (NATS) and RedisWorkerRegistry satisfy this.
type ScalerRegistry interface {
	Regions() []string
	GetWorkersByRegion(region string) []*WorkerInfo
	RegionUtilization(region string) float64
}

// ScalerConfig configures the autoscaler.
type ScalerConfig struct {
	Pool        compute.Pool
	Registry    ScalerRegistry
	WorkerImage string
	Cooldown    time.Duration // minimum time between scale-up actions per region
	Interval    time.Duration // how often to evaluate scaling (0 = default 30s)
}

// Scaler manages autoscaling of workers via the compute Pool.
type Scaler struct {
	pool        compute.Pool
	registry    ScalerRegistry
	image       string
	cooldown    time.Duration
	interval    time.Duration
	lastScaleUp map[string]time.Time // region -> last scale-up time
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

	if utilization > scaleUpThreshold {
		// Check cooldown before scaling up
		if last, ok := s.lastScaleUp[region]; ok && time.Since(last) < s.cooldown {
			log.Printf("scaler: region %s needs scale-up but cooldown active (%s remaining)",
				region, s.cooldown-time.Since(last))
			return
		}
		log.Printf("scaler: region %s utilization %.1f%% > %.0f%%, scaling up", region, utilization*100, scaleUpThreshold*100)
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
	log.Printf("scaler: created machine %s in %s (addr=%s)", machine.ID, region, machine.Addr)
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
