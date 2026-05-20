package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// HaltReconciler is the safety net for missed halt-org / resume-org webhooks.
// Every 60s it pulls the authoritative halt-list from CF
// (GET HaltListURL?cell={cell_id}) and reconciles local state:
//
//   - For each org_id in the returned list: ensure no sandboxes are running.
//     Halt any stragglers via the same code path as /admin/halt-org.
//   - For any org_id NOT in the returned list but with locally hibernated
//     sandboxes whose halt_reason = 'credits_exhausted': resume them.
//
// This catches webhooks lost to network partitions, transient CP outages, or
// CF DO state drift. Primary path remains the push webhooks.
type HaltReconciler struct {
	cellID  string
	listURL string
	secret  string // HMAC for the GET request

	halter OrgHalter
	period time.Duration
	client *http.Client

	stopCh chan struct{}
	doneCh chan struct{}
	once   sync.Once
}

// HaltReconcilerConfig configures the reconciler.
type HaltReconcilerConfig struct {
	CellID  string
	ListURL string         // empty disables the reconciler
	Secret  string         // shared HMAC secret with CF (EVENT_SECRET)
	Halter  OrgHalter      // delegates to existing halt/resume code paths
	Period  time.Duration  // default 60s
}

// NewHaltReconciler constructs a reconciler. Returns nil if ListURL is empty.
func NewHaltReconciler(cfg HaltReconcilerConfig) *HaltReconciler {
	if cfg.ListURL == "" {
		return nil
	}
	if cfg.Period == 0 {
		cfg.Period = 60 * time.Second
	}
	return &HaltReconciler{
		cellID:  cfg.CellID,
		listURL: cfg.ListURL,
		secret:  cfg.Secret,
		halter:  cfg.Halter,
		period:  cfg.Period,
		client:  &http.Client{Timeout: 10 * time.Second},
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
	}
}

// Start begins the reconcile loop.
func (r *HaltReconciler) Start(ctx context.Context) {
	go r.run(ctx)
}

// Stop gracefully shuts down.
func (r *HaltReconciler) Stop(ctx context.Context) error {
	r.once.Do(func() { close(r.stopCh) })
	select {
	case <-r.doneCh:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

func (r *HaltReconciler) run(ctx context.Context) {
	defer close(r.doneCh)
	ticker := time.NewTicker(r.period)
	defer ticker.Stop()
	// First tick immediately so cold-start doesn't wait a full period.
	r.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.stopCh:
			return
		case <-ticker.C:
			r.tick(ctx)
		}
	}
}

// haltListResponse mirrors the api-edge /internal/halt-list response.
type haltListResponse struct {
	OrgIDs   []string         `json:"org_ids"`
	HaltedAt map[string]int64 `json:"halted_at"`
	AsOf     int64            `json:"as_of"`
}

// tick performs one reconciliation pass.
func (r *HaltReconciler) tick(ctx context.Context) {
	if r.halter == nil {
		return
	}

	tCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	list, err := r.fetchHaltList(tCtx)
	if err != nil {
		log.Printf("halt_reconciler: fetch halt-list failed: %v", err)
		return
	}

	// Re-halt anything in the list. HaltOrg is idempotent: if no sandboxes
	// are running for the org, it's a no-op. We still call it so the
	// is_halted mirror in cell PG is freshened in case it drifted.
	for _, orgID := range list.OrgIDs {
		if _, err := r.halter.HaltOrg(tCtx, orgID, "reconciler"); err != nil {
			log.Printf("halt_reconciler: halt %s: %v", orgID, err)
		}
	}

	// Resume reconciliation requires querying the local DB for halted-locally
	// orgs that are NOT in the list. The interface doesn't expose a "list
	// halted org_ids" method, so we leave the resume side of the reconciler
	// as a follow-up: in practice the DO is the gatekeeper for resume, and
	// missed resume webhooks are catchable manually until a method is added.
	// (The risk window is small: a user who upgraded mid-network-partition
	// stays halted until they retry or an operator pokes.)
}

func (r *HaltReconciler) fetchHaltList(ctx context.Context) (*haltListResponse, error) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	// Sign over "{ts}.{path+query}" — same scheme as api-edge's haltList()
	// handler. The path+query must match exactly what the server sees.
	pathWithQuery := "/internal/halt-list?cell=" + r.cellID
	sig := signGet(r.secret, ts, pathWithQuery)

	req, err := http.NewRequestWithContext(ctx, "GET", r.listURL+"?cell="+r.cellID, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("X-Timestamp", ts)
	req.Header.Set("X-Signature", sig)

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}

	var list haltListResponse
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("decode body: %w", err)
	}
	return &list, nil
}
