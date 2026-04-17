package billing

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/opensandbox/opensandbox/internal/db"
	pb "github.com/opensandbox/opensandbox/proto/worker"
	"google.golang.org/grpc"
)

// --- Test doubles ---

type fakeLister struct {
	sessions           []db.SandboxSession
	err                error
	hibernatedStatuses []string
	endedScaleEvents   []string
	hibernations       []string
}

func (f *fakeLister) ListSandboxSessions(_ context.Context, _ uuid.UUID, _ string, _, _ int) ([]db.SandboxSession, error) {
	return f.sessions, f.err
}

func (f *fakeLister) UpdateSandboxSessionStatus(_ context.Context, sandboxID, status string, _ *string) error {
	f.hibernatedStatuses = append(f.hibernatedStatuses, sandboxID+"="+status)
	return nil
}

func (f *fakeLister) EndScaleEvent(_ context.Context, sandboxID string) error {
	f.endedScaleEvents = append(f.endedScaleEvents, sandboxID)
	return nil
}

func (f *fakeLister) CreateHibernation(_ context.Context, sandboxID string, _ uuid.UUID, key string, _ int64, _, _ string, _ json.RawMessage) (*db.SandboxHibernation, string, error) {
	f.hibernations = append(f.hibernations, sandboxID+"="+key)
	return &db.SandboxHibernation{SandboxID: sandboxID, HibernationKey: key}, "", nil
}

type fakeWorkerClient struct {
	pb.SandboxWorkerClient
	hibernated []string
	hibernateErr error
}

func (c *fakeWorkerClient) HibernateSandbox(_ context.Context, req *pb.HibernateSandboxRequest, _ ...grpc.CallOption) (*pb.HibernateSandboxResponse, error) {
	if c.hibernateErr != nil {
		return nil, c.hibernateErr
	}
	c.hibernated = append(c.hibernated, req.SandboxId)
	return &pb.HibernateSandboxResponse{
		SandboxId:     req.SandboxId,
		CheckpointKey: "ckpt-" + req.SandboxId,
		SizeBytes:     1024,
	}, nil
}

type fakeWorkerSource struct {
	clients map[string]*fakeWorkerClient
	missing map[string]bool
}

func (s *fakeWorkerSource) GetWorkerClient(workerID string) (pb.SandboxWorkerClient, error) {
	if s.missing[workerID] {
		return nil, errors.New("no client for worker")
	}
	if c, ok := s.clients[workerID]; ok {
		return c, nil
	}
	return nil, errors.New("unknown worker")
}

// --- Tests ---

func TestEnforceCreditExhaustion_NoSessions(t *testing.T) {
	lister := &fakeLister{}
	workers := &fakeWorkerSource{clients: map[string]*fakeWorkerClient{}}
	got, err := EnforceCreditExhaustion(context.Background(), lister, workers, uuid.New())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 0 {
		t.Fatalf("expected 0 hibernated, got %d", got)
	}
}

func TestEnforceCreditExhaustion_HibernatesAllRunning(t *testing.T) {
	orgID := uuid.New()
	lister := &fakeLister{
		sessions: []db.SandboxSession{
			{SandboxID: "sb-1", WorkerID: "w-a"},
			{SandboxID: "sb-2", WorkerID: "w-a"},
			{SandboxID: "sb-3", WorkerID: "w-b"},
		},
	}
	wA := &fakeWorkerClient{}
	wB := &fakeWorkerClient{}
	workers := &fakeWorkerSource{
		clients: map[string]*fakeWorkerClient{"w-a": wA, "w-b": wB},
	}

	got, err := EnforceCreditExhaustion(context.Background(), lister, workers, orgID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 3 {
		t.Fatalf("expected 3 hibernated, got %d", got)
	}
	if len(wA.hibernated) != 2 || len(wB.hibernated) != 1 {
		t.Fatalf("unexpected dispatch split: w-a=%v w-b=%v", wA.hibernated, wB.hibernated)
	}
}

func TestEnforceCreditExhaustion_SkipsWorkersWithoutClient(t *testing.T) {
	lister := &fakeLister{
		sessions: []db.SandboxSession{
			{SandboxID: "sb-1", WorkerID: "w-a"},
			{SandboxID: "sb-2", WorkerID: "w-gone"},
			{SandboxID: "sb-3", WorkerID: "w-a"},
		},
	}
	wA := &fakeWorkerClient{}
	workers := &fakeWorkerSource{
		clients: map[string]*fakeWorkerClient{"w-a": wA},
		missing: map[string]bool{"w-gone": true},
	}

	got, err := EnforceCreditExhaustion(context.Background(), lister, workers, uuid.New())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 2 {
		t.Fatalf("expected 2 hibernated (one worker gone), got %d", got)
	}
}

func TestEnforceCreditExhaustion_SkipsSessionsWithoutWorkerID(t *testing.T) {
	lister := &fakeLister{
		sessions: []db.SandboxSession{
			{SandboxID: "sb-orphan", WorkerID: ""},
			{SandboxID: "sb-ok", WorkerID: "w-a"},
		},
	}
	wA := &fakeWorkerClient{}
	workers := &fakeWorkerSource{
		clients: map[string]*fakeWorkerClient{"w-a": wA},
	}

	got, err := EnforceCreditExhaustion(context.Background(), lister, workers, uuid.New())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 1 {
		t.Fatalf("expected 1 hibernated (orphan skipped), got %d", got)
	}
	if len(wA.hibernated) != 1 || wA.hibernated[0] != "sb-ok" {
		t.Fatalf("expected only sb-ok hibernated, got %v", wA.hibernated)
	}
}

func TestEnforceCreditExhaustion_ContinuesOnGRPCError(t *testing.T) {
	lister := &fakeLister{
		sessions: []db.SandboxSession{
			{SandboxID: "sb-err", WorkerID: "w-err"},
			{SandboxID: "sb-ok", WorkerID: "w-ok"},
		},
	}
	wErr := &fakeWorkerClient{hibernateErr: errors.New("gRPC unavailable")}
	wOk := &fakeWorkerClient{}
	workers := &fakeWorkerSource{
		clients: map[string]*fakeWorkerClient{"w-err": wErr, "w-ok": wOk},
	}

	got, err := EnforceCreditExhaustion(context.Background(), lister, workers, uuid.New())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 1 {
		t.Fatalf("expected 1 hibernated (one errored), got %d", got)
	}
}
