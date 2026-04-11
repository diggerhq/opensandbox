// redis_registry.go — Worker discovery and health tracking via Redis.
//
// Workers send heartbeats to Redis every 10s (key: worker:{id}, TTL: 30s).
// The control plane discovers workers through two mechanisms:
//
//  1. Pub/sub (fast path): Workers publish heartbeats to "workers:heartbeat".
//     The registry subscribes and updates its in-memory cache immediately.
//     This provides sub-second worker discovery.
//
//  2. Periodic SCAN (reconciliation): Every 10s, the registry scans all
//     worker:* keys in Redis. Workers whose keys have expired (TTL elapsed)
//     are removed. This handles cases where pub/sub messages are lost.
//
// For each discovered worker, the registry maintains a gRPC connection pool
// (see grpc_pool.go) used to dispatch sandbox operations.
package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/connectivity"

	pb "github.com/opensandbox/opensandbox/proto/worker"
)

// WorkerEntry represents a worker's current state as reported via heartbeat.
// Updated on every heartbeat (every 10s) with the latest load metrics.
type WorkerEntry struct {
	ID        string  `json:"worker_id"`
	MachineID string  `json:"machine_id,omitempty"`
	Region    string  `json:"region"`
	GRPCAddr  string  `json:"grpc_addr"`
	HTTPAddr  string  `json:"http_addr"`
	Capacity  int     `json:"capacity"` // max sandboxes this worker can run
	Current   int     `json:"current"`  // sandboxes currently running
	CPUPct    float64 `json:"cpu_pct"`  // current CPU usage percentage
	MemPct    float64 `json:"mem_pct"`  // current memory usage percentage
}

// RedisWorkerRegistry maintains an in-memory cache of known workers,
// backed by Redis for discovery, and a gRPC connection pool per worker
// for dispatching sandbox operations.
type RedisWorkerRegistry struct {
	rdb     *redis.Client
	mu      sync.RWMutex
	workers map[string]*WorkerEntry // workerID → latest state
	pools   map[string]*grpcPool    // workerID → gRPC connection pool
	stop    chan struct{}
}

// NewRedisWorkerRegistry connects to Redis and returns a new registry.
// Call Start() to begin worker discovery.
func NewRedisWorkerRegistry(redisURL string) (*RedisWorkerRegistry, error) {
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("invalid redis URL: %w", err)
	}

	rdb := redis.NewClient(opts)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		rdb.Close()
		return nil, fmt.Errorf("redis ping failed: %w", err)
	}

	return &RedisWorkerRegistry{
		rdb:     rdb,
		workers: make(map[string]*WorkerEntry),
		pools:   make(map[string]*grpcPool),
		stop:    make(chan struct{}),
	}, nil
}

// Start begins worker discovery via pub/sub and periodic reconciliation.
func (r *RedisWorkerRegistry) Start() {
	go r.subscribeLoop()
	go r.reconcileLoop()
}

// subscribeLoop listens for worker heartbeats via Redis pub/sub.
// Provides fast (~millisecond) discovery of new workers and load updates.
// Automatically reconnects if the pub/sub channel drops.
func (r *RedisWorkerRegistry) subscribeLoop() {
	for {
		select {
		case <-r.stop:
			return
		default:
		}

		pubsub := r.rdb.Subscribe(context.Background(), "workers:heartbeat")
		ch := pubsub.Channel()

		for {
			select {
			case msg, ok := <-ch:
				if !ok {
					goto reconnect
				}
				var entry WorkerEntry
				if err := json.Unmarshal([]byte(msg.Payload), &entry); err != nil {
					log.Printf("redis_registry: invalid heartbeat payload: %v", err)
					continue
				}
				r.handleHeartbeat(entry)
			case <-r.stop:
				pubsub.Close()
				return
			}
		}

	reconnect:
		pubsub.Close()
		log.Println("redis_registry: pub/sub channel closed, reconnecting...")
		time.Sleep(2 * time.Second)
	}
}

// reconcileLoop periodically scans Redis for worker:* keys and prunes stale workers.
// Runs every 10s as a safety net — if a pub/sub heartbeat is missed, the SCAN
// will still discover/remove workers within one cycle.
func (r *RedisWorkerRegistry) reconcileLoop() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	// Initial scan on startup so workers are available immediately.
	r.reconcileAndPrune()

	for {
		select {
		case <-ticker.C:
			r.reconcileAndPrune()
		case <-r.stop:
			return
		}
	}
}

// reconcileAndPrune scans all worker:* keys in Redis, updates the in-memory
// cache, and removes workers whose heartbeat keys have expired (worker is down).
func (r *RedisWorkerRegistry) reconcileAndPrune() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var cursor uint64
	seen := make(map[string]bool)

	for {
		keys, nextCursor, err := r.rdb.Scan(ctx, cursor, "worker:*", 100).Result()
		if err != nil {
			log.Printf("redis_registry: SCAN failed: %v", err)
			return
		}

		for _, key := range keys {
			val, err := r.rdb.Get(ctx, key).Result()
			if err != nil {
				continue
			}
			var entry WorkerEntry
			if err := json.Unmarshal([]byte(val), &entry); err != nil {
				continue
			}
			seen[entry.ID] = true
			r.handleHeartbeat(entry)
		}

		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}

	// Workers not found in Redis have stopped sending heartbeats — remove them.
	r.mu.Lock()
	defer r.mu.Unlock()
	for id := range r.workers {
		if !seen[id] {
			log.Printf("redis_registry: worker %s no longer in Redis, removing", id)
			delete(r.workers, id)
			if pool, ok := r.pools[id]; ok {
				pool.close()
				delete(r.pools, id)
			}
		}
	}
}

// handleHeartbeat processes a worker heartbeat: updates the in-memory cache
// and ensures a healthy gRPC connection pool exists for this worker.
func (r *RedisWorkerRegistry) handleHeartbeat(entry WorkerEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()

	existing, ok := r.workers[entry.ID]
	if ok {
		// Update load metrics for existing worker.
		existing.Current = entry.Current
		existing.Capacity = entry.Capacity
		existing.CPUPct = entry.CPUPct
		existing.MemPct = entry.MemPct
		if entry.GRPCAddr != "" {
			existing.GRPCAddr = entry.GRPCAddr
		}
		if entry.HTTPAddr != "" {
			existing.HTTPAddr = entry.HTTPAddr
		}
		if entry.MachineID != "" {
			existing.MachineID = entry.MachineID
		}
	} else {
		// First time seeing this worker — add to cache.
		r.workers[entry.ID] = &entry
		log.Printf("redis_registry: new worker registered: %s (region=%s, grpc=%s)", entry.ID, entry.Region, entry.GRPCAddr)
	}

	// Ensure a gRPC connection pool exists and is healthy.
	// Re-create the pool if the worker's gRPC address changed (e.g. new IP after
	// restart) or if the primary connection entered a failed state.
	if entry.GRPCAddr != "" {
		pool, hasPool := r.pools[entry.ID]
		if hasPool {
			addrChanged := existing != nil && existing.GRPCAddr != entry.GRPCAddr
			state := pool.conns[0].GetState()
			connDead := state == connectivity.TransientFailure || state == connectivity.Shutdown
			if addrChanged || connDead {
				if addrChanged {
					log.Printf("redis_registry: worker %s gRPC address changed (%s → %s), re-dialing",
						entry.ID, existing.GRPCAddr, entry.GRPCAddr)
				} else {
					log.Printf("redis_registry: worker %s gRPC connection in %s state, re-dialing",
						entry.ID, state.String())
				}
				pool.close()
				delete(r.pools, entry.ID)
				hasPool = false
			}
		}
		if !hasPool {
			capacity := entry.Capacity
			if w, ok := r.workers[entry.ID]; ok {
				capacity = w.Capacity
			}
			pool, err := dialPool(entry.GRPCAddr, capacity)
			if err != nil {
				log.Printf("redis_registry: failed to dial gRPC pool for worker %s at %s: %v", entry.ID, entry.GRPCAddr, err)
			} else {
				r.pools[entry.ID] = pool
			}
		}
	}
}

// --- Worker selection ---

// GetLeastLoadedWorker picks the best worker for a new sandbox.
// Scoring: remaining capacity × resource headroom (CPU + memory).
// Workers with >90% CPU or memory are excluded.
// Prefers workers in the requested region, falls back to any region.
func (r *RedisWorkerRegistry) GetLeastLoadedWorker(region string) (*WorkerEntry, pb.SandboxWorkerClient, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var best *WorkerEntry
	bestScore := -1.0

	// First pass: workers in the requested region.
	for _, w := range r.workers {
		if region != "" && w.Region != region {
			continue
		}
		remaining := w.Capacity - w.Current
		if remaining <= 0 {
			continue
		}
		if w.CPUPct > 90 || w.MemPct > 90 {
			continue
		}
		resourceScore := (100.0 - w.CPUPct) / 100.0 * (100.0 - w.MemPct) / 100.0
		score := float64(remaining) * resourceScore
		if score > bestScore {
			best = w
			bestScore = score
		}
	}

	// Fallback: any region if no match found.
	if best == nil && region != "" {
		for _, w := range r.workers {
			remaining := w.Capacity - w.Current
			if remaining <= 0 {
				continue
			}
			if w.CPUPct > 90 || w.MemPct > 90 {
				continue
			}
			resourceScore := (100.0 - w.CPUPct) / 100.0 * (100.0 - w.MemPct) / 100.0
			score := float64(remaining) * resourceScore
			if score > bestScore {
				best = w
				bestScore = score
			}
		}
	}

	if best == nil {
		return nil, nil, fmt.Errorf("no workers available")
	}

	pool, ok := r.pools[best.ID]
	if !ok {
		return nil, nil, fmt.Errorf("no gRPC connection to worker %s", best.ID)
	}

	return best, pool.get(), nil
}

// GetWorkerClient returns a gRPC client for a specific worker, selected
// round-robin from the connection pool.
func (r *RedisWorkerRegistry) GetWorkerClient(workerID string) (pb.SandboxWorkerClient, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	pool, ok := r.pools[workerID]
	if !ok {
		return nil, fmt.Errorf("no gRPC connection to worker %s", workerID)
	}
	return pool.get(), nil
}

// GetWorker returns the cached state for a specific worker, or nil if unknown.
func (r *RedisWorkerRegistry) GetWorker(workerID string) *WorkerEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.workers[workerID]
}

// GetAllWorkers returns all known workers.
func (r *RedisWorkerRegistry) GetAllWorkers() []*WorkerEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]*WorkerEntry, 0, len(r.workers))
	for _, w := range r.workers {
		result = append(result, w)
	}
	return result
}

// Stop shuts down the registry: stops discovery loops, closes all gRPC
// connection pools, and disconnects from Redis.
func (r *RedisWorkerRegistry) Stop() {
	close(r.stop)

	r.mu.Lock()
	defer r.mu.Unlock()

	for id, pool := range r.pools {
		pool.close()
		delete(r.pools, id)
	}

	r.rdb.Close()
	log.Println("redis_registry: stopped")
}

// --- Autoscaler interface (ScalerRegistry) ---

// Regions returns all regions that have at least one registered worker.
func (r *RedisWorkerRegistry) Regions() []string {
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

// GetWorkersByRegion returns all workers in a specific region.
func (r *RedisWorkerRegistry) GetWorkersByRegion(region string) []*WorkerInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result []*WorkerInfo
	for _, w := range r.workers {
		if w.Region == region {
			result = append(result, &WorkerInfo{
				ID:        w.ID,
				MachineID: w.MachineID,
				Region:    w.Region,
				GRPCAddr:  w.GRPCAddr,
				HTTPAddr:  w.HTTPAddr,
				Capacity:  w.Capacity,
				Current:   w.Current,
				CPUPct:    w.CPUPct,
				MemPct:    w.MemPct,
			})
		}
	}
	return result
}

// RegionResourcePressure returns the peak CPU and memory usage across all
// workers in a region. Used by the autoscaler to decide when to scale up.
func (r *RedisWorkerRegistry) RegionResourcePressure(region string) (maxCPU, maxMem float64) {
	workers := r.GetWorkersByRegion(region)
	for _, w := range workers {
		if w.CPUPct > maxCPU {
			maxCPU = w.CPUPct
		}
		if w.MemPct > maxMem {
			maxMem = w.MemPct
		}
	}
	return maxCPU, maxMem
}

// RegionUtilization returns the average sandbox utilization (current/capacity)
// across all workers in a region. Used by the autoscaler for scale-down decisions.
func (r *RedisWorkerRegistry) RegionUtilization(region string) float64 {
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
