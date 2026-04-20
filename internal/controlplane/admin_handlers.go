package controlplane

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	pb "github.com/opensandbox/opensandbox/proto/worker"

	"github.com/opensandbox/opensandbox/internal/billing"
	"github.com/opensandbox/opensandbox/internal/db"
)

const (
	adminMaxClockSkewSeconds = 5 * 60
	adminWakeTimeoutSeconds  = 90
)

// AdminHandlers bundles the HMAC-authed endpoints the CF api-edge Worker and
// CreditAccount DO call into: /admin/halt-org and /admin/resume-org. HMAC
// secret is shared with the CF side via cfg.CFAdminSecret.
type AdminHandlers struct {
	store    *db.Store
	workers  billing.WorkerClientSource
	secret   []byte
	resolver WorkerResolver
}

// WorkerResolver is the subset of the Redis registry the resume path needs:
// for a hibernated sandbox we pick a currently-available worker in the cell
// to wake onto, which the registry already does for us.
type WorkerResolver interface {
	GetLeastLoadedWorker(region string) (*WorkerEntry, pb.SandboxWorkerClient, error)
}

func NewAdminHandlers(store *db.Store, workers billing.WorkerClientSource, resolver WorkerResolver, secret string) *AdminHandlers {
	return &AdminHandlers{
		store:    store,
		workers:  workers,
		secret:   []byte(secret),
		resolver: resolver,
	}
}

// Register wires the admin endpoints onto an http.ServeMux (or any http.Handler
// registrar). Call from wherever the CP's public HTTP server lives.
func (h *AdminHandlers) Register(mux *http.ServeMux) {
	mux.HandleFunc("/admin/halt-org", h.withAuth(h.handleHaltOrg))
	mux.HandleFunc("/admin/resume-org", h.withAuth(h.handleResumeOrg))
}

// --- Authentication -------------------------------------------------------

func (h *AdminHandlers) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ts := r.Header.Get("X-Timestamp")
		sig := r.Header.Get("X-Signature")
		if ts == "" || sig == "" {
			http.Error(w, "missing auth headers", http.StatusUnauthorized)
			return
		}
		tsNum, err := strconv.ParseInt(ts, 10, 64)
		if err != nil {
			http.Error(w, "bad timestamp", http.StatusUnauthorized)
			return
		}
		if abs64(time.Now().Unix()-tsNum) > adminMaxClockSkewSeconds {
			http.Error(w, "timestamp out of window", http.StatusUnauthorized)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body)) // let handler re-read

		expected := hmacHex(h.secret, ts+"."+string(body))
		if !hmac.Equal([]byte(expected), []byte(sig)) {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// --- /admin/halt-org ------------------------------------------------------

func (h *AdminHandlers) handleHaltOrg(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		OrgID  string `json:"org_id"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.OrgID == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	orgID, err := uuid.Parse(body.OrgID)
	if err != nil {
		http.Error(w, "bad org_id", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	hibernated, err := billing.EnforceCreditExhaustion(ctx, h.store, h.workers, orgID)
	if err != nil {
		log.Printf("admin: halt-org %s failed: %v", orgID, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"org_id":     body.OrgID,
		"reason":     body.Reason,
		"hibernated": hibernated,
	})
}

// --- /admin/resume-org ----------------------------------------------------

func (h *AdminHandlers) handleResumeOrg(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		OrgID string `json:"org_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.OrgID == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	orgID, err := uuid.Parse(body.OrgID)
	if err != nil {
		http.Error(w, "bad org_id", http.StatusBadRequest)
		return
	}

	if h.resolver == nil {
		http.Error(w, "worker resolver not configured", http.StatusServiceUnavailable)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	// Find all hibernated sessions for the org. We filter by status only —
	// the CF side already scoped the call to cells where hibernations exist,
	// and distinguishing "credits_exhausted" vs user-initiated hibernate would
	// require a dedicated reason column. Today waking an already-credited
	// user-hibernated sandbox is harmless; if it becomes undesirable, the
	// plan's "halt_reason" column on sandbox_sessions is the fix.
	sessions, err := h.store.ListSandboxSessions(ctx, orgID, "hibernated", 500, 0)
	if err != nil {
		log.Printf("admin: resume-org %s list failed: %v", orgID, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resumed := 0
	for _, sess := range sessions {
		if err := h.wakeSession(ctx, sess); err != nil {
			log.Printf("admin: resume-org %s sandbox %s: %v", orgID, sess.SandboxID, err)
			continue
		}
		resumed++
	}
	log.Printf("admin: resume-org %s — resumed %d/%d hibernated sandbox(es)", orgID, resumed, len(sessions))
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"org_id":  body.OrgID,
		"resumed": resumed,
	})
}

func (h *AdminHandlers) wakeSession(ctx context.Context, sess db.SandboxSession) error {
	checkpoint, err := h.store.GetActiveHibernation(ctx, sess.SandboxID)
	if err != nil {
		return fmt.Errorf("no active hibernation: %w", err)
	}
	worker, client, err := h.resolver.GetLeastLoadedWorker(checkpoint.Region)
	if err != nil {
		return fmt.Errorf("no workers in region %s: %w", checkpoint.Region, err)
	}

	callCtx, cancel := context.WithTimeout(ctx, adminWakeTimeoutSeconds*time.Second)
	defer cancel()

	_, err = client.WakeSandbox(callCtx, &pb.WakeSandboxRequest{
		SandboxId:     sess.SandboxID,
		CheckpointKey: checkpoint.HibernationKey,
		Timeout:       300,
	})
	if err != nil {
		return fmt.Errorf("gRPC WakeSandbox: %w", err)
	}

	if err := h.store.MarkHibernationRestored(ctx, sess.SandboxID); err != nil {
		log.Printf("admin: MarkHibernationRestored %s: %v", sess.SandboxID, err)
	}
	if err := h.store.UpdateSandboxSessionForWake(ctx, sess.SandboxID, worker.ID); err != nil {
		log.Printf("admin: UpdateSandboxSessionForWake %s: %v", sess.SandboxID, err)
	}
	return nil
}

// --- Utilities ------------------------------------------------------------

func hmacHex(secret []byte, message string) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(message))
	return hex.EncodeToString(m.Sum(nil))
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
