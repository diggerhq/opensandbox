// backfill copies the global control-plane tables from a cell's PostgreSQL
// into the edge Cloudflare D1 database, for the PG → D1 cutover.
//
// It runs in three modes against the same code path:
//
//	--mode=full    snapshot every row (no watermark filter). Run once up front.
//	--mode=delta   only rows with updated_at > --since. Run repeatedly to keep
//	               D1 fresh while PG is still live; cheap, idempotent.
//	--mode=final   identical to delta — the name documents intent (the last
//	               delta run during the write blackout at the cutover moment).
//
// Every write is an UPSERT keyed on the D1 primary key, so re-runs and
// overlapping deltas converge. The watermark is updated_at across all tables
// (added uniformly by migration 040_add_updated_at). Each run prints the new
// max watermark to feed --since of the next run.
//
// Usage:
//
//	OPENSANDBOX_DATABASE_URL=postgres://... \
//	OPENSANDBOX_CF_API_TOKEN=... \
//	  go run ./cmd/backfill \
//	    --mode=full \
//	    --cell=azure-westus2-cell-0 \
//	    --home-cell=azure-westus2-cell-0 \
//	    --blob-prefix="azure-blob://opencomputer-prod/" \
//	    --account=1241f114453e32d292197e3fb36210b2 \
//	    --database=f9e534bf-a9f3-4b30-81cd-f9eb9087e96d
//
// The 9 global identity/billing/catalog tables are pure PG→D1. sandboxes_index
// and checkpoints_index are per-cell projections: cell_id/owner_cell_id come
// from --cell (single existing cell at first cutover), and checkpoints_index
// derives golden_hash from sandbox_sessions.golden_version — checkpoints whose
// golden can't be recovered are skipped (logged) since they stay restorable in
// their home cell.
package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/opensandbox/opensandbox/internal/crypto"
)

// rawLiteral marks a value that must be inlined into the SQL text verbatim
// instead of being sent as a bound parameter — used for SQLite blob literals
// (X'..hex..'), which can't travel as JSON params over the D1 HTTP API.
type rawLiteral string

// tableSpec describes one PG→D1 table copy.
type tableSpec struct {
	d1Table string
	cols    []string // D1 column order for the INSERT
	pk      []string // conflict-target columns
	// query returns the PG SQL + args. full=true omits the watermark filter;
	// otherwise the query must filter updated_at > to_timestamp($1).
	query func(full bool) (string, []any)
	// assemble maps one PG row (pgx rows.Values(), in the query's select order)
	// into a D1 row aligned with cols, plus the row's watermark (unix s) and a
	// skip flag for rows we intentionally drop.
	assemble func(src []any) (row []any, wm float64, skip bool)
}

func main() {
	mode := flag.String("mode", "", "full | delta | final")
	since := flag.Float64("since", 0, "delta/final: only rows with updated_at > this unix-seconds watermark")
	cell := flag.String("cell", "", "cell_id for sandboxes_index.cell_id")
	homeCell := flag.String("home-cell", "", "home_cell stamped on every backfilled org (defaults to --cell)")
	blobPrefix := flag.String("blob-prefix", "", "prefix prepended to rootfs_s3_key to form checkpoints_index.s3_url")
	account := flag.String("account", os.Getenv("OPENSANDBOX_CF_ACCOUNT_ID"), "Cloudflare account id")
	database := flag.String("database", os.Getenv("OPENSANDBOX_D1_DATABASE_ID"), "D1 database id")
	sandboxStates := flag.String("sandbox-states", "running,hibernated", "comma list of sandbox statuses to include in sandboxes_index")
	only := flag.String("tables", "", "optional comma list to restrict which D1 tables to sync (default all)")
	batchParams := flag.Int("batch-params", 90, "max bound params per D1 query (rows are chunked to fit)")
	interval := flag.Duration("interval", 10*time.Second, "daemon mode: poll interval")
	stateFile := flag.String("state-file", "/tmp/backfill-sync-state.json", "daemon mode: where to persist the watermark + delete-seq cursor")
	dryRun := flag.Bool("dry-run", false, "query + count only; no D1 writes")
	flag.Parse()

	if *mode != "full" && *mode != "delta" && *mode != "final" && *mode != "daemon" {
		log.Fatal("--mode must be full, delta, final, or daemon")
	}
	full := *mode == "full"
	// delta/final need an explicit --since; daemon bootstraps from --state-file
	// (falling back to --since the first time if the state file is absent).
	if (*mode == "delta" || *mode == "final") && *since <= 0 {
		log.Fatal("--since (unix seconds) is required for delta/final mode")
	}
	if *cell == "" {
		log.Fatal("--cell is required")
	}
	if *homeCell == "" {
		*homeCell = *cell
	}
	if *blobPrefix == "" {
		log.Fatal("--blob-prefix is required (for checkpoints_index.s3_url)")
	}

	dbURL := os.Getenv("OPENSANDBOX_DATABASE_URL")
	if dbURL == "" {
		log.Fatal("OPENSANDBOX_DATABASE_URL is required")
	}
	token := os.Getenv("OPENSANDBOX_CF_API_TOKEN")
	if !*dryRun {
		if token == "" || *account == "" || *database == "" {
			log.Fatal("OPENSANDBOX_CF_API_TOKEN, --account, and --database are required unless --dry-run")
		}
	}

	var ctx context.Context
	var cancel context.CancelFunc
	if *mode == "daemon" {
		ctx, cancel = signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	} else {
		ctx, cancel = context.WithTimeout(context.Background(), 30*time.Minute)
	}
	defer cancel()

	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		log.Fatalf("connect pg: %v", err)
	}
	defer conn.Close(ctx)

	d1 := &d1Client{
		http:     &http.Client{Timeout: 60 * time.Second},
		account:  *account,
		database: *database,
		token:    token,
		dryRun:   *dryRun,
	}

	var wanted map[string]bool
	if *only != "" {
		wanted = map[string]bool{}
		for _, t := range strings.Split(*only, ",") {
			wanted[strings.TrimSpace(t)] = true
		}
	}

	// secret_store_entries can't be raw-copied: the cell encrypts via the
	// keyring ([2B version][nonce][ct]) but the edge expects bare [nonce][ct].
	// So we decrypt with the cell keyring and re-encrypt in the edge's bare
	// format using the shared base key. Needs OPENSANDBOX_SECRET_ENCRYPTION_KEY.
	var keyring *crypto.KeyRing
	var reenc *crypto.Encryptor
	if wanted == nil || wanted["secret_store_entries"] {
		baseKey := os.Getenv("OPENSANDBOX_SECRET_ENCRYPTION_KEY")
		if baseKey == "" {
			log.Fatal("OPENSANDBOX_SECRET_ENCRYPTION_KEY is required to backfill secret_store_entries (decrypt + re-encrypt); exclude it via --tables or set the key")
		}
		var err error
		if keyring, err = crypto.NewKeyRingFromEnv(); err != nil {
			log.Fatalf("build decrypt keyring: %v", err)
		}
		if reenc, err = crypto.NewEncryptor(baseKey); err != nil {
			log.Fatalf("build re-encryptor: %v", err)
		}
	}

	specs := buildSpecs(*cell, *homeCell, *blobPrefix, *sandboxStates, keyring, reenc)

	if *mode == "daemon" {
		runDaemon(ctx, conn, d1, specs, *since, *interval, *stateFile, *batchParams)
		return
	}

	log.Printf("backfill mode=%s since=%.0f cell=%s dry-run=%v", *mode, *since, *cell, *dryRun)
	var globalMaxWM float64
	for _, spec := range specs {
		if wanted != nil && !wanted[spec.d1Table] {
			continue
		}
		n, skipped, maxWM, err := syncTable(ctx, conn, d1, spec, full, *since, *batchParams)
		if err != nil {
			log.Fatalf("%s: %v", spec.d1Table, err)
		}
		if maxWM > globalMaxWM {
			globalMaxWM = maxWM
		}
		log.Printf("  %-24s synced=%-7d skipped=%-5d maxWatermark=%.3f", spec.d1Table, n, skipped, maxWM)
	}

	if globalMaxWM > 0 {
		log.Printf("DONE. Next run: --mode=delta --since=%.3f", globalMaxWM)
	} else {
		log.Printf("DONE. No rows matched (watermark unchanged).")
	}
}

// syncTable runs one table's PG query, assembles D1 rows, and upserts them in
// param-bounded batches. Returns synced count, skipped count, and max watermark.
func syncTable(ctx context.Context, conn *pgx.Conn, d1 *d1Client, spec tableSpec, full bool, since float64, batchParams int) (int, int, float64, error) {
	sql, args := spec.query(full)
	if !full {
		args = append(args, since)
	}
	rows, err := conn.Query(ctx, sql, args...)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("pg query: %w", err)
	}
	defer rows.Close()

	var batch [][]any
	synced, skipped := 0, 0
	var maxWM float64

	// rowsPerBatch keeps total bound params under batchParams. rawLiteral cells
	// don't count, but we conservatively size on the full column count.
	rowsPerBatch := max(batchParams/len(spec.cols), 1)

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := d1.upsert(ctx, spec, batch); err != nil {
			return err
		}
		synced += len(batch)
		batch = batch[:0]
		return nil
	}

	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return 0, 0, 0, fmt.Errorf("scan: %w", err)
		}
		row, wm, skip := spec.assemble(vals)
		if skip {
			skipped++
			continue
		}
		if wm > maxWM {
			maxWM = wm
		}
		batch = append(batch, row)
		if len(batch) >= rowsPerBatch {
			if err := flush(); err != nil {
				return 0, 0, 0, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return 0, 0, 0, fmt.Errorf("iterate: %w", err)
	}
	if err := flush(); err != nil {
		return 0, 0, 0, err
	}
	return synced, skipped, maxWM, nil
}

// ---- D1 HTTP client ------------------------------------------------------

type d1Client struct {
	http     *http.Client
	account  string
	database string
	token    string
	dryRun   bool
}

type d1Response struct {
	Success bool `json:"success"`
	Errors  []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
}

// upsert builds a multi-row INSERT ... ON CONFLICT DO UPDATE for the batch and
// executes it against D1. rawLiteral cells are inlined; all others are bound
// params.
func (c *d1Client) upsert(ctx context.Context, spec tableSpec, batch [][]any) error {
	var b strings.Builder
	fmt.Fprintf(&b, "INSERT INTO %s (%s) VALUES ", spec.d1Table, strings.Join(spec.cols, ", "))

	params := make([]any, 0, len(batch)*len(spec.cols))
	for i, row := range batch {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteByte('(')
		for j, v := range row {
			if j > 0 {
				b.WriteString(", ")
			}
			if lit, ok := v.(rawLiteral); ok {
				b.WriteString(string(lit))
			} else {
				b.WriteByte('?')
				params = append(params, v)
			}
		}
		b.WriteByte(')')
	}

	// ON CONFLICT upsert: update every non-PK column from the excluded row.
	pkSet := map[string]bool{}
	for _, k := range spec.pk {
		pkSet[k] = true
	}
	var setClauses []string
	for _, col := range spec.cols {
		if !pkSet[col] {
			setClauses = append(setClauses, fmt.Sprintf("%s = excluded.%s", col, col))
		}
	}
	fmt.Fprintf(&b, " ON CONFLICT(%s) DO UPDATE SET %s",
		strings.Join(spec.pk, ", "), strings.Join(setClauses, ", "))

	if c.dryRun {
		return nil
	}
	return c.query(ctx, b.String(), params)
}

func (c *d1Client) query(ctx context.Context, sql string, params []any) error {
	body, _ := json.Marshal(map[string]any{"sql": sql, "params": params})
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/d1/database/%s/query", c.account, c.database)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var parsed d1Response
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return fmt.Errorf("decode d1 response (http %d): %w", resp.StatusCode, err)
	}
	if !parsed.Success {
		if len(parsed.Errors) > 0 {
			return fmt.Errorf("d1 error %d: %s", parsed.Errors[0].Code, parsed.Errors[0].Message)
		}
		return fmt.Errorf("d1 request failed (http %d)", resp.StatusCode)
	}
	return nil
}

// ---- helpers -------------------------------------------------------------

// toFloat64 normalizes a pgx scan result to float64; nil → 0. Used for the
// watermark (EXTRACT(EPOCH …)::float8), which is fractional so we don't
// re-sync the boundary second's rows on every delta tick.
func toFloat64(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int64:
		return float64(n)
	case int32:
		return float64(n)
	case int:
		return float64(n)
	}
	return 0
}

// where appends the watermark filter for delta/final modes.
func where(full bool, base, filter string) string {
	if full {
		return base
	}
	return base + " " + filter
}

// ---- table specs ---------------------------------------------------------

func buildSpecs(cell, homeCell, blobPrefix, sandboxStates string, keyring *crypto.KeyRing, reenc *crypto.Encryptor) []tableSpec {
	activeStates := map[string]bool{}
	for _, s := range strings.Split(sandboxStates, ",") {
		activeStates[strings.TrimSpace(s)] = true
	}

	return []tableSpec{
		// orgs ---------------------------------------------------------------
		{
			d1Table: "orgs",
			pk:      []string{"id"},
			cols: []string{
				"id", "name", "slug", "plan", "home_cell", "stripe_customer_id",
				"stripe_subscription_id", "workos_org_id", "is_personal", "owner_user_id",
				"created_at", "updated_at", "is_halted", "halted_at", "custom_domain",
				"cf_hostname_id", "domain_verification_status", "domain_ssl_status",
				"verification_txt_name", "verification_txt_value", "ssl_txt_name",
				"ssl_txt_value", "free_credits_remaining_cents", "credit_balance_cents",
				"max_concurrent_sandboxes", "max_sandbox_timeout_sec", "max_disk_mb",
				"max_memory_gb", "billing_mode", "last_usage_reported_at",
			},
			query: func(full bool) (string, []any) {
				return where(full, `
					SELECT id::text, name, slug, plan, stripe_customer_id, stripe_subscription_id,
					       workos_org_id, is_personal::int, owner_user_id::text,
					       EXTRACT(EPOCH FROM created_at)::bigint, EXTRACT(EPOCH FROM updated_at)::bigint,
					       custom_domain, cf_hostname_id, domain_verification_status, domain_ssl_status,
					       verification_txt_name, verification_txt_value, ssl_txt_name, ssl_txt_value,
					       free_credits_remaining_cents, credit_balance_cents,
					       max_concurrent_sandboxes, max_sandbox_timeout_sec, max_disk_mb, max_memory_gb,
					       billing_mode, EXTRACT(EPOCH FROM last_usage_reported_at)::bigint,
					       EXTRACT(EPOCH FROM updated_at)::float8 AS _wm
					  FROM orgs`,
					`WHERE updated_at > to_timestamp($1)`), nil
			},
			assemble: func(s []any) ([]any, float64, bool) {
				wm := toFloat64(s[27])
				row := []any{
					s[0], s[1], s[2], s[3], homeCell, s[4], // id..stripe_customer_id, home_cell injected
					s[5], s[6], s[7], s[8], // stripe_sub, workos_org, is_personal, owner_user
					s[9], s[10], 0, nil, s[11], // created, updated, is_halted=0, halted_at=NULL, custom_domain
					s[12], s[13], s[14], // cf_hostname, dom_verif, dom_ssl
					s[15], s[16], s[17], s[18], // verif/ssl txt
					s[19], s[20], // free_credits, credit_balance
					s[21], s[22], s[23], s[24], // quotas
					s[25], s[26], // billing_mode, last_usage_reported_at
				}
				return row, wm, false
			},
		},

		// users --------------------------------------------------------------
		{
			d1Table: "users",
			pk:      []string{"id"},
			cols:    []string{"id", "email", "workos_user_id", "name", "created_at"},
			query: func(full bool) (string, []any) {
				return where(full, `
					SELECT id::text, email, workos_user_id, name,
					       EXTRACT(EPOCH FROM created_at)::bigint,
					       EXTRACT(EPOCH FROM updated_at)::float8 AS _wm
					  FROM users`,
					`WHERE updated_at > to_timestamp($1)`), nil
			},
			assemble: func(s []any) ([]any, float64, bool) {
				return []any{s[0], s[1], s[2], s[3], s[4]}, toFloat64(s[5]), false
			},
		},

		// org_memberships (derived from users.org_id/role) ------------------
		{
			d1Table: "org_memberships",
			pk:      []string{"org_id", "user_id"},
			cols:    []string{"org_id", "user_id", "role", "created_at"},
			query: func(full bool) (string, []any) {
				return where(full, `
					SELECT org_id::text, id::text, role,
					       EXTRACT(EPOCH FROM created_at)::bigint,
					       EXTRACT(EPOCH FROM updated_at)::float8 AS _wm
					  FROM users`,
					`WHERE updated_at > to_timestamp($1)`), nil
			},
			assemble: func(s []any) ([]any, float64, bool) {
				return []any{s[0], s[1], s[2], s[3]}, toFloat64(s[4]), false
			},
		},

		// api_keys -----------------------------------------------------------
		{
			d1Table: "api_keys",
			pk:      []string{"id"},
			cols: []string{"id", "org_id", "created_by", "key_hash", "key_prefix", "name",
				"scopes", "last_used", "expires_at", "created_at"},
			query: func(full bool) (string, []any) {
				return where(full, `
					SELECT id::text, org_id::text, created_by::text, key_hash, key_prefix, name,
					       array_to_json(scopes)::text,
					       EXTRACT(EPOCH FROM last_used)::bigint, EXTRACT(EPOCH FROM expires_at)::bigint,
					       EXTRACT(EPOCH FROM created_at)::bigint,
					       EXTRACT(EPOCH FROM updated_at)::float8 AS _wm
					  FROM api_keys`,
					`WHERE updated_at > to_timestamp($1)`), nil
			},
			assemble: func(s []any) ([]any, float64, bool) {
				return []any{s[0], s[1], s[2], s[3], s[4], s[5], s[6], s[7], s[8], s[9]}, toFloat64(s[10]), false
			},
		},

		// templates ----------------------------------------------------------
		{
			d1Table: "templates",
			pk:      []string{"id"},
			cols: []string{"id", "org_id", "name", "tag", "template_type", "image_ref",
				"rootfs_s3_key", "workspace_s3_key", "dockerfile", "is_public", "status",
				"cells_available", "created_at"},
			query: func(full bool) (string, []any) {
				return where(full, `
					SELECT id::text, org_id::text, name, tag, template_type, image_ref,
					       rootfs_s3_key, workspace_s3_key, dockerfile, is_public::int, status,
					       EXTRACT(EPOCH FROM created_at)::bigint,
					       EXTRACT(EPOCH FROM updated_at)::float8 AS _wm
					  FROM templates`,
					`WHERE updated_at > to_timestamp($1)`), nil
			},
			assemble: func(s []any) ([]any, float64, bool) {
				row := []any{s[0], s[1], s[2], s[3], s[4], s[5], s[6], s[7], s[8], s[9], s[10], "[]", s[11]}
				return row, toFloat64(s[12]), false
			},
		},

		// secret_stores ------------------------------------------------------
		{
			d1Table: "secret_stores",
			pk:      []string{"id"},
			cols:    []string{"id", "org_id", "name", "egress_allowlist", "created_at", "updated_at"},
			query: func(full bool) (string, []any) {
				return where(full, `
					SELECT id::text, org_id::text, name, array_to_json(egress_allowlist)::text,
					       EXTRACT(EPOCH FROM created_at)::bigint, EXTRACT(EPOCH FROM updated_at)::bigint,
					       EXTRACT(EPOCH FROM updated_at)::float8 AS _wm
					  FROM secret_stores`,
					`WHERE updated_at > to_timestamp($1)`), nil
			},
			assemble: func(s []any) ([]any, float64, bool) {
				return []any{s[0], s[1], s[2], s[3], s[4], s[5]}, toFloat64(s[6]), false
			},
		},

		// secret_store_entries (BYTEA → BLOB literal) ------------------------
		{
			d1Table: "secret_store_entries",
			pk:      []string{"id"},
			cols:    []string{"id", "store_id", "name", "encrypted_value", "allowed_hosts", "created_at", "updated_at"},
			query: func(full bool) (string, []any) {
				return where(full, `
					SELECT id::text, store_id::text, name, encode(encrypted_value, 'hex'),
					       array_to_json(allowed_hosts)::text,
					       EXTRACT(EPOCH FROM created_at)::bigint, EXTRACT(EPOCH FROM updated_at)::bigint,
					       EXTRACT(EPOCH FROM updated_at)::float8 AS _wm
					  FROM secret_store_entries`,
					`WHERE updated_at > to_timestamp($1)`), nil
			},
			assemble: func(s []any) ([]any, float64, bool) {
				hexCt, _ := s[3].(string)
				raw, err := hex.DecodeString(hexCt)
				if err != nil {
					log.Printf("  secret_store_entries %v: bad hex — SKIPPED: %v", s[0], err)
					return nil, 0, true
				}
				// Decrypt with the cell keyring (strips version header, tries
				// rotated keys), re-encrypt bare so the edge can read it.
				plaintext, err := keyring.Decrypt(raw)
				if err != nil {
					log.Printf("  secret_store_entries %v: DECRYPT FAILED (investigate — real secret won't survive cutover) — SKIPPED: %v", s[0], err)
					return nil, 0, true
				}
				reencrypted, err := reenc.Encrypt(plaintext)
				if err != nil {
					log.Printf("  secret_store_entries %v: re-encrypt failed — SKIPPED: %v", s[0], err)
					return nil, 0, true
				}
				ev := rawLiteral("X'" + hex.EncodeToString(reencrypted) + "'")
				return []any{s[0], s[1], s[2], ev, s[4], s[5], s[6]}, toFloat64(s[7]), false
			},
		},

		// agent_subscriptions (shape mismatch) -------------------------------
		{
			d1Table: "agent_subscriptions",
			pk:      []string{"id"},
			cols:    []string{"id", "org_id", "agent_id", "feature", "status", "stripe_item_id", "created_at", "cancelled_at"},
			query: func(full bool) (string, []any) {
				return where(full, `
					SELECT id::text, org_id::text, agent_id, feature, status, stripe_subscription_id,
					       EXTRACT(EPOCH FROM created_at)::bigint, EXTRACT(EPOCH FROM canceled_at)::bigint,
					       EXTRACT(EPOCH FROM updated_at)::float8 AS _wm
					  FROM agent_subscriptions`,
					`WHERE updated_at > to_timestamp($1)`), nil
			},
			assemble: func(s []any) ([]any, float64, bool) {
				return []any{s[0], s[1], s[2], s[3], s[4], s[5], s[6], s[7]}, toFloat64(s[8]), false
			},
		},

		// org_subscription_items (memory_mb → tier) --------------------------
		{
			d1Table: "org_subscription_items",
			pk:      []string{"org_id", "tier"},
			cols:    []string{"org_id", "tier", "stripe_item_id", "price_id"},
			query: func(full bool) (string, []any) {
				return where(full, `
					SELECT org_id::text, memory_mb::text, stripe_subscription_item_id,
					       EXTRACT(EPOCH FROM updated_at)::float8 AS _wm
					  FROM org_subscription_items`,
					`WHERE updated_at > to_timestamp($1)`), nil
			},
			assemble: func(s []any) ([]any, float64, bool) {
				return []any{s[0], s[1], s[2], ""}, toFloat64(s[3]), false
			},
		},

		// sandboxes_index (latest session per sandbox, active states) --------
		{
			d1Table: "sandboxes_index",
			pk:      []string{"id"},
			cols:    []string{"id", "org_id", "user_id", "cell_id", "worker_id", "status", "template_id", "created_at", "last_event_at", "stopped_at"},
			query: func(full bool) (string, []any) {
				return where(full, `
					SELECT DISTINCT ON (sandbox_id)
					       sandbox_id, org_id::text, user_id::text, worker_id, status,
					       based_on_template_id::text,
					       EXTRACT(EPOCH FROM started_at)::bigint,
					       EXTRACT(EPOCH FROM COALESCE(stopped_at, started_at))::bigint,
					       EXTRACT(EPOCH FROM stopped_at)::bigint,
					       EXTRACT(EPOCH FROM updated_at)::float8 AS _wm
					  FROM sandbox_sessions`,
					`WHERE updated_at > to_timestamp($1)`) +
					` ORDER BY sandbox_id, started_at DESC`, nil
			},
			assemble: func(s []any) ([]any, float64, bool) {
				status, _ := s[4].(string)
				if !activeStates[status] {
					return nil, 0, true
				}
				row := []any{s[0], s[1], s[2], cell, s[3], s[4], s[5], s[6], s[7], s[8]}
				return row, toFloat64(s[9]), false
			},
		},

		// checkpoints_index (golden_hash from session; skip unrecoverable) ---
		{
			d1Table: "checkpoints_index",
			pk:      []string{"id"},
			cols:    []string{"id", "sandbox_id", "org_id", "owner_cell_id", "s3_url", "size_bytes", "golden_hash", "workspace_size", "created_at", "expires_at", "replicated_to"},
			query: func(full bool) (string, []any) {
				return where(full, `
					SELECT c.id::text, c.sandbox_id, c.org_id::text, c.rootfs_s3_key, c.size_bytes,
					       s.golden_version,
					       EXTRACT(EPOCH FROM c.created_at)::bigint,
					       EXTRACT(EPOCH FROM c.updated_at)::float8 AS _wm
					  FROM sandbox_checkpoints c
					  LEFT JOIN LATERAL (
					      SELECT golden_version FROM sandbox_sessions ss
					       WHERE ss.sandbox_id = c.sandbox_id AND ss.golden_version IS NOT NULL
					       ORDER BY ss.started_at DESC LIMIT 1
					  ) s ON true
					 WHERE c.status = 'ready'`,
					`AND c.updated_at > to_timestamp($1)`), nil
			},
			assemble: func(s []any) ([]any, float64, bool) {
				golden, _ := s[5].(string)
				rootfsKey, _ := s[3].(string)
				if golden == "" || rootfsKey == "" {
					return nil, 0, true // unrecoverable golden or missing bytes — home-cell restorable
				}
				row := []any{
					s[0], s[1], s[2], cell, blobPrefix + rootfsKey, s[4], golden,
					nil, s[6], nil, "[]", // workspace_size NULL, expires_at NULL, replicated_to '[]'
				}
				return row, toFloat64(s[7]), false
			},
		},
	}
}
