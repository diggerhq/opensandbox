package controlplane

import (
	"context"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	leaderKey     = "controlplane:leader"
	leaderTTL     = 15 * time.Second
	leaderRefresh = 5 * time.Second
)

// LeaderElector manages leader election for the control plane via Redis.
// Only the leader runs the scaler and maintenance tasks.
type LeaderElector struct {
	rdb        *redis.Client
	instanceID string
	isLeader   int32 // atomic

	mu               sync.Mutex
	onBecomeLeader   func()
	onLoseLeadership func()
	stop             chan struct{}
	wg               sync.WaitGroup
}

// NewLeaderElector creates a new leader elector.
// instanceID should be unique per control plane instance (hostname or UUID).
func NewLeaderElector(rdb *redis.Client, instanceID string) *LeaderElector {
	if instanceID == "" {
		instanceID, _ = os.Hostname()
		if instanceID == "" {
			instanceID = "unknown"
		}
	}
	return &LeaderElector{
		rdb:        rdb,
		instanceID: instanceID,
		stop:       make(chan struct{}),
	}
}

// OnBecomeLeader registers a callback fired when this instance becomes the leader.
func (le *LeaderElector) OnBecomeLeader(fn func()) {
	le.mu.Lock()
	defer le.mu.Unlock()
	le.onBecomeLeader = fn
}

// OnLoseLeadership registers a callback fired when this instance loses leadership.
func (le *LeaderElector) OnLoseLeadership(fn func()) {
	le.mu.Lock()
	defer le.mu.Unlock()
	le.onLoseLeadership = fn
}

// IsLeader returns true if this instance is currently the leader.
func (le *LeaderElector) IsLeader() bool {
	return atomic.LoadInt32(&le.isLeader) == 1
}

// InstanceID returns this instance's identifier.
func (le *LeaderElector) InstanceID() string {
	return le.instanceID
}

// Start begins the leader election loop.
func (le *LeaderElector) Start() {
	le.wg.Add(1)
	go func() {
		defer le.wg.Done()
		le.tryAcquire()

		ticker := time.NewTicker(leaderRefresh)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				le.tryAcquire()
			case <-le.stop:
				le.release()
				return
			}
		}
	}()
	log.Printf("leader: election started (instance=%s)", le.instanceID)
}

// Stop stops the leader election loop and releases leadership if held.
func (le *LeaderElector) Stop() {
	close(le.stop)
	le.wg.Wait()
}

func (le *LeaderElector) tryAcquire() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	wasLeader := atomic.LoadInt32(&le.isLeader) == 1

	// Try to set the key if it doesn't exist
	set, err := le.rdb.SetArgs(ctx, leaderKey, le.instanceID, redis.SetArgs{
		Mode: "NX",
		TTL:  leaderTTL,
	}).Result()

	if err == nil && set == "OK" {
		// We acquired leadership
		if !wasLeader {
			atomic.StoreInt32(&le.isLeader, 1)
			log.Printf("leader: acquired leadership (instance=%s)", le.instanceID)
			le.mu.Lock()
			fn := le.onBecomeLeader
			le.mu.Unlock()
			if fn != nil {
				fn()
			}
		}
		return
	}

	// Key already exists — check if we own it
	current, err := le.rdb.Get(ctx, leaderKey).Result()
	if err != nil {
		// Redis error — can't determine leader, stay in current state
		return
	}

	if current == le.instanceID {
		// We're still the leader — refresh TTL
		le.rdb.Expire(ctx, leaderKey, leaderTTL)
		if !wasLeader {
			atomic.StoreInt32(&le.isLeader, 1)
			log.Printf("leader: reclaimed leadership (instance=%s)", le.instanceID)
			le.mu.Lock()
			fn := le.onBecomeLeader
			le.mu.Unlock()
			if fn != nil {
				fn()
			}
		}
	} else {
		// Another instance is leader
		if wasLeader {
			atomic.StoreInt32(&le.isLeader, 0)
			log.Printf("leader: lost leadership to %s (instance=%s)", current, le.instanceID)
			le.mu.Lock()
			fn := le.onLoseLeadership
			le.mu.Unlock()
			if fn != nil {
				fn()
			}
		}
	}
}

func (le *LeaderElector) release() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Only delete the key if we own it (avoid deleting another leader's key)
	current, err := le.rdb.Get(ctx, leaderKey).Result()
	if err == nil && current == le.instanceID {
		le.rdb.Del(ctx, leaderKey)
		log.Printf("leader: released leadership (instance=%s)", le.instanceID)
	}

	if atomic.LoadInt32(&le.isLeader) == 1 {
		atomic.StoreInt32(&le.isLeader, 0)
		le.mu.Lock()
		fn := le.onLoseLeadership
		le.mu.Unlock()
		if fn != nil {
			fn()
		}
	}
}
