package api

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/opensandbox/opensandbox/internal/controlplane"
	"github.com/opensandbox/opensandbox/internal/db"
	pb "github.com/opensandbox/opensandbox/proto/worker"
)

// Compile-time assertion that *Server satisfies the OrgHalter interface from
// the controlplane package. If the interface changes, this fails fast.
var _ controlplane.OrgHalter = (*Server)(nil)

// haltCoordinator dedupes concurrent halt webhooks for the same org. The DO
// retries on transient failure, and the halt_reconciler safety net may also
// fire a halt for the same org in the same ~30s window. We want each org to
// have at most one halt goroutine running at a time, so a redundant call
// short-circuits while the original finishes.
var (
	haltInFlight = struct {
		sync.Mutex
		m map[string]struct{}
	}{m: make(map[string]struct{})}
	resumeInFlight = struct {
		sync.Mutex
		m map[string]struct{}
	}{m: make(map[string]struct{})}
)

func acquireHaltSlot(orgID string) bool {
	haltInFlight.Lock()
	defer haltInFlight.Unlock()
	if _, busy := haltInFlight.m[orgID]; busy {
		return false
	}
	haltInFlight.m[orgID] = struct{}{}
	return true
}

func releaseHaltSlot(orgID string) {
	haltInFlight.Lock()
	defer haltInFlight.Unlock()
	delete(haltInFlight.m, orgID)
}

func acquireResumeSlot(orgID string) bool {
	resumeInFlight.Lock()
	defer resumeInFlight.Unlock()
	if _, busy := resumeInFlight.m[orgID]; busy {
		return false
	}
	resumeInFlight.m[orgID] = struct{}{}
	return true
}

func releaseResumeSlot(orgID string) {
	resumeInFlight.Lock()
	defer resumeInFlight.Unlock()
	delete(resumeInFlight.m, orgID)
}

// HaltOrg responds quickly (202-style — runs the actual hibernate work in a
// background goroutine) and returns the count of sandboxes the cell intends to
// halt. The DO doesn't need to block on every hibernate completing; the cell's
// halt_reconciler will re-issue the halt on the next tick if any sandboxes
// remain running, which keeps the system convergent without long DO calls.
//
// halt_reason='credits_exhausted' is stamped on each hibernated session so
// ResumeOrg can wake just those (and leave user-initiated hibernations alone).
func (s *Server) HaltOrg(ctx context.Context, orgIDStr, reason string) (int, error) {
	if s.store == nil {
		return 0, fmt.Errorf("database not configured")
	}
	if s.workerRegistry == nil {
		return 0, fmt.Errorf("worker registry not configured")
	}
	orgID, err := uuid.Parse(orgIDStr)
	if err != nil {
		return 0, fmt.Errorf("invalid org_id: %w", err)
	}

	// Mirror DO halt state to cell PG first — the wake handler reads this
	// column synchronously, so doing it before kicking off the async halt
	// closes the race where a wake arrives while the halt goroutine is
	// still iterating.
	if err := s.store.UpdateOrgHaltState(ctx, orgID, true); err != nil {
		// Non-fatal — log and continue. The reconciler will re-issue if needed.
		log.Printf("admin: halt-org %s: UpdateOrgHaltState failed: %v", orgIDStr, err)
	}

	sessions, err := s.store.ListSandboxSessions(ctx, orgID, "running", 1000, 0)
	if err != nil {
		return 0, fmt.Errorf("list running sessions: %w", err)
	}
	if len(sessions) == 0 {
		return 0, nil
	}

	if !acquireHaltSlot(orgIDStr) {
		// Another halt for this org is already in flight — short-circuit. The
		// running goroutine will hibernate everything visible at its start,
		// and the reconciler catches anything created in between.
		return len(sessions), nil
	}

	// Detach from the inbound HTTP context — caller (CF DO) is going to
	// return immediately, but the hibernate gRPC calls take 30s+ each and
	// we don't want them cancelled by the webhook's idle close.
	go func(sessions []db.SandboxSession) {
		defer releaseHaltSlot(orgIDStr)
		bgCtx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		for i := range sessions {
			sess := &sessions[i]
			if err := s.hibernateForHalt(bgCtx, sess.SandboxID, sess.WorkerID, sess.Region, sess.Template, sess.Config, orgID); err != nil {
				log.Printf("admin: halt-org %s: hibernate sandbox %s failed: %v (reason=%s)", orgIDStr, sess.SandboxID, err, reason)
				continue
			}
			if err := s.store.SetSandboxHaltReason(bgCtx, sess.SandboxID, "credits_exhausted"); err != nil {
				log.Printf("admin: halt-org %s: stamp halt_reason on %s failed: %v", orgIDStr, sess.SandboxID, err)
			}
		}
	}(sessions)

	return len(sessions), nil
}

// ResumeOrg clears the cell-local halt flag, then asynchronously wakes every
// sandbox the org has hibernated with halt_reason='credits_exhausted'. Manual
// hibernations (halt_reason IS NULL) stay hibernated — the user can wake them
// explicitly. skip_resume short-circuits the wake fan-out (useful when the
// DO just wants to mark the org as un-halted without auto-waking).
func (s *Server) ResumeOrg(ctx context.Context, orgIDStr string, skipResume bool) (int, error) {
	if s.store == nil {
		return 0, fmt.Errorf("database not configured")
	}
	if s.workerRegistry == nil {
		return 0, fmt.Errorf("worker registry not configured")
	}
	orgID, err := uuid.Parse(orgIDStr)
	if err != nil {
		return 0, fmt.Errorf("invalid org_id: %w", err)
	}

	if err := s.store.UpdateOrgHaltState(ctx, orgID, false); err != nil {
		log.Printf("admin: resume-org %s: UpdateOrgHaltState failed: %v", orgIDStr, err)
	}

	if skipResume {
		return 0, nil
	}

	sessions, err := s.store.ListSandboxSessionsByHaltReason(ctx, orgID, "credits_exhausted")
	if err != nil {
		return 0, fmt.Errorf("list halted sessions: %w", err)
	}
	if len(sessions) == 0 {
		return 0, nil
	}

	if !acquireResumeSlot(orgIDStr) {
		return len(sessions), nil
	}

	go func(sessions []db.SandboxSession) {
		defer releaseResumeSlot(orgIDStr)
		bgCtx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		for i := range sessions {
			sess := &sessions[i]
			if err := s.wakeForResume(bgCtx, sess.SandboxID); err != nil {
				log.Printf("admin: resume-org %s: wake sandbox %s failed: %v", orgIDStr, sess.SandboxID, err)
				continue
			}
			// Clear halt_reason — the sandbox is no longer credit-halted.
			if err := s.store.SetSandboxHaltReason(bgCtx, sess.SandboxID, ""); err != nil {
				log.Printf("admin: resume-org %s: clear halt_reason on %s failed: %v", orgIDStr, sess.SandboxID, err)
			}
		}
	}(sessions)

	return len(sessions), nil
}

// hibernateForHalt mirrors hibernateSandboxRemote but skips the auth check
// and runs without an echo.Context. Same gRPC + DB path; the DO webhook is
// the authorization.
func (s *Server) hibernateForHalt(ctx context.Context, sandboxID, workerID, region, template string, config []byte, orgID uuid.UUID) error {
	client, err := s.workerRegistry.GetWorkerClient(workerID)
	if err != nil {
		return fmt.Errorf("worker %s unreachable: %w", workerID, err)
	}
	grpcCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	grpcResp, err := client.HibernateSandbox(grpcCtx, &pb.HibernateSandboxRequest{SandboxId: sandboxID})
	if err != nil {
		return fmt.Errorf("grpc HibernateSandbox: %w", err)
	}
	_, superseded, _ := s.store.CreateHibernation(ctx, sandboxID, orgID, grpcResp.CheckpointKey, grpcResp.SizeBytes, region, template, config)
	s.deleteSupersededHibernation(superseded)
	_ = s.store.UpdateSandboxSessionStatus(ctx, sandboxID, "hibernated", nil)
	if s.sandboxAPIProxy != nil {
		s.sandboxAPIProxy.InvalidateRouteCache(sandboxID)
	}
	// Worker's HibernateSandbox handler emits the "hibernated" lifecycle event
	// + SandboxDBManager flushes it before SQLite is removed. events-ingest
	// updates D1 sandboxes_index from there.
	return nil
}

// wakeForResume mirrors wakeSandboxRemote but skips the wake-time credit
// gate (the whole point of resume is that the DO just decided the org is
// no longer halted) and runs without an echo.Context.
func (s *Server) wakeForResume(ctx context.Context, sandboxID string) error {
	hibernation, err := s.store.GetActiveHibernation(ctx, sandboxID)
	if err != nil {
		return fmt.Errorf("get active hibernation: %w", err)
	}
	session, err := s.store.GetSandboxSession(ctx, sandboxID)
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}
	if session.Status != "hibernated" {
		return fmt.Errorf("sandbox %s is not hibernated (status=%s)", sandboxID, session.Status)
	}

	region := hibernation.Region
	uploadComplete := hibernation.UploadedAt != nil
	var workerClient pb.SandboxWorkerClient
	var workerEntry *controlplane.WorkerEntry
	if session.WorkerID != "" {
		if src := s.workerRegistry.GetWorker(session.WorkerID); src != nil &&
			!src.Draining && src.CPUPct < 90 && src.MemPct < 90 && src.DiskPct < 90 {
			if cli, cerr := s.workerRegistry.GetWorkerClient(session.WorkerID); cerr == nil {
				workerEntry = src
				workerClient = cli
			}
		}
	}
	if workerEntry == nil {
		if !uploadComplete {
			return fmt.Errorf("source worker unavailable and hibernation upload not yet complete; retry shortly")
		}
		var lerr error
		workerEntry, workerClient, lerr = s.workerRegistry.GetLeastLoadedWorker(region)
		if lerr != nil {
			return fmt.Errorf("no workers available in region %s: %w", region, lerr)
		}
	}

	grpcCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	_, err = workerClient.WakeSandbox(grpcCtx, &pb.WakeSandboxRequest{
		SandboxId:     sandboxID,
		CheckpointKey: hibernation.HibernationKey,
	})
	if err != nil {
		return fmt.Errorf("grpc WakeSandbox: %w", err)
	}
	_ = s.store.MarkHibernationRestored(ctx, sandboxID)
	_ = s.store.UpdateSandboxSessionForWake(ctx, sandboxID, workerEntry.ID)
	if workerEntry.GoldenVersion != "" {
		_ = s.store.SetSandboxGoldenVersion(ctx, sandboxID, workerEntry.GoldenVersion)
	}
	if s.sandboxAPIProxy != nil {
		s.sandboxAPIProxy.InvalidateRouteCache(sandboxID)
	}
	// Worker's WakeSandbox handler emits "woke"; events-ingest sets D1 to running.
	return nil
}
