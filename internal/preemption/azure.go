// Azure Scheduled Events preemption monitor.
//
// Azure publishes scheduled-events on IMDS at
//   GET /metadata/scheduledevents?api-version=2020-07-01
// (requires header `Metadata: true`). Response shape:
//   {
//     "DocumentIncarnation": 42,
//     "Events": [
//       { "EventId": "...", "EventType": "Preempt|Reboot|Redeploy|Freeze|Terminate",
//         "ResourceType": "VirtualMachine",
//         "Resources": ["<vm-name>"],
//         "EventStatus": "Scheduled|Started",
//         "NotBefore": "Mon, 19 May 2026 12:34:56 GMT" }
//     ]
//   }
//
// "Preempt" is the spot-equivalent ("This VM is being preempted; you have
// ~30s"). "Terminate" is unscheduled-but-imminent.
//
// This is a stub for the PoC — Azure cells aren't running the new
// preemption-aware code path yet. Wiring this up later is mostly populating
// the Watch loop with the same probe-then-emit pattern as the AWS monitor.

package preemption

import (
	"context"
	"log"
	"time"
)

type azureMonitor struct {
	pollInterval time.Duration
	imdsEndpoint string
}

func (m *azureMonitor) Name() string { return "azure-scheduled-events" }

func (m *azureMonitor) Watch(ctx context.Context) <-chan Notice {
	ch := make(chan Notice, 1)
	go func() {
		defer close(ch)
		log.Printf("preemption: azure monitor stubbed — TODO wire to /metadata/scheduledevents (see internal/preemption/azure.go)")
		<-ctx.Done()
	}()
	return ch
}
