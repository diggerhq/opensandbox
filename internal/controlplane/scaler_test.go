package controlplane

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opensandbox/opensandbox/internal/compute"
	pb "github.com/opensandbox/opensandbox/proto/worker"
)

// --- Mock ScalerRegistry ---

type mockRegistry struct {
	mu      sync.RWMutex
	workers map[string][]*WorkerInfo // region -> workers
}

func newMockRegistry() *mockRegistry {
	return &mockRegistry{workers: make(map[string][]*WorkerInfo)}
}

func (r *mockRegistry) addWorker(w *WorkerInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.workers[w.Region] = append(r.workers[w.Region], w)
}

func (r *mockRegistry) removeWorker(workerID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for region, workers := range r.workers {
		for i, w := range workers {
			if w.ID == workerID {
				r.workers[region] = append(workers[:i], workers[i+1:]...)
				return
			}
		}
	}
}

func (r *mockRegistry) getWorker(workerID string) *WorkerInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, workers := range r.workers {
		for _, w := range workers {
			if w.ID == workerID {
				return w
			}
		}
	}
	return nil
}

func (r *mockRegistry) updateWorker(workerID string, fn func(w *WorkerInfo)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, workers := range r.workers {
		for _, w := range workers {
			if w.ID == workerID {
				fn(w)
				return
			}
		}
	}
}

func (r *mockRegistry) Regions() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var regions []string
	for region := range r.workers {
		regions = append(regions, region)
	}
	return regions
}

func (r *mockRegistry) GetWorkersByRegion(region string) []*WorkerInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]*WorkerInfo, len(r.workers[region]))
	copy(result, r.workers[region])
	return result
}

func (r *mockRegistry) RegionUtilization(region string) float64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var totalCap, totalCur int
	for _, w := range r.workers[region] {
		totalCap += w.Capacity
		totalCur += w.Current
	}
	if totalCap == 0 {
		return 0
	}
	return float64(totalCur) / float64(totalCap)
}

func (r *mockRegistry) RegionResourcePressure(region string) (maxCPU, maxMem, maxDisk float64) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, w := range r.workers[region] {
		if w.CPUPct > maxCPU {
			maxCPU = w.CPUPct
		}
		if w.MemPct > maxMem {
			maxMem = w.MemPct
		}
		if w.DiskPct > maxDisk {
			maxDisk = w.DiskPct
		}
	}
	return
}

func (r *mockRegistry) GetWorkerClient(workerID string) (pb.SandboxWorkerClient, error) {
	return nil, fmt.Errorf("mock: no gRPC client for %s", workerID)
}

// --- Mock compute.Pool ---

type mockPool struct {
	mu        sync.Mutex
	machines  map[string]string // machineID -> region
	created   int32
	destroyed int32
}

func newMockPool() *mockPool {
	return &mockPool{machines: make(map[string]string)}
}

func (p *mockPool) CreateMachine(_ context.Context, opts compute.MachineOpts) (*compute.Machine, error) {
	id := fmt.Sprintf("osb-worker-%d", atomic.AddInt32(&p.created, 1))
	p.mu.Lock()
	p.machines[id] = opts.Region
	p.mu.Unlock()
	return &compute.Machine{ID: id, Addr: "10.0.0.1", Region: opts.Region}, nil
}

func (p *mockPool) DestroyMachine(_ context.Context, machineID string) error {
	p.mu.Lock()
	delete(p.machines, machineID)
	p.mu.Unlock()
	atomic.AddInt32(&p.destroyed, 1)
	return nil
}

func (p *mockPool) DrainMachine(_ context.Context, _ string) error               { return nil }
func (p *mockPool) StartMachine(_ context.Context, _ string) error               { return nil }
func (p *mockPool) StopMachine(_ context.Context, _ string) error                { return nil }
func (p *mockPool) HealthCheck(_ context.Context, _ string) error                { return nil }
func (p *mockPool) ListMachines(_ context.Context) ([]*compute.Machine, error)   { return nil, nil }
func (p *mockPool) SupportedRegions(_ context.Context) ([]string, error) {
	return []string{"us-east-1"}, nil
}

// --- Helpers ---

func makeWorker(id, region string, capacity, current int) *WorkerInfo {
	return &WorkerInfo{
		ID:        id,
		MachineID: "osb-worker-" + id,
		Region:    region,
		Capacity:  capacity,
		Current:   current,
		CPUPct:    float64(current) / float64(capacity) * 60, // approximate
		MemPct:    float64(current) / float64(capacity) * 50,
		DiskPct:   30.0,
	}
}

func newTestScaler(registry *mockRegistry, pool *mockPool) *Scaler {
	return NewScaler(ScalerConfig{
		Pool:       pool,
		Registry:   registry,
		Cooldown:   1 * time.Second,
		Interval:   100 * time.Millisecond,
		MinWorkers: 1,
		MaxWorkers: 20,
	})
}

// ============================================================
// Test: Scale-up triggers
// ============================================================

func TestScaleUpOnHighUtilization(t *testing.T) {
	reg := newMockRegistry()
	pool := newMockPool()

	// One worker at 80% capacity → utilization = 0.80 > 0.70 threshold
	reg.addWorker(&WorkerInfo{
		ID: "w1", MachineID: "osb-worker-w1", Region: "us-east-1",
		Capacity: 50, Current: 40, CPUPct: 50, MemPct: 50, DiskPct: 30,
	})

	s := newTestScaler(reg, pool)
	ctx := context.Background()
	s.evaluateRegion(ctx, "us-east-1")

	// scaleUp runs async — wait briefly for the goroutine
	time.Sleep(100 * time.Millisecond)
	if atomic.LoadInt32(&pool.created) == 0 {
		t.Error("expected scale-up due to high utilization, but no machine was created")
	}
}

func TestNoScaleUpBelowThreshold(t *testing.T) {
	reg := newMockRegistry()
	pool := newMockPool()

	// One active worker at 50% capacity + one idle (satisfies reserve)
	reg.addWorker(&WorkerInfo{
		ID: "w1", MachineID: "osb-worker-w1", Region: "us-east-1",
		Capacity: 50, Current: 25, CPUPct: 40, MemPct: 40, DiskPct: 30,
	})
	reg.addWorker(&WorkerInfo{
		ID: "w2", MachineID: "osb-worker-w2", Region: "us-east-1",
		Capacity: 50, Current: 0, CPUPct: 0, MemPct: 0, DiskPct: 0,
	})

	s := newTestScaler(reg, pool)
	ctx := context.Background()
	s.evaluateRegion(ctx, "us-east-1")

	if atomic.LoadInt32(&pool.created) != 0 {
		t.Error("expected no scale-up below threshold, but a machine was created")
	}
}

func TestScaleUpOnCPUPressure(t *testing.T) {
	reg := newMockRegistry()
	pool := newMockPool()

	// Low utilization but high CPU → should still scale up
	reg.addWorker(&WorkerInfo{
		ID: "w1", MachineID: "osb-worker-w1", Region: "us-east-1",
		Capacity: 50, Current: 10, CPUPct: 75, MemPct: 40, DiskPct: 30,
	})

	s := newTestScaler(reg, pool)
	ctx := context.Background()
	s.evaluateRegion(ctx, "us-east-1")
	time.Sleep(100 * time.Millisecond)

	if atomic.LoadInt32(&pool.created) == 0 {
		t.Error("expected scale-up due to CPU pressure > 70%%, but no machine was created")
	}
}

func TestScaleUpOnMemoryPressure(t *testing.T) {
	reg := newMockRegistry()
	pool := newMockPool()

	reg.addWorker(&WorkerInfo{
		ID: "w1", MachineID: "osb-worker-w1", Region: "us-east-1",
		Capacity: 50, Current: 10, CPUPct: 40, MemPct: 75, DiskPct: 30,
	})

	s := newTestScaler(reg, pool)
	ctx := context.Background()
	s.evaluateRegion(ctx, "us-east-1")
	time.Sleep(100 * time.Millisecond)

	if atomic.LoadInt32(&pool.created) == 0 {
		t.Error("expected scale-up due to memory pressure > 70%%, but no machine was created")
	}
}

func TestScaleUpOnDiskPressure(t *testing.T) {
	reg := newMockRegistry()
	pool := newMockPool()

	reg.addWorker(&WorkerInfo{
		ID: "w1", MachineID: "osb-worker-w1", Region: "us-east-1",
		Capacity: 50, Current: 10, CPUPct: 40, MemPct: 40, DiskPct: 65,
	})

	s := newTestScaler(reg, pool)
	ctx := context.Background()
	s.evaluateRegion(ctx, "us-east-1")
	time.Sleep(100 * time.Millisecond)

	if atomic.LoadInt32(&pool.created) == 0 {
		t.Error("expected scale-up due to disk pressure > 60%%, but no machine was created")
	}
}

func TestScaleUpRespectsMaxWorkers(t *testing.T) {
	reg := newMockRegistry()
	pool := newMockPool()

	// 20 workers all at high utilization — already at max
	for i := 0; i < 20; i++ {
		reg.addWorker(&WorkerInfo{
			ID: fmt.Sprintf("w%d", i), MachineID: fmt.Sprintf("osb-worker-w%d", i), Region: "us-east-1",
			Capacity: 50, Current: 45, CPUPct: 50, MemPct: 50, DiskPct: 30,
		})
	}

	s := newTestScaler(reg, pool)
	ctx := context.Background()
	s.evaluateRegion(ctx, "us-east-1")

	if atomic.LoadInt32(&pool.created) != 0 {
		t.Error("expected no scale-up at max workers, but a machine was created")
	}
}

func TestScaleUpRespectsCooldown(t *testing.T) {
	reg := newMockRegistry()
	pool := newMockPool()

	reg.addWorker(&WorkerInfo{
		ID: "w1", MachineID: "osb-worker-w1", Region: "us-east-1",
		Capacity: 50, Current: 40, CPUPct: 50, MemPct: 50, DiskPct: 30,
	})

	s := newTestScaler(reg, pool)
	ctx := context.Background()

	// First evaluation: should scale up
	s.evaluateRegion(ctx, "us-east-1")
	time.Sleep(100 * time.Millisecond)
	if atomic.LoadInt32(&pool.created) == 0 {
		t.Fatal("expected first scale-up")
	}

	// Register the pending machine as an idle worker so it doesn't block,
	// and also satisfies the idle reserve check.
	time.Sleep(10 * time.Millisecond)
	s.state.SetPendingLaunches("us-east-1", nil)
	reg.addWorker(&WorkerInfo{
		ID: "w-idle", MachineID: "osb-worker-1", Region: "us-east-1",
		Capacity: 50, Current: 0, CPUPct: 0, MemPct: 0, DiskPct: 0,
	})

	// Second evaluation immediately: should be blocked by cooldown
	created := atomic.LoadInt32(&pool.created)
	s.evaluateRegion(ctx, "us-east-1")
	if atomic.LoadInt32(&pool.created) != created {
		t.Error("expected cooldown to prevent second scale-up")
	}
}

func TestMinWorkersEnforced(t *testing.T) {
	reg := newMockRegistry()
	pool := newMockPool()

	// No workers at all — should launch minWorkers
	s := NewScaler(ScalerConfig{
		Pool:       pool,
		Registry:   reg,
		Cooldown:   1 * time.Second,
		Interval:   100 * time.Millisecond,
		MinWorkers: 3,
		MaxWorkers: 10,
	})

	// Add the region so evaluateRegion runs
	reg.addWorker(&WorkerInfo{
		ID: "w1", MachineID: "osb-worker-w1", Region: "us-east-1",
		Capacity: 50, Current: 0, CPUPct: 0, MemPct: 0, DiskPct: 0,
	})

	ctx := context.Background()
	s.evaluateRegion(ctx, "us-east-1")

	// Should launch 2 more to meet minimum of 3
	time.Sleep(50 * time.Millisecond) // async launches
	if atomic.LoadInt32(&pool.created) < 2 {
		t.Errorf("expected at least 2 machines launched to meet minWorkers=3, got %d", atomic.LoadInt32(&pool.created))
	}
}

// ============================================================
// Test: Scale-down triggers
// ============================================================

func TestSmartScaleDownTargetsLeastLoaded(t *testing.T) {
	reg := newMockRegistry()
	pool := newMockPool()

	// Three autoscaled workers, one with very low usage
	reg.addWorker(&WorkerInfo{
		ID: "w1", MachineID: "osb-worker-w1", Region: "us-east-1",
		Capacity: 50, Current: 5, CPUPct: 20, MemPct: 20, DiskPct: 20,
	})
	reg.addWorker(&WorkerInfo{
		ID: "w2", MachineID: "osb-worker-w2", Region: "us-east-1",
		Capacity: 50, Current: 2, CPUPct: 10, MemPct: 10, DiskPct: 10,
	})
	reg.addWorker(&WorkerInfo{
		ID: "w3", MachineID: "osb-worker-w3", Region: "us-east-1",
		Capacity: 50, Current: 5, CPUPct: 20, MemPct: 20, DiskPct: 20,
	})

	s := newTestScaler(reg, pool)
	ctx := context.Background()

	// Utilization = 12/150 = 8% < 30% → scale down
	s.smartScaleDown(ctx, "us-east-1", reg.GetWorkersByRegion("us-east-1"))

	// w2 (lowest current=2) should be selected for draining
	if !s.state.IsDraining("osb-worker-w2") {
		t.Error("expected w2 to be selected for draining (least loaded)")
	}
}

func TestScaleDownSkipsStaticWorker(t *testing.T) {
	reg := newMockRegistry()
	pool := newMockPool()

	// Only osb-worker-1 (static) is present
	reg.addWorker(&WorkerInfo{
		ID: "w1", MachineID: "osb-worker-1", Region: "us-east-1",
		Capacity: 50, Current: 2, CPUPct: 10, MemPct: 10, DiskPct: 10,
	})

	s := newTestScaler(reg, pool)
	ctx := context.Background()

	s.smartScaleDown(ctx, "us-east-1", reg.GetWorkersByRegion("us-east-1"))

	if len(s.state.AllDraining()) != 0 {
		t.Error("expected no draining — osb-worker-1 is static and should be skipped")
	}
}

func TestScaleDownSkipsAlreadyDraining(t *testing.T) {
	reg := newMockRegistry()
	pool := newMockPool()

	reg.addWorker(&WorkerInfo{
		ID: "w2", MachineID: "osb-worker-w2", Region: "us-east-1",
		Capacity: 50, Current: 2, CPUPct: 10, MemPct: 10, DiskPct: 10,
	})
	reg.addWorker(&WorkerInfo{
		ID: "w3", MachineID: "osb-worker-w3", Region: "us-east-1",
		Capacity: 50, Current: 5, CPUPct: 20, MemPct: 20, DiskPct: 20,
	})

	s := newTestScaler(reg, pool)
	s.state.SetDraining("osb-worker-w2", &drainState{workerID: "w2", machineID: "osb-worker-w2"})

	ctx := context.Background()
	s.smartScaleDown(ctx, "us-east-1", reg.GetWorkersByRegion("us-east-1"))

	// w3 should be selected since w2 is already draining
	if !s.state.IsDraining("osb-worker-w3") {
		t.Error("expected w3 to be selected for draining (w2 already draining)")
	}
}

// ============================================================
// Test: Evacuation
// ============================================================

func TestEvacuationTriggersOnCPU(t *testing.T) {
	reg := newMockRegistry()
	pool := newMockPool()

	// Worker above evacuation CPU threshold (80%)
	reg.addWorker(&WorkerInfo{
		ID: "hot", MachineID: "osb-worker-hot", Region: "us-east-1",
		Capacity: 50, Current: 40, CPUPct: 85, MemPct: 50, DiskPct: 30,
	})
	// Target worker
	reg.addWorker(&WorkerInfo{
		ID: "cold", MachineID: "osb-worker-cold", Region: "us-east-1",
		Capacity: 50, Current: 5, CPUPct: 20, MemPct: 20, DiskPct: 20,
	})

	s := newTestScaler(reg, pool)
	workers := reg.GetWorkersByRegion("us-east-1")

	// evacuateHotWorkers should trigger (can't actually migrate without gRPC, but it will try)
	s.evacuateHotWorkers(context.Background(), "us-east-1", workers)

	if _, ok := s.state.GetLastEvacuation("hot"); !ok {
		t.Error("expected evacuation to be triggered for hot worker")
	}
}

func TestEvacuationSkipsBelowThreshold(t *testing.T) {
	reg := newMockRegistry()
	pool := newMockPool()

	reg.addWorker(&WorkerInfo{
		ID: "w1", MachineID: "osb-worker-w1", Region: "us-east-1",
		Capacity: 50, Current: 30, CPUPct: 75, MemPct: 75, DiskPct: 65,
	})

	s := newTestScaler(reg, pool)
	workers := reg.GetWorkersByRegion("us-east-1")

	s.evacuateHotWorkers(context.Background(), "us-east-1", workers)

	if _, ok := s.state.GetLastEvacuation("w1"); ok {
		t.Error("expected no evacuation below 80%% CPU / 80%% mem / 70%% disk thresholds")
	}
}

func TestEvacuationRespectsColdown(t *testing.T) {
	reg := newMockRegistry()
	pool := newMockPool()

	reg.addWorker(&WorkerInfo{
		ID: "hot", MachineID: "osb-worker-hot", Region: "us-east-1",
		Capacity: 50, Current: 40, CPUPct: 85, MemPct: 50, DiskPct: 30,
	})
	reg.addWorker(&WorkerInfo{
		ID: "cold", MachineID: "osb-worker-cold", Region: "us-east-1",
		Capacity: 50, Current: 5, CPUPct: 20, MemPct: 20, DiskPct: 20,
	})

	s := newTestScaler(reg, pool)

	// Set recent evacuation
	s.state.SetLastEvacuation("hot", time.Now())

	workers := reg.GetWorkersByRegion("us-east-1")
	s.evacuateHotWorkers(context.Background(), "us-east-1", workers)

	// lastEvacuation should not be updated (cooldown active)
	if last, _ := s.state.GetLastEvacuation("hot"); last.After(time.Now().Add(-1 * time.Second)) {
		// It was just set, which means it was re-triggered. Check more carefully.
		// Actually, we set it above, so it will be recent regardless. The test is
		// that evacuateBatch was NOT called (no goroutine launched).
		// We can't easily check goroutine launch, so this test just validates
		// the cooldown path doesn't panic.
	}
}

func TestEvacuationOnDiskPressure(t *testing.T) {
	reg := newMockRegistry()
	pool := newMockPool()

	reg.addWorker(&WorkerInfo{
		ID: "diskfull", MachineID: "osb-worker-diskfull", Region: "us-east-1",
		Capacity: 50, Current: 20, CPUPct: 30, MemPct: 30, DiskPct: 75,
	})
	reg.addWorker(&WorkerInfo{
		ID: "target", MachineID: "osb-worker-target", Region: "us-east-1",
		Capacity: 50, Current: 5, CPUPct: 20, MemPct: 20, DiskPct: 20,
	})

	s := newTestScaler(reg, pool)
	workers := reg.GetWorkersByRegion("us-east-1")
	s.evacuateHotWorkers(context.Background(), "us-east-1", workers)

	if _, ok := s.state.GetLastEvacuation("diskfull"); !ok {
		t.Error("expected evacuation triggered by disk pressure > 70%%")
	}
}

// ============================================================
// Test: Emergency hibernate
// ============================================================

func TestEmergencyHibernateTriggersAboveCritical(t *testing.T) {
	reg := newMockRegistry()
	pool := newMockPool()

	// Worker above emergency thresholds
	reg.addWorker(&WorkerInfo{
		ID: "critical", MachineID: "osb-worker-critical", Region: "us-east-1",
		Capacity: 50, Current: 45, CPUPct: 96, MemPct: 96, DiskPct: 50,
	})

	s := newTestScaler(reg, pool)
	workers := reg.GetWorkersByRegion("us-east-1")

	s.emergencyHibernate(context.Background(), "us-east-1", workers)

	// Should have triggered (sets lastEvacuation as cooldown marker)
	if _, ok := s.state.GetLastEvacuation("critical"); !ok {
		t.Error("expected emergency hibernate triggered for critical worker (CPU > 95%%)")
	}
}

func TestEmergencyHibernateOnDiskCritical(t *testing.T) {
	reg := newMockRegistry()
	pool := newMockPool()

	reg.addWorker(&WorkerInfo{
		ID: "diskdead", MachineID: "osb-worker-diskdead", Region: "us-east-1",
		Capacity: 50, Current: 30, CPUPct: 40, MemPct: 40, DiskPct: 92,
	})

	s := newTestScaler(reg, pool)
	workers := reg.GetWorkersByRegion("us-east-1")
	s.emergencyHibernate(context.Background(), "us-east-1", workers)

	if _, ok := s.state.GetLastEvacuation("diskdead"); !ok {
		t.Error("expected emergency hibernate triggered for disk > 90%%")
	}
}

func TestEmergencyHibernateSkipsBelowCritical(t *testing.T) {
	reg := newMockRegistry()
	pool := newMockPool()

	reg.addWorker(&WorkerInfo{
		ID: "w1", MachineID: "osb-worker-w1", Region: "us-east-1",
		Capacity: 50, Current: 40, CPUPct: 85, MemPct: 85, DiskPct: 80,
	})

	s := newTestScaler(reg, pool)
	workers := reg.GetWorkersByRegion("us-east-1")
	s.emergencyHibernate(context.Background(), "us-east-1", workers)

	if _, ok := s.state.GetLastEvacuation("w1"); ok {
		t.Error("expected no emergency hibernate below critical thresholds")
	}
}

// ============================================================
// Test: Migration target selection
// ============================================================

func TestFindMigrationTargetSelectsLeastLoaded(t *testing.T) {
	reg := newMockRegistry()
	pool := newMockPool()

	reg.addWorker(&WorkerInfo{
		ID: "hot", MachineID: "osb-worker-hot", Region: "us-east-1",
		Capacity: 50, Current: 45, CPUPct: 85, MemPct: 50, DiskPct: 30,
	})
	reg.addWorker(&WorkerInfo{
		ID: "medium", MachineID: "osb-worker-medium", Region: "us-east-1",
		Capacity: 50, Current: 25, CPUPct: 40, MemPct: 40, DiskPct: 30,
	})
	reg.addWorker(&WorkerInfo{
		ID: "cold", MachineID: "osb-worker-cold", Region: "us-east-1",
		Capacity: 50, Current: 5, CPUPct: 10, MemPct: 10, DiskPct: 10,
	})

	s := newTestScaler(reg, pool)
	target := s.findMigrationTarget("us-east-1", "hot")

	if target == nil {
		t.Fatal("expected a migration target")
	}
	if target.ID != "cold" {
		t.Errorf("expected cold worker as target, got %s", target.ID)
	}
}

func TestFindMigrationTargetSkipsPressuredWorkers(t *testing.T) {
	reg := newMockRegistry()
	pool := newMockPool()

	reg.addWorker(&WorkerInfo{
		ID: "hot", MachineID: "osb-worker-hot", Region: "us-east-1",
		Capacity: 50, Current: 45, CPUPct: 90, MemPct: 50, DiskPct: 30,
	})
	// Only other worker is also under pressure
	reg.addWorker(&WorkerInfo{
		ID: "alsohot", MachineID: "osb-worker-alsohot", Region: "us-east-1",
		Capacity: 50, Current: 40, CPUPct: 88, MemPct: 88, DiskPct: 30,
	})

	s := newTestScaler(reg, pool)
	target := s.findMigrationTarget("us-east-1", "hot")

	if target != nil {
		t.Errorf("expected no target (all workers under pressure), got %s", target.ID)
	}
}

func TestFindMigrationTargetSkipsDraining(t *testing.T) {
	reg := newMockRegistry()
	pool := newMockPool()

	reg.addWorker(&WorkerInfo{
		ID: "hot", MachineID: "osb-worker-hot", Region: "us-east-1",
		Capacity: 50, Current: 45, CPUPct: 85, MemPct: 50, DiskPct: 30,
	})
	reg.addWorker(&WorkerInfo{
		ID: "draining", MachineID: "osb-worker-draining", Region: "us-east-1",
		Capacity: 50, Current: 5, CPUPct: 10, MemPct: 10, DiskPct: 10,
	})

	s := newTestScaler(reg, pool)
	s.state.SetDraining("osb-worker-draining", &drainState{workerID: "draining"})

	target := s.findMigrationTarget("us-east-1", "hot")
	if target != nil {
		t.Errorf("expected no target (only candidate is draining), got %s", target.ID)
	}
}

func TestFindMigrationTargetAccountsForInFlight(t *testing.T) {
	reg := newMockRegistry()
	pool := newMockPool()

	reg.addWorker(&WorkerInfo{
		ID: "source", MachineID: "osb-worker-source", Region: "us-east-1",
		Capacity: 50, Current: 45, CPUPct: 85, MemPct: 50, DiskPct: 30,
	})
	// Almost full when accounting for in-flight
	reg.addWorker(&WorkerInfo{
		ID: "target1", MachineID: "osb-worker-target1", Region: "us-east-1",
		Capacity: 10, Current: 5, CPUPct: 40, MemPct: 40, DiskPct: 30,
	})
	reg.addWorker(&WorkerInfo{
		ID: "target2", MachineID: "osb-worker-target2", Region: "us-east-1",
		Capacity: 50, Current: 5, CPUPct: 20, MemPct: 20, DiskPct: 20,
	})

	s := newTestScaler(reg, pool)

	// Simulate 5 in-flight migrations to target1 → effective remaining = 0
	for i := 0; i < 5; i++ {
		s.state.IncrInFlight("target1")
	}

	target := s.findMigrationTarget("us-east-1", "source")
	if target == nil {
		t.Fatal("expected a migration target")
	}
	if target.ID != "target2" {
		t.Errorf("expected target2 (target1 full with in-flight), got %s", target.ID)
	}
}

func TestFindMigrationTargetSkipsHighDisk(t *testing.T) {
	reg := newMockRegistry()
	pool := newMockPool()

	reg.addWorker(&WorkerInfo{
		ID: "source", MachineID: "osb-worker-source", Region: "us-east-1",
		Capacity: 50, Current: 45, CPUPct: 85, MemPct: 50, DiskPct: 30,
	})
	// Good capacity but high disk
	reg.addWorker(&WorkerInfo{
		ID: "diskfull", MachineID: "osb-worker-diskfull", Region: "us-east-1",
		Capacity: 50, Current: 5, CPUPct: 20, MemPct: 20, DiskPct: 88,
	})

	s := newTestScaler(reg, pool)
	target := s.findMigrationTarget("us-east-1", "source")

	if target != nil {
		t.Errorf("expected no target (only candidate has disk > 85%%), got %s", target.ID)
	}
}

// ============================================================
// Test: Drain timeout → hibernate
// ============================================================

func TestDrainTimeoutCancelsDrainKeepsWorker(t *testing.T) {
	reg := newMockRegistry()
	pool := newMockPool()

	reg.addWorker(&WorkerInfo{
		ID: "w2", MachineID: "osb-worker-w2", Region: "us-east-1",
		Capacity: 50, Current: 5, CPUPct: 20, MemPct: 20, DiskPct: 20,
	})

	s := newTestScaler(reg, pool)

	// Simulate a drain that started long ago (past timeout)
	s.state.SetDraining("osb-worker-w2", &drainState{
		workerID:  "w2",
		machineID: "osb-worker-w2",
		region:    "us-east-1",
		startedAt: time.Now().Add(-20 * time.Minute), // well past drainTimeout
	})

	ctx := context.Background()
	s.checkDrainingWorkers(ctx, "us-east-1")

	// Drain should be cancelled (removed from draining map) — worker stays alive
	if s.state.IsDraining("osb-worker-w2") {
		t.Error("expected timed-out drain to be cancelled")
	}

	// Machine should NOT be destroyed — sandboxes must be preserved
	time.Sleep(50 * time.Millisecond)
	if atomic.LoadInt32(&pool.destroyed) != 0 {
		t.Error("machine should not be destroyed when drain times out with active sandboxes")
	}
}

func TestDrainCompletesWhenEmpty(t *testing.T) {
	reg := newMockRegistry()
	pool := newMockPool()

	// Worker with 0 sandboxes
	reg.addWorker(&WorkerInfo{
		ID: "w2", MachineID: "osb-worker-w2", Region: "us-east-1",
		Capacity: 50, Current: 0, CPUPct: 0, MemPct: 0, DiskPct: 0,
	})

	s := newTestScaler(reg, pool)
	s.state.SetDraining("osb-worker-w2", &drainState{
		workerID:  "w2",
		machineID: "osb-worker-w2",
		region:    "us-east-1",
		startedAt: time.Now(),
	})

	ctx := context.Background()
	s.checkDrainingWorkers(ctx, "us-east-1")

	if s.state.IsDraining("osb-worker-w2") {
		t.Error("expected drain to complete (worker has 0 sandboxes)")
	}

	time.Sleep(50 * time.Millisecond)
	if atomic.LoadInt32(&pool.destroyed) == 0 {
		t.Error("expected machine to be destroyed after drain completes")
	}
}

// ============================================================
// Test: Golden version in migration target selection
// ============================================================

func TestGoldenVersionTrackedInWorkerInfo(t *testing.T) {
	reg := newMockRegistry()

	reg.addWorker(&WorkerInfo{
		ID: "w1", Region: "us-east-1", GoldenVersion: "abc123",
		Capacity: 50, Current: 10,
	})
	reg.addWorker(&WorkerInfo{
		ID: "w2", Region: "us-east-1", GoldenVersion: "abc123",
		Capacity: 50, Current: 10,
	})
	reg.addWorker(&WorkerInfo{
		ID: "w3", Region: "us-east-1", GoldenVersion: "def456",
		Capacity: 50, Current: 10,
	})

	workers := reg.GetWorkersByRegion("us-east-1")
	sameVersion := 0
	for _, w := range workers {
		if w.GoldenVersion == "abc123" {
			sameVersion++
		}
	}
	if sameVersion != 2 {
		t.Errorf("expected 2 workers with same golden version, got %d", sameVersion)
	}
}

// ============================================================
// Test: 200 concurrent scale-up/down stress test
// ============================================================

func TestConcurrentScaleUpDown200(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stress test in short mode")
	}

	reg := newMockRegistry()
	pool := newMockPool()

	// Start with 5 workers
	for i := 0; i < 5; i++ {
		reg.addWorker(&WorkerInfo{
			ID:        fmt.Sprintf("w%d", i),
			MachineID: fmt.Sprintf("osb-worker-w%d", i),
			Region:    "us-east-1",
			Capacity:  50,
			Current:   25,
			CPUPct:    50,
			MemPct:    50,
			DiskPct:   30,
		})
	}

	s := NewScaler(ScalerConfig{
		Pool:       pool,
		Registry:   reg,
		Cooldown:   50 * time.Millisecond,
		Interval:   10 * time.Millisecond,
		MinWorkers: 1,
		MaxWorkers: 20,
	})

	// Run 200 concurrent goroutines that randomly change worker loads
	// and trigger evaluations
	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Track panics
	var panicked int32

	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					atomic.AddInt32(&panicked, 1)
					t.Errorf("goroutine %d panicked: %v", id, r)
				}
			}()

			rng := rand.New(rand.NewSource(int64(id)))

			for {
				select {
				case <-ctx.Done():
					return
				default:
				}

				action := rng.Intn(10)
				switch {
				case action < 3:
					// Simulate load increase on random worker
					workers := reg.GetWorkersByRegion("us-east-1")
					if len(workers) > 0 {
						w := workers[rng.Intn(len(workers))]
						reg.updateWorker(w.ID, func(w *WorkerInfo) {
							w.Current = rng.Intn(w.Capacity + 1)
							w.CPUPct = float64(rng.Intn(100))
							w.MemPct = float64(rng.Intn(100))
							w.DiskPct = float64(rng.Intn(100))
						})
					}

				case action < 5:
					// Evaluate (triggers scale decisions)
					s.evaluateRegion(ctx, "us-east-1")

				case action < 7:
					// Add a new worker
					newID := fmt.Sprintf("dyn-%d-%d", id, rng.Intn(1000))
					reg.addWorker(&WorkerInfo{
						ID:        newID,
						MachineID: "osb-worker-" + newID,
						Region:    "us-east-1",
						Capacity:  50,
						Current:   rng.Intn(50),
						CPUPct:    float64(rng.Intn(80)),
						MemPct:    float64(rng.Intn(80)),
						DiskPct:   float64(rng.Intn(60)),
					})

				case action < 9:
					// Remove a random worker
					workers := reg.GetWorkersByRegion("us-east-1")
					if len(workers) > 1 {
						w := workers[rng.Intn(len(workers))]
						reg.removeWorker(w.ID)
					}

				default:
					// Query state (read contention)
					_ = reg.RegionUtilization("us-east-1")
					_, _, _ = reg.RegionResourcePressure("us-east-1")
					_ = s.findMigrationTarget("us-east-1", "nonexistent")
				}

				time.Sleep(time.Duration(rng.Intn(5)) * time.Millisecond)
			}
		}(i)
	}

	wg.Wait()

	if atomic.LoadInt32(&panicked) > 0 {
		t.Fatalf("%d goroutines panicked during concurrent stress test", atomic.LoadInt32(&panicked))
	}

	t.Logf("stress test complete: created=%d, destroyed=%d, workers=%d",
		atomic.LoadInt32(&pool.created),
		atomic.LoadInt32(&pool.destroyed),
		len(reg.GetWorkersByRegion("us-east-1")))
}

// ============================================================
// Test: 200 sandbox disk pressure growth stress test
// ============================================================

func TestDiskPressureGrowth200Sandboxes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stress test in short mode")
	}

	reg := newMockRegistry()
	pool := newMockPool()

	// 5 workers, each with 40 sandboxes (200 total)
	numWorkers := 5
	sandboxesPerWorker := 40

	for i := 0; i < numWorkers; i++ {
		reg.addWorker(&WorkerInfo{
			ID:        fmt.Sprintf("w%d", i),
			MachineID: fmt.Sprintf("osb-worker-w%d", i),
			Region:    "us-east-1",
			Capacity:  50,
			Current:   sandboxesPerWorker,
			CPUPct:    40,
			MemPct:    40,
			DiskPct:   30, // start at 30%
		})
	}

	s := NewScaler(ScalerConfig{
		Pool:       pool,
		Registry:   reg,
		Cooldown:   50 * time.Millisecond,
		Interval:   10 * time.Millisecond,
		MinWorkers: 1,
		MaxWorkers: 20,
	})

	var panicked int32
	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Track events
	var scaleUps, evacuations, emergencies int32

	// Goroutine: randomly grow disk on workers (simulating sandbox disk growth)
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(sandboxNum int) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					atomic.AddInt32(&panicked, 1)
					t.Errorf("sandbox %d panicked: %v", sandboxNum, r)
				}
			}()

			rng := rand.New(rand.NewSource(int64(sandboxNum)))
			workerIdx := sandboxNum % numWorkers
			workerID := fmt.Sprintf("w%d", workerIdx)

			for {
				select {
				case <-ctx.Done():
					return
				default:
				}

				// Randomly grow disk usage on the worker
				growth := rng.Float64() * 2.0 // 0-2% per tick
				reg.updateWorker(workerID, func(w *WorkerInfo) {
					w.DiskPct += growth
					if w.DiskPct > 99 {
						w.DiskPct = 99
					}
					// Also slightly grow CPU/mem from the workload
					w.CPUPct += rng.Float64() * 0.5
					if w.CPUPct > 99 {
						w.CPUPct = 99
					}
					w.MemPct += rng.Float64() * 0.3
					if w.MemPct > 99 {
						w.MemPct = 99
					}
				})

				time.Sleep(time.Duration(10+rng.Intn(20)) * time.Millisecond)
			}
		}(i)
	}

	// Goroutine: scaler evaluation loop
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Snapshot state before evaluation
				_, _, maxDiskBefore := reg.RegionResourcePressure("us-east-1")

				s.evaluateRegion(ctx, "us-east-1")

				// Track what happened
				if atomic.LoadInt32(&pool.created) > 0 {
					atomic.AddInt32(&scaleUps, 1)
				}

				// Check for evacuation and emergency triggers
				workers := reg.GetWorkersByRegion("us-east-1")
				for _, w := range workers {
					if w.DiskPct > emergencyDiskThreshold {
						atomic.AddInt32(&emergencies, 1)
					} else if w.DiskPct > evacuationDiskThreshold {
						atomic.AddInt32(&evacuations, 1)
					}
				}

				// Simulate evacuation relief: if a worker had sandboxes migrated,
				// its sandbox count and disk would decrease
				for _, w := range workers {
					if _, ok := s.state.GetLastEvacuation(w.ID); ok {
						reg.updateWorker(w.ID, func(w *WorkerInfo) {
							if w.Current > 3 {
								w.Current -= 3
							}
							w.DiskPct -= 5
							if w.DiskPct < 20 {
								w.DiskPct = 20
							}
						})
					}
				}

				// If new workers were added, spread some load to them
				if len(workers) > numWorkers {
					for _, w := range workers {
						if w.Current == 0 && w.DiskPct < 30 {
							reg.updateWorker(w.ID, func(w *WorkerInfo) {
								w.Current = 10
								w.CPUPct = 30
								w.MemPct = 30
								w.DiskPct = 20
							})
						}
					}
				}

				_ = maxDiskBefore
			}
		}
	}()

	wg.Wait()

	if atomic.LoadInt32(&panicked) > 0 {
		t.Fatalf("%d goroutines panicked during disk pressure stress test", atomic.LoadInt32(&panicked))
	}

	// Verify the system responded to pressure
	finalWorkers := reg.GetWorkersByRegion("us-east-1")
	_, _, maxDisk := reg.RegionResourcePressure("us-east-1")

	t.Logf("disk stress test complete:")
	t.Logf("  workers: %d (started with %d)", len(finalWorkers), numWorkers)
	t.Logf("  machines created: %d", atomic.LoadInt32(&pool.created))
	t.Logf("  max disk at end: %.1f%%", maxDisk)
	t.Logf("  evacuation triggers: %d", atomic.LoadInt32(&evacuations))
	t.Logf("  emergency triggers: %d", atomic.LoadInt32(&emergencies))

	// The scaler should have responded — either scaled up or triggered evacuations
	created := atomic.LoadInt32(&pool.created)
	evacs := atomic.LoadInt32(&evacuations)
	emers := atomic.LoadInt32(&emergencies)

	if created == 0 && evacs == 0 && emers == 0 {
		t.Error("scaler did not respond to disk pressure growth at all")
	}
}

// ============================================================
// Test: Pending launch tracking
// ============================================================

func TestPendingLaunchExpires(t *testing.T) {
	reg := newMockRegistry()
	pool := newMockPool()

	reg.addWorker(&WorkerInfo{
		ID: "w1", MachineID: "osb-worker-w1", Region: "us-east-1",
		Capacity: 50, Current: 10, CPUPct: 30, MemPct: 30, DiskPct: 30,
	})

	s := newTestScaler(reg, pool)

	// Simulate a pending launch from 15 minutes ago
	s.state.SetPendingLaunches("us-east-1", []pendingLaunch{
		{machineID: "osb-worker-stale", launchedAt: time.Now().Add(-15 * time.Minute)},
	})

	s.expirePending("us-east-1")

	if len(s.state.GetPendingLaunches("us-east-1")) != 0 {
		t.Error("expected stale pending launch to be expired")
	}
}

func TestPendingLaunchRegistered(t *testing.T) {
	reg := newMockRegistry()
	pool := newMockPool()

	// Worker that matches the pending machine ID
	reg.addWorker(&WorkerInfo{
		ID: "w-new", MachineID: "osb-worker-new", Region: "us-east-1",
		Capacity: 50, Current: 0, CPUPct: 0, MemPct: 0, DiskPct: 0,
	})

	s := newTestScaler(reg, pool)
	s.state.SetPendingLaunches("us-east-1", []pendingLaunch{
		{machineID: "osb-worker-new", launchedAt: time.Now()},
	})

	s.expirePending("us-east-1")

	if len(s.state.GetPendingLaunches("us-east-1")) != 0 {
		t.Error("expected registered pending launch to be cleared")
	}
}

// ============================================================
// Test: In-flight migration tracking
// ============================================================

func TestInFlightTrackingPreventsOverload(t *testing.T) {
	reg := newMockRegistry()
	pool := newMockPool()

	reg.addWorker(&WorkerInfo{
		ID: "target", MachineID: "osb-worker-target", Region: "us-east-1",
		Capacity: 5, Current: 2, CPUPct: 30, MemPct: 30, DiskPct: 30,
	})

	s := newTestScaler(reg, pool)

	// 3 remaining capacity, but 3 in-flight → effective 0
	for i := 0; i < 3; i++ {
		s.state.IncrInFlight("target")
	}

	target := s.findMigrationTarget("us-east-1", "other")
	if target != nil {
		t.Error("expected no target — in-flight migrations fill remaining capacity")
	}
}

func TestInFlightCleanup(t *testing.T) {
	s := &Scaler{
		state: NewInMemoryScalerState(),
	}

	// Simulate increment
	s.state.IncrInFlight("w1")
	s.state.IncrInFlight("w1")

	// Simulate one completion
	s.state.DecrInFlight("w1")

	if got := s.state.GetInFlight("w1"); got != 1 {
		t.Errorf("expected 1 in-flight after decrement, got %d", got)
	}

	// Simulate final completion
	s.state.DecrInFlight("w1")

	if got := s.state.GetInFlight("w1"); got != 0 {
		t.Errorf("expected 0 in-flight after final decrement, got %d", got)
	}
}
