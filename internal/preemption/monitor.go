// Package preemption watches the cloud-specific spot-interruption signal and
// publishes a Notice on a channel once interruption is imminent. The worker
// binary subscribes to the channel and kicks off graceful drain:
//
//   1. Flip its Redis-heartbeat state to "draining" so the CP stops placing.
//   2. Hibernate every live sandbox to S3 in parallel (existing primitive).
//   3. Delete the heartbeat key; exit cleanly.
//
// On AWS the signal is IMDSv2 /latest/meta-data/spot/instance-action returning
// 200 with a JSON body. On Azure it's /metadata/scheduledevents. The shape of
// the work the worker does in response is identical — only the detection is
// cloud-specific. This package abstracts that.
//
// LocalCloud is decided by the OPENSANDBOX_CLOUD env var (set by cloud-init
// from terraform). If unset, the Monitor returned by NewMonitor is a no-op
// that never fires — safe default for dev / Azure-without-AWS.

package preemption

import (
	"context"
	"log"
	"os"
	"time"
)

// Action enumerates what the cloud says it will do to the instance.
type Action string

const (
	ActionTerminate Action = "terminate"
	ActionStop      Action = "stop"
	ActionHibernate Action = "hibernate" // AWS-only; Azure preempts only with "preempt"
)

// Notice carries the advance-warning details. ETA is when the cloud says it
// will act; the worker has roughly ETA - now to drain. AWS guarantees ~2 min
// for spot interruption; Azure scheduled events give ~30s for preempt and
// up to 15 min for reboot/freeze.
type Notice struct {
	Action Action
	ETA    time.Time
	// Source describes where the notice came from for logging — "aws-imds",
	// "azure-scheduled-events", or "redis" for CP-fanout fallback.
	Source string
}

// Monitor watches for preemption and emits a Notice on its channel when one
// arrives. Implementations must:
//   - Return a buffered channel (size >= 1) so a slow consumer doesn't lose
//     the notice.
//   - Survive transient errors (network blips polling IMDS) — log + retry.
//   - Stop cleanly when the provided context is canceled.
type Monitor interface {
	Name() string
	Watch(ctx context.Context) <-chan Notice
}

// NewMonitor returns the cloud-appropriate monitor or a no-op if no cloud
// is configured. Callers should always call Watch — even the no-op returns
// a channel they can select on, simplifying wire-up.
func NewMonitor() Monitor {
	switch os.Getenv("OPENSANDBOX_CLOUD") {
	case "aws":
		return &awsMonitor{
			pollInterval: 5 * time.Second,
			imdsEndpoint: "http://169.254.169.254",
		}
	case "azure":
		return &azureMonitor{
			pollInterval: 5 * time.Second,
			imdsEndpoint: "http://169.254.169.254",
		}
	default:
		log.Printf("preemption: no cloud configured (OPENSANDBOX_CLOUD unset) — preemption notices disabled")
		return &noopMonitor{}
	}
}

// noopMonitor never emits anything. Used for dev and any deployment where
// the cloud's preemption signal isn't worth wiring up (on-demand-only,
// bare-metal-colo, etc.).
type noopMonitor struct{}

func (noopMonitor) Name() string { return "noop" }
func (noopMonitor) Watch(ctx context.Context) <-chan Notice {
	ch := make(chan Notice, 1)
	// Channel left open; the caller's select will simply never fire on
	// this case. Closing on ctx.Done would also work but would make
	// receivers reading via "n, ok := <-ch" misinterpret it as a notice.
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch
}
