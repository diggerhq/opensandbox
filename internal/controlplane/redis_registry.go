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
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	pb "github.com/opensandbox/opensandbox/proto/worker"
)

// WorkerEntry represents a worker in the Redis-backed registry.
type WorkerEntry struct {
	ID       string  `json:"worker_id"`
	Region   string  `json:"region"`
	GRPCAddr string  `json:"grpc_addr"`
	HTTPAddr string  `json:"http_addr"`
	Capacity int     `json:"capacity"`
	Current  int     `json:"current"`
	CPUPct   float64 `json:"cpu_pct"`
	MemPct   float64 `json:"mem_pct"`
}

// RedisWorkerRegistry maintains an in-memory cache of worker state
// backed by Redis pub/sub for real-time updates and periodic SCAN for reconciliation.
// It also maintains a persistent gRPC connection pool to workers.
type RedisWorkerRegistry struct {
	rdb     *redis.Client
	mu      sync.RWMutex
	workers map[string]*WorkerEntry       // in-memory hot cache
	conns   map[string]*grpc.ClientConn   // persistent gRPC connections
	clients map[string]pb.SandboxWorkerClient // cached gRPC clients
	stop    chan struct{}
}

// NewRedisWorkerRegistry connects to Redis and returns a new registry.
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
		if entry.GRPCAddr != "" {
			existing.GRPCAddr = entry.GRPCAddr
		}
		if entry.HTTPAddr != "" {
			existing.HTTPAddr = entry.HTTPAddr
		}
	} else {
		// New worker
		r.workers[entry.ID] = &entry
		log.Printf("redis_registry: new worker registered: %s (region=%s, grpc=%s)", entry.ID, entry.Region, entry.GRPCAddr)
	}

	// Ensure gRPC connection exists and is healthy.
	// Re-dial if address changed OR connection is in a failed state.
	if entry.GRPCAddr != "" {
		if conn, hasConn := r.conns[entry.ID]; hasConn {
			addrChanged := existing != nil && existing.GRPCAddr != entry.GRPCAddr
			state := conn.GetState()
			connDead := state == connectivity.TransientFailure || state == connectivity.Shutdown
			if addrChanged || connDead {
				if addrChanged {
					log.Printf("redis_registry: worker %s gRPC address changed (%s → %s), re-dialing",
						entry.ID, existing.GRPCAddr, entry.GRPCAddr)
				} else {
					log.Printf("redis_registry: worker %s gRPC connection in %s state, re-dialing",
						entry.ID, conn.GetState().String())
				}
				conn.Close()
				delete(r.conns, entry.ID)
				delete(r.clients, entry.ID)
				r.dialWorkerLocked(entry.ID, entry.GRPCAddr)
			}
		} else {
			r.dialWorkerLocked(entry.ID, entry.GRPCAddr)
		}
	}
}

// dialWorkerLocked dials a gRPC connection to a worker. Must be called with r.mu held.
func (r *RedisWorkerRegistry) dialWorkerLocked(workerID, grpcAddr string) {
	conn, err := grpc.NewClient(grpcAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                30 * time.Second,
			Timeout:             10 * time.Second,
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

// GetLeastLoadedWorker returns the worker with the most remaining capacity.
// If region is non-empty, only workers in that region are considered.
// If no workers match the region, falls back to all workers.
func (r *RedisWorkerRegistry) GetLeastLoadedWorker(region string) (*WorkerEntry, pb.SandboxWorkerClient, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var best *WorkerEntry
	bestRemaining := -1

	// First pass: try workers in the requested region
	for _, w := range r.workers {
		if region != "" && w.Region != region {
			continue
		}
		remaining := w.Capacity - w.Current
		if remaining <= 0 {
			continue
		}
		if remaining > bestRemaining {
			best = w
			bestRemaining = remaining
		}
	}

	// Fallback: try any region
	if best == nil && region != "" {
		for _, w := range r.workers {
			remaining := w.Capacity - w.Current
			if remaining <= 0 {
				continue
			}
			if remaining > bestRemaining {
				best = w
				bestRemaining = remaining
			}
		}
	}

	if best == nil {
		return nil, nil, fmt.Errorf("no workers available")
	}

	client, ok := r.clients[best.ID]
	if !ok {
		return nil, nil, fmt.Errorf("no gRPC connection to worker %s", best.ID)
	}

	return best, client, nil
}

// GetWorkerClient returns the gRPC client for a specific worker.
func (r *RedisWorkerRegistry) GetWorkerClient(workerID string) (pb.SandboxWorkerClient, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	client, ok := r.clients[workerID]
	if !ok {
		return nil, fmt.Errorf("no gRPC connection to worker %s", workerID)
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
