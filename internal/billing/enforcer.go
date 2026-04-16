package billing

import (
	"context"
	"fmt"
	"log"

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

// sessionLister is the subset of *db.Store the enforcer depends on. Kept
// narrow for testability.
type sessionLister interface {
	ListSandboxSessions(ctx context.Context, orgID uuid.UUID, status string, limit, offset int) ([]db.SandboxSession, error)
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
	store sessionLister,
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
		if _, err := client.HibernateSandbox(ctx, &pb.HibernateSandboxRequest{
			SandboxId: sess.SandboxID,
		}); err != nil {
			log.Printf("enforcer: org %s sandbox %s hibernate failed: %v",
				orgID, sess.SandboxID, err)
			continue
		}
		hibernated++
	}

	log.Printf("enforcer: org %s credits exhausted — hibernated %d/%d running sandbox(es)",
		orgID, hibernated, len(sessions))
	return hibernated, nil
}
