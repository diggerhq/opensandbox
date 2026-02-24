package controlplane

import (
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

// WorkerInfo represents a registered worker.
type WorkerInfo struct {
	ID           string    `json:"worker_id"`
	MachineID    string    `json:"machine_id,omitempty"` // EC2 instance ID
	Region       string    `json:"region"`
	GRPCAddr     string    `json:"grpc_addr"`
	HTTPAddr     string    `json:"http_addr"`
	Capacity     int       `json:"capacity"`
	Current      int       `json:"current"`
	CPUPct       float64   `json:"cpu_pct"`
	MemPct       float64   `json:"mem_pct"`
	LastSeen     time.Time `json:"-"`
	MissedBeats  int       `json:"-"`
}

// WorkerRegistry tracks live workers from NATS heartbeats.
type WorkerRegistry struct {
	mu      sync.RWMutex
	workers map[string]*WorkerInfo // worker ID -> info
	nc      *nats.Conn
	sub     *nats.Subscription
	stop    chan struct{}

	onWorkerDown func(workerID string) // callback when worker is marked down
}

// NewWorkerRegistry creates a worker registry that subscribes to NATS heartbeats.
func NewWorkerRegistry(natsURL string) (*WorkerRegistry, error) {
	nc, err := nats.Connect(natsURL,
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
	)
	if err != nil {
		return nil, err
	}

	return &WorkerRegistry{
		workers: make(map[string]*WorkerInfo),
		nc:      nc,
		stop:    make(chan struct{}),
	}, nil
}

// OnWorkerDown sets a callback for when a worker is detected as down.
func (r *WorkerRegistry) OnWorkerDown(fn func(workerID string)) {
	r.onWorkerDown = fn
}

// Start begins listening for heartbeats and checking for stale workers.
func (r *WorkerRegistry) Start() error {
	sub, err := r.nc.Subscribe("workers.heartbeat.>", r.handleHeartbeat)
	if err != nil {
		return err
	}
	r.sub = sub

	// Stale worker checker
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				r.checkStaleWorkers()
			case <-r.stop:
				return
			}
		}
	}()

	log.Println("worker_registry: listening for heartbeats")
	return nil
}

// Stop stops the registry.
func (r *WorkerRegistry) Stop() {
	close(r.stop)
	if r.sub != nil {
		r.sub.Unsubscribe()
	}
	r.nc.Close()
}

// GetWorkersByRegion returns healthy workers in a region, sorted by available capacity.
func (r *WorkerRegistry) GetWorkersByRegion(region string) []*WorkerInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result []*WorkerInfo
	for _, w := range r.workers {
		if w.Region == region && w.MissedBeats < 3 {
			result = append(result, w)
		}
	}
	return result
}

// GetLeastLoadedWorker returns the worker with the most remaining capacity in a region.
func (r *WorkerRegistry) GetLeastLoadedWorker(region string) *WorkerInfo {
	workers := r.GetWorkersByRegion(region)
	if len(workers) == 0 {
		return nil
	}

	var best *WorkerInfo
	for _, w := range workers {
		remaining := w.Capacity - w.Current
		if best == nil || remaining > (best.Capacity-best.Current) {
			best = w
		}
	}
	return best
}

// GetAllWorkers returns all known workers.
func (r *WorkerRegistry) GetAllWorkers() []*WorkerInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]*WorkerInfo, 0, len(r.workers))
	for _, w := range r.workers {
		result = append(result, w)
	}
	return result
}

// GetWorker returns info for a specific worker.
func (r *WorkerRegistry) GetWorker(workerID string) *WorkerInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.workers[workerID]
}

// RegionUtilization returns the average utilization for a region.
func (r *WorkerRegistry) RegionUtilization(region string) float64 {
	workers := r.GetWorkersByRegion(region)
	if len(workers) == 0 {
		return 0
	}

	var totalCap, totalCur int
	for _, w := range workers {
		totalCap += w.Capacity
		totalCur += w.Current
	}
	if totalCap == 0 {
		return 0
	}
	return float64(totalCur) / float64(totalCap)
}

// Regions returns all known regions.
func (r *WorkerRegistry) Regions() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	regionSet := make(map[string]struct{})
	for _, w := range r.workers {
		regionSet[w.Region] = struct{}{}
	}

	regions := make([]string, 0, len(regionSet))
	for region := range regionSet {
		regions = append(regions, region)
	}
	return regions
}

func (r *WorkerRegistry) handleHeartbeat(msg *nats.Msg) {
	var hb WorkerInfo
	if err := json.Unmarshal(msg.Data, &hb); err != nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	existing, ok := r.workers[hb.ID]
	if ok {
		existing.Current = hb.Current
		existing.Capacity = hb.Capacity
		existing.CPUPct = hb.CPUPct
		existing.MemPct = hb.MemPct
		existing.LastSeen = time.Now()
		existing.MissedBeats = 0
		if hb.GRPCAddr != "" {
			existing.GRPCAddr = hb.GRPCAddr
		}
		if hb.HTTPAddr != "" {
			existing.HTTPAddr = hb.HTTPAddr
		}
		if hb.MachineID != "" {
			existing.MachineID = hb.MachineID
		}
	} else {
		hb.LastSeen = time.Now()
		r.workers[hb.ID] = &hb
		log.Printf("worker_registry: new worker registered: %s (region=%s)", hb.ID, hb.Region)
	}
}

func (r *WorkerRegistry) checkStaleWorkers() {
	r.mu.Lock()
	defer r.mu.Unlock()

	for id, w := range r.workers {
		if time.Since(w.LastSeen) > 15*time.Second {
			w.MissedBeats++
			if w.MissedBeats >= 3 {
				log.Printf("worker_registry: worker %s marked as down (missed %d heartbeats)", id, w.MissedBeats)
				delete(r.workers, id)
				if r.onWorkerDown != nil {
					go r.onWorkerDown(id)
				}
			}
		}
	}
}
