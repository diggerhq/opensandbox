package compute

import (
	"context"
	"errors"
)

// ErrQuotaExceeded indicates a provider quota or capacity constraint that may
// be resolved by trying a different machine size. Pool implementations wrap
// (via errors.Join or fmt.Errorf %w) when they detect provider-specific quota
// or "no capacity in this SKU/zone" failures. The autoscaler uses errors.Is to
// decide whether to fall through to the next size in a ranked list versus
// abort the launch.
var ErrQuotaExceeded = errors.New("quota or capacity exceeded")

// Machine represents a worker machine in the compute pool.
type Machine struct {
	ID       string `json:"id"`
	Addr     string `json:"addr"`     // internal address (host:port) for gRPC
	HTTPAddr string `json:"httpAddr"` // public HTTP address for direct SDK access
	Region   string `json:"region"`
	Status   string `json:"status"`   // "running", "stopped", "creating"
	Capacity int    `json:"capacity"` // max sandboxes
	Current  int    `json:"current"`  // current sandbox count
}

// MachineOpts are options for creating a new machine.
type MachineOpts struct {
	Region string
	Size   string // provider-specific machine size
	Image  string // worker Docker image
}

// Pool is the interface for compute pool providers.
type Pool interface {
	CreateMachine(ctx context.Context, opts MachineOpts) (*Machine, error)
	DestroyMachine(ctx context.Context, machineID string) error
	StartMachine(ctx context.Context, machineID string) error
	StopMachine(ctx context.Context, machineID string) error
	ListMachines(ctx context.Context) ([]*Machine, error)
	HealthCheck(ctx context.Context, machineID string) error
	SupportedRegions(ctx context.Context) ([]string, error)
	DrainMachine(ctx context.Context, machineID string) error
}
