// grpc_pool.go — gRPC connection pool for control plane → worker communication.
//
// Problem: A single HTTP/2 connection multiplexes all RPCs over one TCP stream.
// Under concurrent load (100+ simultaneous sandbox creates/destroys), the 64KB
// send window becomes a bottleneck — RPCs queue behind each other (head-of-line
// blocking), causing timeouts.
//
// Solution: Maintain a pool of independent gRPC connections per worker. Each RPC
// is dispatched round-robin across the pool, spreading load across multiple
// HTTP/2 streams. Pool size scales with worker capacity (1 conn per 25 sandboxes,
// min 8, max 16).
package controlplane

import (
	"log"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	"github.com/opensandbox/opensandbox/internal/grpctls"
	pb "github.com/opensandbox/opensandbox/proto/worker"
)

// grpcPool holds multiple gRPC connections to a single worker.
// All connections target the same address; round-robin distributes RPCs.
type grpcPool struct {
	conns   []*grpc.ClientConn       // underlying transport connections
	clients []pb.SandboxWorkerClient // gRPC stubs, one per connection
	counter uint64                   // atomic round-robin counter
}

// dialPool creates a pool of gRPC connections to a worker at grpcAddr.
// The capacity parameter (worker's max sandbox count) determines pool size:
//   - 1 connection per 25 sandboxes
//   - minimum 8 connections (handles burst traffic on small workers)
//   - maximum 16 connections (diminishing returns beyond this)
//
// Each connection is configured with:
//   - mTLS credentials (loaded from grpctls)
//   - 256MB max message size (for large file transfers)
//   - 30s keepalive interval (detects dead connections before RPCs fail)
func dialPool(grpcAddr string, capacity int) (*grpcPool, error) {
	poolSize := 8
	if capacity > 0 {
		poolSize = capacity / 25
	}
	if poolSize < 8 {
		poolSize = 8
	}
	if poolSize > 16 {
		poolSize = 16
	}

	creds, err := grpctls.ClientCredentials()
	if err != nil {
		return nil, err
	}

	pool := &grpcPool{
		conns:   make([]*grpc.ClientConn, 0, poolSize),
		clients: make([]pb.SandboxWorkerClient, 0, poolSize),
	}

	for i := 0; i < poolSize; i++ {
		conn, err := grpc.NewClient(grpcAddr,
			grpc.WithTransportCredentials(creds),
			grpc.WithDefaultCallOptions(
				grpc.MaxCallRecvMsgSize(256*1024*1024),
				grpc.MaxCallSendMsgSize(256*1024*1024),
			),
			grpc.WithKeepaliveParams(keepalive.ClientParameters{
				Time:                30 * time.Second,
				Timeout:             10 * time.Second,
				PermitWithoutStream: true,
			}),
		)
		if err != nil {
			pool.close() // clean up any connections already opened
			return nil, err
		}
		// Force immediate connection (grpc.NewClient is lazy by default).
		// Without this, keepalive can't detect a dead connection until the
		// first RPC, which may be too late.
		conn.Connect()
		pool.conns = append(pool.conns, conn)
		pool.clients = append(pool.clients, pb.NewSandboxWorkerClient(conn))
	}

	log.Printf("grpc_pool: created %d connections to %s", poolSize, grpcAddr)
	return pool, nil
}

// get returns the next client via atomic round-robin.
// Safe for concurrent use — no locks needed.
func (p *grpcPool) get() pb.SandboxWorkerClient {
	idx := atomic.AddUint64(&p.counter, 1) % uint64(len(p.clients))
	return p.clients[idx]
}

// close shuts down all connections in the pool.
func (p *grpcPool) close() {
	for _, conn := range p.conns {
		conn.Close()
	}
}
