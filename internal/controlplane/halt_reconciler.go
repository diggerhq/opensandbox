package controlplane

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/opensandbox/opensandbox/internal/billing"
	"github.com/opensandbox/opensandbox/internal/db"
)

const (
	haltReconcileInterval = 60 * time.Second
	haltReconcileTimeout  = 30 * time.Second
)

// HaltReconciler is the CP-side safety net for the CF DO push path. Every
// 60 seconds it calls GET /internal/halt-list?cell={cell_id} on the api-edge
// Worker and compares the returned org_ids against local PG state:
//
//   - Orgs in the CF list with running sandboxes locally → halt them
//     (primary halt webhook lost).
//   - Orgs NOT in the CF list with locally-hibernated sandboxes that were
//     credit-exhausted → resume them (resume webhook lost).
//
// The reconciler uses the same HMAC secret as the admin handlers (shared
// CFAdminSecret) and is idempotent: double-halting a hibernated sandbox is a
// no-op, and double-resuming is protected by status checks inside wake.
type HaltReconciler struct {
	store        *db.Store
	workers      billing.WorkerClientSource
	resolver     WorkerResolver
	haltListURL  string
	secret       []byte
	cellID       string
	httpClient   *http.Client
	admin        *AdminHandlers // reused for wake/hibernate implementations

	stop chan struct{}
	wg   sync.WaitGroup
}

func NewHaltReconciler(
	store *db.Store,
	workers billing.WorkerClientSource,
	resolver WorkerResolver,
	haltListURL, adminSecret, cellID string,
	admin *AdminHandlers,
) *HaltReconciler {
	return &HaltReconciler{
		store:       store,
		workers:     workers,
		resolver:    resolver,
		haltListURL: haltListURL,
		secret:      []byte(adminSecret),
		cellID:      cellID,
		httpClient:  &http.Client{Timeout: haltReconcileTimeout},
		admin:       admin,
		stop:        make(chan struct{}),
	}
}

func (r *HaltReconciler) Start() {
	r.wg.Add(1)
	go r.loop()
}

func (r *HaltReconciler) Stop() {
	close(r.stop)
	r.wg.Wait()
}

func (r *HaltReconciler) loop() {
	defer r.wg.Done()
	ticker := time.NewTicker(haltReconcileInterval)
	defer ticker.Stop()

	// First pass runs after one tick so the process isn't hammering CF during
	// startup thundering-herd windows.
	for {
		select {
		case <-r.stop:
			return
		case <-ticker.C:
			if err := r.reconcile(); err != nil {
				log.Printf("halt_reconciler: %v", err)
			}
		}
	}
}

func (r *HaltReconciler) reconcile() error {
	ctx, cancel := context.WithTimeout(context.Background(), haltReconcileTimeout)
	defer cancel()

	orgIDs, err := r.fetchHaltList(ctx)
	if err != nil {
		return fmt.Errorf("fetch halt-list: %w", err)
	}

	inList := make(map[string]struct{}, len(orgIDs))
	for _, id := range orgIDs {
		inList[id] = struct{}{}
	}

	// Primary reconciliation: for each org the CF side says should be halted,
	// ensure we have no running sandboxes for it. EnforceCreditExhaustion is
	// idempotent on already-hibernated sandboxes (it lists running only).
	for id := range inList {
		orgID, err := uuid.Parse(id)
		if err != nil {
			continue
		}
		if _, err := billing.EnforceCreditExhaustion(ctx, r.store, r.workers, orgID); err != nil {
			log.Printf("halt_reconciler: enforce halt for %s: %v", id, err)
		}
	}

	// Secondary reconciliation: orgs NOT in the list but with locally
	// hibernated sandboxes may have had their resume webhook lost. We can't
	// tell credit-exhausted from user-initiated hibernate without a reason
	// column, so we only resume when the CF side has flipped the plan to
	// "pro" — which, in the CF-parallel model, means the org simply isn't
	// in the halt list AND isn't free anymore. Since we don't query CF for
	// plan here, we skip this branch for now; upgrade-driven resumes flow
	// through the DO's /mark-pro dispatch instead.
	_ = inList

	return nil
}

func (r *HaltReconciler) fetchHaltList(ctx context.Context) ([]string, error) {
	u, err := url.Parse(r.haltListURL)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("cell", r.cellID)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := reconcileSign(r.secret, ts, "")
	req.Header.Set("X-Timestamp", ts)
	req.Header.Set("X-Signature", sig)

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("halt-list returned %d", resp.StatusCode)
	}
	var body struct {
		OrgIDs []string `json:"org_ids"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	return body.OrgIDs, nil
}

func reconcileSign(secret []byte, ts, body string) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(ts + "." + body))
	return hex.EncodeToString(m.Sum(nil))
}
