package controlplane

import (
	"time"

	"github.com/opensandbox/opensandbox/internal/auth"
	"github.com/opensandbox/opensandbox/internal/db"
)

// Server is a thin shell kept for projects.go's secret-store handlers, which
// were designed to hang off this type in an earlier architecture. Nothing
// currently constructs Server — the live HTTP server is api.NewServer — but
// the handler methods still compile against this struct. If you wire them up
// again, add whatever fields you need here.
type Server struct {
	store     *db.Store
	jwtIssuer *auth.JWTIssuer
}

// WorkerInfo represents a registered worker in the cell. The NATS-based
// registry that used to live here has been superseded by RedisWorkerRegistry
// in redis_registry.go; only the shared type remains.
type WorkerInfo struct {
	ID            string    `json:"worker_id"`
	MachineID     string    `json:"machine_id,omitempty"` // EC2 instance ID
	Region        string    `json:"region"`
	GRPCAddr      string    `json:"grpc_addr"`
	HTTPAddr      string    `json:"http_addr"`
	Capacity      int       `json:"capacity"`
	Current       int       `json:"current"`
	CPUPct        float64   `json:"cpu_pct"`
	MemPct        float64   `json:"mem_pct"`
	DiskPct       float64   `json:"disk_pct"`
	GoldenVersion string    `json:"golden_version,omitempty"`
	WorkerVersion string    `json:"worker_version,omitempty"`
	LastSeen      time.Time `json:"-"`
	MissedBeats   int       `json:"-"`
}
