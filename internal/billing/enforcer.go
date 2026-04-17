package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/opensandbox/opensandbox/internal/db"
	pb "github.com/opensandbox/opensandbox/proto/worker"
)

// WorkerClientSource is the minimal surface the enforcer needs to reach
// worker gRPC servers. Both the NATS and Redis registries satisfy this
// implicitly (ScalerRegistry in internal/controlplane), and we keep the
// dependency at interface-level so billing stays independent of controlplane.
type WorkerClientSource interface {
	GetWorkerClient(workerID string) (pb.SandboxWorkerClient, error)
}

// enforcerStore is the subset of *db.Store the enforcer depends on. Kept
// narrow for testability.
type enforcerStore interface {
	ListSandboxSessions(ctx context.Context, orgID uuid.UUID, status string, limit, offset int) ([]db.SandboxSession, error)
	UpdateSandboxSessionStatus(ctx context.Context, sandboxID, status string, errorMsg *string) error
	EndScaleEvent(ctx context.Context, sandboxID string) error
	CreateHibernation(ctx context.Context, sandboxID string, orgID uuid.UUID, hibernationKey string, sizeBytes int64, region, template string, sandboxConfig json.RawMessage) (*db.SandboxHibernation, string, error)
}

// EnforceCreditExhaustion force-hibernates every running sandbox belonging to
// an org whose free-tier credit balance has dropped to <= 0. Returns the
// number of sandboxes successfully hibernated. Errors on individual sandboxes
// are logged and counted as failures; the function continues for the rest.
//
// Intended caller: the usage reporter, immediately after DeductFreeCredits
// returns a non-positive balance.
func EnforceCreditExhaustion(
	ctx context.Context,
	store enforcerStore,
	workers WorkerClientSource,
	orgID uuid.UUID,
) (hibernated int, err error) {
	// Page through all running sandboxes for this org. The default page size
	// of 500 is well above any realistic free-tier sandbox count.
	const pageSize = 500
	sessions, err := store.ListSandboxSessions(ctx, orgID, "running", pageSize, 0)
	if err != nil {
		return 0, fmt.Errorf("list running sandboxes: %w", err)
	}
	if len(sessions) == 0 {
		return 0, nil
	}

	for _, sess := range sessions {
		if sess.WorkerID == "" {
			log.Printf("enforcer: org %s sandbox %s has no worker_id, skipping", orgID, sess.SandboxID)
			continue
		}
		client, err := workers.GetWorkerClient(sess.WorkerID)
		if err != nil {
			log.Printf("enforcer: org %s sandbox %s: no client for worker %s: %v",
				orgID, sess.SandboxID, sess.WorkerID, err)
			continue
		}
		// Per-sandbox timeout so one slow/stuck worker can't block the entire
		// enforcer pass (and kill the reporter goroutine's tick loop).
		callCtx, callCancel := context.WithTimeout(ctx, 30*time.Second)
		resp, err := client.HibernateSandbox(callCtx, &pb.HibernateSandboxRequest{
			SandboxId: sess.SandboxID,
		})
		callCancel()
		if err != nil {
			log.Printf("enforcer: org %s sandbox %s hibernate failed: %v",
				orgID, sess.SandboxID, err)
			continue
		}
		// Update PG directly — the control plane initiated this hibernation,
		// so we can't rely on worker-side callbacks or async sync to propagate
		// the status change back to PG.
		if err := store.EndScaleEvent(ctx, sess.SandboxID); err != nil {
			log.Printf("enforcer: org %s sandbox %s: EndScaleEvent failed: %v", orgID, sess.SandboxID, err)
		}
		if err := store.UpdateSandboxSessionStatus(ctx, sess.SandboxID, "hibernated", nil); err != nil {
			log.Printf("enforcer: org %s sandbox %s: status update failed: %v", orgID, sess.SandboxID, err)
		}
		// Create the hibernation record so the wake endpoint can find the
		// checkpoint key. The gRPC handler doesn't do this (only the
		// auto-timeout path does), so we must do it here.
		checkpointKey := ""
		var sizeBytes int64
		if resp != nil {
			checkpointKey = resp.CheckpointKey
			sizeBytes = resp.SizeBytes
		}
		if checkpointKey != "" {
			if _, _, err := store.CreateHibernation(ctx, sess.SandboxID, orgID, checkpointKey, sizeBytes, sess.Region, sess.Template, sess.Config); err != nil {
				log.Printf("enforcer: org %s sandbox %s: CreateHibernation failed: %v", orgID, sess.SandboxID, err)
			}
		} else {
			log.Printf("enforcer: org %s sandbox %s: no checkpoint key in response, wake will not work", orgID, sess.SandboxID)
		}
		hibernated++
	}

	log.Printf("enforcer: org %s credits exhausted — hibernated %d/%d running sandbox(es)",
		orgID, hibernated, len(sessions))
	return hibernated, nil
}
