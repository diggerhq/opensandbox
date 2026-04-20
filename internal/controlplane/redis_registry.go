package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/keepalive"

	"github.com/opensandbox/opensandbox/internal/grpctls"

	pb "github.com/opensandbox/opensandbox/proto/worker"
)

// WorkerEntry represents a worker in the Redis-backed registry.
type WorkerEntry struct {
	ID        string  `json:"worker_id"`
	MachineID string  `json:"machine_id,omitempty"` // EC2 instance ID
	Region    string  `json:"region"`
	GRPCAddr  string  `json:"grpc_addr"`
	HTTPAddr  string  `json:"http_addr"`
	Capacity  int     `json:"capacity"`
	Current   int     `json:"current"`
	CPUPct    float64 `json:"cpu_pct"`
	MemPct    float64 `json:"mem_pct"`
	DiskPct           float64 `json:"disk_pct"`
	TotalMemoryMB     int     `json:"total_memory_mb,omitempty"`
	CommittedMemoryMB int     `json:"committed_memory_mb,omitempty"`
	GoldenVersion     string  `json:"golden_version,omitempty"`
	WorkerVersion     string  `json:"worker_version,omitempty"`
	Draining          bool    `json:"draining,omitempty"`
}

// RedisWorkerRegistry maintains an in-memory cache of worker state
// backed by Redis pub/sub for real-time updates and periodic SCAN for reconciliation.
// It also maintains a persistent gRPC connection pool to workers.
type RedisWorkerRegistry struct {
	rdb        *redis.Client
	mu         sync.RWMutex
	workers    map[string]*WorkerEntry       // in-memory hot cache
	conns      map[string]*grpc.ClientConn   // persistent gRPC connections
	clients    map[string]pb.SandboxWorkerClient // cached gRPC clients
	rrCounter  uint64                        // round-robin counter for tie-breaking
	stop       chan struct{}
}

// RedisClient returns the underlying Redis client (for health checks, shared state, etc.).
func (r *RedisWorkerRegistry) RedisClient() *redis.Client {
	return r.rdb
}

// NewRedisWorkerRegistry connects to Redis and returns a new registry.
func NewRedisWorkerRegistry(redisURL string) (*RedisWorkerRegistry, error) {
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("invalid redis URL: %w", err)
	}
	opts.PoolSize = 10
	opts.MinIdleConns = 2
	opts.ConnMaxIdleTime = 5 * time.Minute
	opts.ConnMaxLifetime = 30 * time.Minute
	opts.MaxRetries = 3

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
		conns:   make(map[string]*grpc.ClientConn),
		clients: make(map[string]pb.SandboxWorkerClient),
		stop:    make(chan struct{}),
	}, nil
}

// Start subscribes to the workers:heartbeat pub/sub channel and runs
// periodic reconciliation by scanning worker:* keys in Redis.
func (r *RedisWorkerRegistry) Start() {
	// Pub/sub subscriber
	go r.subscribeLoop()

	// Periodic reconciliation + stale worker cleanup
	go r.reconcileLoop()
}

// subscribeLoop listens for heartbeat messages via Redis pub/sub.
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

// reconcileLoop periodically scans Redis for worker:* keys.
// Workers present in Redis are added/updated; workers absent from Redis
// for 2 consecutive cycles are removed. This is the primary discovery
// mechanism — pub/sub provides faster first-detection but is not required.
func (r *RedisWorkerRegistry) reconcileLoop() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	// Do an initial scan immediately
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
// map, and removes any workers whose keys have expired (TTL elapsed).
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

	// Remove workers not found in this scan
	r.mu.Lock()
	defer r.mu.Unlock()
	for id := range r.workers {
		if !seen[id] {
			log.Printf("redis_registry: worker %s no longer in Redis, removing", id)
			delete(r.workers, id)
			if conn, ok := r.conns[id]; ok {
				conn.Close()
				delete(r.conns, id)
				delete(r.clients, id)
			}
		}
	}
}

// handleHeartbeat updates the in-memory worker map and dials gRPC if this is a new worker.
func (r *RedisWorkerRegistry) handleHeartbeat(entry WorkerEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()

	existing, ok := r.workers[entry.ID]
	if ok {
		// Update existing entry
		existing.Current = entry.Current
		existing.Capacity = entry.Capacity
		existing.CPUPct = entry.CPUPct
		existing.MemPct = entry.MemPct
		existing.DiskPct = entry.DiskPct
		existing.TotalMemoryMB = entry.TotalMemoryMB
		existing.CommittedMemoryMB = entry.CommittedMemoryMB
		if entry.GoldenVersion != "" {
			existing.GoldenVersion = entry.GoldenVersion
		}
		if entry.WorkerVersion != "" {
			existing.WorkerVersion = entry.WorkerVersion
		}
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
		// New worker
		r.workers[entry.ID] = &entry
		log.Printf("redis_registry: new worker registered: %s (region=%s, grpc=%s)", entry.ID, entry.Region, entry.GRPCAddr)
	}

	// Ensure gRPC connection exists and is healthy.
	// Re-dial if address changed, connection is failed/idle, or worker is newly registered.
	if entry.GRPCAddr != "" {
		needsDial := false
		if conn, hasConn := r.conns[entry.ID]; hasConn {
			addrChanged := existing != nil && existing.GRPCAddr != entry.GRPCAddr
			state := conn.GetState()
			connDead := state == connectivity.TransientFailure || state == connectivity.Shutdown
			// Also treat IDLE as potentially stale — the remote may have restarted.
			// gRPC won't detect a dead peer until an RPC is attempted on an IDLE conn.
			connIdle := state == connectivity.Idle
			isNewWorker := existing == nil

			if addrChanged || connDead || (connIdle && isNewWorker) {
				if addrChanged {
					log.Printf("redis_registry: worker %s gRPC address changed (%s → %s), re-dialing",
						entry.ID, existing.GRPCAddr, entry.GRPCAddr)
				} else {
					log.Printf("redis_registry: worker %s gRPC connection in %s state (new=%v), re-dialing",
						entry.ID, state.String(), isNewWorker)
				}
				conn.Close()
				delete(r.conns, entry.ID)
				delete(r.clients, entry.ID)
				needsDial = true
			} else if connIdle {
				// Existing worker with idle connection — force it to reconnect
				// so stale connections are detected quickly.
				conn.ResetConnectBackoff()
				conn.Connect()
			}
		} else {
			needsDial = true
		}
		if needsDial {
			r.dialWorkerLocked(entry.ID, entry.GRPCAddr)
		}
	}
}

// dialWorkerLocked dials a gRPC connection to a worker. Must be called with r.mu held.
func (r *RedisWorkerRegistry) dialWorkerLocked(workerID, grpcAddr string) {
	creds, err := grpctls.ClientCredentials()
	if err != nil {
		log.Printf("redis_registry: failed to load TLS credentials for worker %s: %v", workerID, err)
		return
	}
	conn, err := grpc.NewClient(grpcAddr,
		grpc.WithTransportCredentials(creds),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(256*1024*1024),
			grpc.MaxCallSendMsgSize(256*1024*1024),
		),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                10 * time.Second,
			Timeout:             5 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		log.Printf("redis_registry: failed to dial gRPC for worker %s at %s: %v", workerID, grpcAddr, err)
		return
	}
	// Force the lazy connection to start connecting immediately.
	// Without this, grpc.NewClient stays IDLE until the first RPC,
	// which means keepalive can't detect a dead connection.
	conn.Connect()
	r.conns[workerID] = conn
	r.clients[workerID] = pb.NewSandboxWorkerClient(conn)
	log.Printf("redis_registry: gRPC connection initiated to worker %s at %s", workerID, grpcAddr)
}

// GetLeastLoadedWorker returns the worker with the best combination of remaining capacity
// and resource headroom. Workers under heavy resource pressure (CPU > 90% or mem > 90%)
// are skipped. If region is non-empty, only workers in that region are considered.
// If no workers match the region, falls back to all workers.
func (r *RedisWorkerRegistry) GetLeastLoadedWorker(region string) (*WorkerEntry, pb.SandboxWorkerClient, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Simple strategy: pick the worker with fewest sandboxes.
	// Skip workers under heavy resource pressure.
	// Skip idle reserves unless all active workers are above 60% committed.
	// Read Redis routing counters to get real-time sandbox counts across both CPs
	routingCtx, routingCancel := context.WithTimeout(context.Background(), time.Second)
	defer routingCancel()

	var eligible []*WorkerEntry
	for _, w := range r.workers {
		if region != "" && w.Region != region {
			continue
		}
		if w.Draining {
			continue
		}
		if w.CPUPct > 90 || w.MemPct > 90 || w.DiskPct > 90 {
			continue
		}
		// Use Redis routing counter if higher than heartbeat count
		rCount, err := r.rdb.Get(routingCtx, "routing:count:"+w.ID).Int()
		if err == nil && rCount > w.Current {
			w.Current = rCount
		}
		eligible = append(eligible, w)
	}

	// Fallback: try any region
	if len(eligible) == 0 && region != "" {
		for _, w := range r.workers {
			if w.CPUPct > 90 || w.MemPct > 90 || w.DiskPct > 90 {
				continue
			}
			eligible = append(eligible, w)
		}
	}

	if len(eligible) == 0 {
		return nil, nil, fmt.Errorf("no workers available")
	}

	// Find workers tied for fewest sandboxes, round-robin among them
	minCount := eligible[0].Current
	for _, w := range eligible[1:] {
		if w.Current < minCount {
			minCount = w.Current
		}
	}
	var tied []*WorkerEntry
	for _, w := range eligible {
		if w.Current <= minCount+1 { // within 1 of the minimum
			tied = append(tied, w)
		}
	}

	// Use Redis-based round-robin counter so both CPs spread evenly
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	rr, _ := r.rdb.Incr(ctx, "routing:rr").Result()
	cancel()
	best := tied[rr%int64(len(tied))]

	client, ok := r.clients[best.ID]
	if !ok {
		return nil, nil, fmt.Errorf("no gRPC connection to worker %s", best.ID)
	}

	// Atomically increment sandbox count in Redis so BOTH CPs see it instantly.
	// The heartbeat will correct to the real count within 10s.
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	r.rdb.Incr(ctx2, "routing:count:"+best.ID)
	r.rdb.Expire(ctx2, "routing:count:"+best.ID, 15*time.Second)
	cancel2()
	best.Current++ // also update local for this CP

	return best, client, nil
}

// GetWorkerClient returns the gRPC client for a specific worker.
func (r *RedisWorkerRegistry) GetWorkerClient(workerID string) (pb.SandboxWorkerClient, error) {
	// Fast path: read lock, check connection state, return if healthy.
	r.mu.RLock()
	conn, hasConn := r.conns[workerID]
	client, hasClient := r.clients[workerID]
	var grpcAddr string
	if w, ok := r.workers[workerID]; ok {
		grpcAddr = w.GRPCAddr
	}
	r.mu.RUnlock()

	if !hasConn || !hasClient {
		return nil, fmt.Errorf("no gRPC connection to worker %s", workerID)
	}

	state := conn.GetState()
	switch {
	case state == connectivity.Ready || state == connectivity.Connecting:
		return client, nil

	case state == connectivity.Idle:
		// Nudge idle connections to reconnect proactively.
		conn.ResetConnectBackoff()
		conn.Connect()
		return client, nil

	case state == connectivity.TransientFailure || state == connectivity.Shutdown:
		// Slow path: write lock, re-dial. Only blocks callers for THIS worker.
		if grpcAddr == "" {
			return nil, fmt.Errorf("gRPC connection to worker %s is %s (no addr to re-dial)", workerID, state)
		}
		r.mu.Lock()
		// Double-check under write lock — another goroutine may have already re-dialed.
		if c, ok := r.conns[workerID]; ok && c.GetState() != connectivity.TransientFailure && c.GetState() != connectivity.Shutdown {
			client = r.clients[workerID]
			r.mu.Unlock()
			return client, nil
		}
		log.Printf("redis_registry: GetWorkerClient %s: conn in %s state, re-dialing", workerID, state)
		conn.Close()
		delete(r.conns, workerID)
		delete(r.clients, workerID)
		r.dialWorkerLocked(workerID, grpcAddr)
		newClient, ok := r.clients[workerID]
		r.mu.Unlock()
		if ok {
			return newClient, nil
		}
		return nil, fmt.Errorf("gRPC re-dial to worker %s failed", workerID)
	}

	return client, nil
}

// GetWorker returns the entry for a specific worker.
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

// SetDraining marks a worker as draining — it will not receive new sandboxes.
func (r *RedisWorkerRegistry) SetDraining(workerID string, draining bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if w, ok := r.workers[workerID]; ok {
		w.Draining = draining
	}
}

// Stop closes the Redis client and all gRPC connections.
func (r *RedisWorkerRegistry) Stop() {
	close(r.stop)

	r.mu.Lock()
	defer r.mu.Unlock()

	for id, conn := range r.conns {
		conn.Close()
		delete(r.conns, id)
		delete(r.clients, id)
	}

	r.rdb.Close()
	log.Println("redis_registry: stopped")
}

// Regions returns all known regions (satisfies ScalerRegistry).
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

// GetWorkersByRegion returns workers in a region (satisfies ScalerRegistry).
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
				DiskPct:       w.DiskPct,
				GoldenVersion: w.GoldenVersion,
				WorkerVersion: w.WorkerVersion,
			})
		}
	}
	return result
}

// RegionResourcePressure returns the maximum CPU and memory usage across all workers in a region (satisfies ScalerRegistry).
func (r *RedisWorkerRegistry) RegionResourcePressure(region string) (maxCPU, maxMem, maxDisk float64) {
	workers := r.GetWorkersByRegion(region)
	for _, w := range workers {
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
	return maxCPU, maxMem, maxDisk
}

// RegionUtilization returns the average utilization for a region (satisfies ScalerRegistry).
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
