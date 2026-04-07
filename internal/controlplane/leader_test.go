package controlplane

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func newTestRedisClient(t *testing.T) *redis.Client {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{
		Addr: "localhost:6379",
		DB:   15, // use DB 15 for tests to avoid collisions
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("skipping: Redis not available at localhost:6379: %v", err)
	}
	// Clean up the leader key before and after
	rdb.Del(ctx, leaderKey)
	t.Cleanup(func() {
		rdb.Del(context.Background(), leaderKey)
		rdb.Close()
	})
	return rdb
}

func TestLeaderElectorAcquire(t *testing.T) {
	rdb := newTestRedisClient(t)

	le := NewLeaderElector(rdb, "test-instance-1")

	if le.IsLeader() {
		t.Fatal("should not be leader before starting")
	}

	le.tryAcquire()

	if !le.IsLeader() {
		t.Fatal("should be leader after acquiring")
	}

	// Verify Redis key holds our instance ID
	ctx := context.Background()
	val, err := rdb.Get(ctx, leaderKey).Result()
	if err != nil {
		t.Fatalf("expected leader key in Redis, got error: %v", err)
	}
	if val != "test-instance-1" {
		t.Fatalf("expected leader key value 'test-instance-1', got '%s'", val)
	}
}

func TestLeaderElectorRelease(t *testing.T) {
	rdb := newTestRedisClient(t)

	le := NewLeaderElector(rdb, "test-instance-1")
	le.tryAcquire()
	if !le.IsLeader() {
		t.Fatal("should be leader after acquiring")
	}

	le.release()

	if le.IsLeader() {
		t.Fatal("should not be leader after releasing")
	}

	// Verify key is gone
	ctx := context.Background()
	exists, err := rdb.Exists(ctx, leaderKey).Result()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exists != 0 {
		t.Fatal("leader key should be deleted after release")
	}
}

func TestLeaderElectorCallbacks(t *testing.T) {
	rdb := newTestRedisClient(t)

	le := NewLeaderElector(rdb, "test-instance-cb")

	var becameLeader int32
	var lostLeadership int32

	le.OnBecomeLeader(func() {
		atomic.AddInt32(&becameLeader, 1)
	})
	le.OnLoseLeadership(func() {
		atomic.AddInt32(&lostLeadership, 1)
	})

	le.tryAcquire()
	if atomic.LoadInt32(&becameLeader) != 1 {
		t.Fatalf("expected OnBecomeLeader to fire once, got %d", atomic.LoadInt32(&becameLeader))
	}

	// Acquiring again while already leader should not fire callback again
	le.tryAcquire()
	if atomic.LoadInt32(&becameLeader) != 1 {
		t.Fatalf("expected OnBecomeLeader to still be 1 after re-acquire, got %d", atomic.LoadInt32(&becameLeader))
	}

	le.release()
	if atomic.LoadInt32(&lostLeadership) != 1 {
		t.Fatalf("expected OnLoseLeadership to fire once, got %d", atomic.LoadInt32(&lostLeadership))
	}
}

func TestLeaderElectorTwoCompeting(t *testing.T) {
	rdb := newTestRedisClient(t)

	// Create a second Redis client on the same DB
	rdb2 := redis.NewClient(&redis.Options{
		Addr: "localhost:6379",
		DB:   15,
	})
	t.Cleanup(func() { rdb2.Close() })

	le1 := NewLeaderElector(rdb, "instance-A")
	le2 := NewLeaderElector(rdb2, "instance-B")

	// Instance A acquires first
	le1.tryAcquire()
	if !le1.IsLeader() {
		t.Fatal("instance-A should be leader")
	}

	// Instance B tries to acquire — should fail
	le2.tryAcquire()
	if le2.IsLeader() {
		t.Fatal("instance-B should not be leader while instance-A holds it")
	}

	// Instance A releases
	le1.release()
	if le1.IsLeader() {
		t.Fatal("instance-A should no longer be leader after release")
	}

	// Now instance B can acquire
	le2.tryAcquire()
	if !le2.IsLeader() {
		t.Fatal("instance-B should be leader after instance-A released")
	}
}

func TestLeaderElectorStartStop(t *testing.T) {
	rdb := newTestRedisClient(t)

	le := NewLeaderElector(rdb, "test-start-stop")

	var becameLeader int32
	le.OnBecomeLeader(func() {
		atomic.StoreInt32(&becameLeader, 1)
	})

	le.Start()

	// Give the goroutine time to acquire
	time.Sleep(200 * time.Millisecond)

	if !le.IsLeader() {
		t.Fatal("should be leader after Start()")
	}
	if atomic.LoadInt32(&becameLeader) != 1 {
		t.Fatal("OnBecomeLeader should have fired")
	}

	le.Stop()

	if le.IsLeader() {
		t.Fatal("should not be leader after Stop()")
	}
}

func TestLeaderElectorDefaultInstanceID(t *testing.T) {
	rdb := newTestRedisClient(t)

	le := NewLeaderElector(rdb, "")
	if le.InstanceID() == "" {
		t.Fatal("instance ID should not be empty when defaulting")
	}
}

func TestLeaderElectorRefreshTTL(t *testing.T) {
	rdb := newTestRedisClient(t)

	le := NewLeaderElector(rdb, "test-refresh")
	le.tryAcquire()
	if !le.IsLeader() {
		t.Fatal("should be leader")
	}

	// Get initial TTL
	ctx := context.Background()
	ttl1, err := rdb.TTL(ctx, leaderKey).Result()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Wait a bit, then re-acquire (refresh)
	time.Sleep(100 * time.Millisecond)
	le.tryAcquire()

	ttl2, err := rdb.TTL(ctx, leaderKey).Result()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// TTL should have been refreshed (ttl2 >= ttl1 or close to original)
	if ttl2 < ttl1-time.Second {
		t.Fatalf("expected TTL to be refreshed, got ttl1=%v ttl2=%v", ttl1, ttl2)
	}
}
