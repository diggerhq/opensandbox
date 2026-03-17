package db

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store provides data access to the global PostgreSQL database.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore creates a new Store with a connection pool.
func NewStore(ctx context.Context, databaseURL string) (*Store, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close closes the connection pool.
func (s *Store) Close() {
	s.pool.Close()
}

// Migrate runs database migrations.
func (s *Store) Migrate(ctx context.Context) error {
	// Create migrations tracking table
	_, err := s.pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version INT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)
	`)
	if err != nil {
		return fmt.Errorf("failed to create migrations table: %w", err)
	}

	// Check current version
	var currentVersion int
	err = s.pool.QueryRow(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&currentVersion)
	if err != nil {
		return fmt.Errorf("failed to get current migration version: %w", err)
	}

	migrations := []struct {
		version  int
		filename string
	}{
		{1, "migrations/001_initial.up.sql"},
		{2, "migrations/002_user_sessions.up.sql"},
		{3, "migrations/003_checkpoint_hibernation.up.sql"},
		{4, "migrations/004_seed_templates.up.sql"},
		{5, "migrations/005_org_custom_domain.up.sql"},
		{6, "migrations/006_sandbox_preview_urls.up.sql"},
		{7, "migrations/007_preview_urls_port.up.sql"},
		{8, "migrations/008_default_template.up.sql"},
		{9, "migrations/009_sandbox_templates.up.sql"},
		{10, "migrations/010_template_status.up.sql"},
		{11, "migrations/011_rename_hibernation.up.sql"},
		{12, "migrations/012_checkpoints.up.sql"},
		{13, "migrations/013_checkpoint_patches.up.sql"},
		{14, "migrations/014_image_cache.up.sql"},
	}

	for _, m := range migrations {
		if currentVersion >= m.version {
			continue
		}
		sql, err := migrationsFS.ReadFile(m.filename)
		if err != nil {
			return fmt.Errorf("failed to read migration file %s: %w", m.filename, err)
		}
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("failed to begin transaction for migration %d: %w", m.version, err)
		}
		defer tx.Rollback(ctx)

		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("failed to apply migration %03d: %w", m.version, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, m.version); err != nil {
			return fmt.Errorf("failed to record migration %03d: %w", m.version, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("failed to commit migration %03d: %w", m.version, err)
		}
	}

	return nil
}

// --- Org operations ---

type Org struct {
	ID                     uuid.UUID `json:"id"`
	Name                   string    `json:"name"`
	Slug                   string    `json:"slug"`
	Plan                   string    `json:"plan"`
	MaxConcurrentSandboxes int       `json:"maxConcurrentSandboxes"`
	MaxSandboxTimeoutSec   int       `json:"maxSandboxTimeoutSec"`
	CreatedAt              time.Time `json:"createdAt"`
	UpdatedAt              time.Time `json:"updatedAt"`

	// Custom domain fields
	CustomDomain               *string `json:"customDomain,omitempty"`
	CFHostnameID               *string `json:"cfHostnameId,omitempty"`
	DomainVerificationStatus   string  `json:"domainVerificationStatus"`
	DomainSSLStatus            string  `json:"domainSslStatus"`
	VerificationTxtName        *string `json:"verificationTxtName,omitempty"`
	VerificationTxtValue       *string `json:"verificationTxtValue,omitempty"`
	SSLTxtName                 *string `json:"sslTxtName,omitempty"`
	SSLTxtValue                *string `json:"sslTxtValue,omitempty"`
}

// orgColumns is the list of columns returned by all Org queries.
const orgColumns = `id, name, slug, plan, max_concurrent_sandboxes, max_sandbox_timeout_sec, created_at, updated_at,
	custom_domain, cf_hostname_id, domain_verification_status, domain_ssl_status,
	verification_txt_name, verification_txt_value, ssl_txt_name, ssl_txt_value`

// scanOrg scans a row into an Org struct.
func scanOrg(row pgx.Row) (*Org, error) {
	org := &Org{}
	err := row.Scan(
		&org.ID, &org.Name, &org.Slug, &org.Plan, &org.MaxConcurrentSandboxes,
		&org.MaxSandboxTimeoutSec, &org.CreatedAt, &org.UpdatedAt,
		&org.CustomDomain, &org.CFHostnameID, &org.DomainVerificationStatus, &org.DomainSSLStatus,
		&org.VerificationTxtName, &org.VerificationTxtValue, &org.SSLTxtName, &org.SSLTxtValue,
	)
	return org, err
}

func (s *Store) CreateOrg(ctx context.Context, name, slug string) (*Org, error) {
	org, err := scanOrg(s.pool.QueryRow(ctx,
		`INSERT INTO orgs (name, slug) VALUES ($1, $2)
		 RETURNING `+orgColumns,
		name, slug,
	))
	if err != nil {
		return nil, fmt.Errorf("failed to create org: %w", err)
	}
	return org, nil
}

func (s *Store) GetOrg(ctx context.Context, id uuid.UUID) (*Org, error) {
	org, err := scanOrg(s.pool.QueryRow(ctx,
		`SELECT `+orgColumns+` FROM orgs WHERE id = $1`, id,
	))
	if err != nil {
		return nil, fmt.Errorf("org not found: %w", err)
	}
	return org, nil
}

func (s *Store) GetOrgBySlug(ctx context.Context, slug string) (*Org, error) {
	org, err := scanOrg(s.pool.QueryRow(ctx,
		`SELECT `+orgColumns+` FROM orgs WHERE slug = $1`, slug,
	))
	if err != nil {
		return nil, fmt.Errorf("org not found: %w", err)
	}
	return org, nil
}

func (s *Store) UpdateOrg(ctx context.Context, id uuid.UUID, name string) (*Org, error) {
	org, err := scanOrg(s.pool.QueryRow(ctx,
		`UPDATE orgs SET name = $1, updated_at = now() WHERE id = $2
		 RETURNING `+orgColumns,
		name, id,
	))
	if err != nil {
		return nil, fmt.Errorf("failed to update org: %w", err)
	}
	return org, nil
}

// SetOrgCustomDomain sets the custom domain and Cloudflare hostname info for an org.
func (s *Store) SetOrgCustomDomain(ctx context.Context, orgID uuid.UUID, domain, cfHostnameID, verificationStatus, sslStatus string, verificationTxtName, verificationTxtValue, sslTxtName, sslTxtValue *string) (*Org, error) {
	org, err := scanOrg(s.pool.QueryRow(ctx,
		`UPDATE orgs SET
			custom_domain = $1, cf_hostname_id = $2,
			domain_verification_status = $3, domain_ssl_status = $4,
			verification_txt_name = $5, verification_txt_value = $6,
			ssl_txt_name = $7, ssl_txt_value = $8,
			updated_at = now()
		 WHERE id = $9
		 RETURNING `+orgColumns,
		domain, cfHostnameID, verificationStatus, sslStatus,
		verificationTxtName, verificationTxtValue, sslTxtName, sslTxtValue,
		orgID,
	))
	if err != nil {
		return nil, fmt.Errorf("failed to set custom domain: %w", err)
	}
	return org, nil
}

// ClearOrgCustomDomain removes the custom domain from an org.
func (s *Store) ClearOrgCustomDomain(ctx context.Context, orgID uuid.UUID) (*Org, error) {
	org, err := scanOrg(s.pool.QueryRow(ctx,
		`UPDATE orgs SET
			custom_domain = NULL, cf_hostname_id = NULL,
			domain_verification_status = 'none', domain_ssl_status = 'none',
			verification_txt_name = NULL, verification_txt_value = NULL,
			ssl_txt_name = NULL, ssl_txt_value = NULL,
			updated_at = now()
		 WHERE id = $1
		 RETURNING `+orgColumns,
		orgID,
	))
	if err != nil {
		return nil, fmt.Errorf("failed to clear custom domain: %w", err)
	}
	return org, nil
}

// UpdateOrgDomainStatus updates the verification and SSL status for an org's custom domain.
func (s *Store) UpdateOrgDomainStatus(ctx context.Context, orgID uuid.UUID, verificationStatus, sslStatus string, verificationTxtName, verificationTxtValue, sslTxtName, sslTxtValue *string) (*Org, error) {
	org, err := scanOrg(s.pool.QueryRow(ctx,
		`UPDATE orgs SET
			domain_verification_status = $1, domain_ssl_status = $2,
			verification_txt_name = $3, verification_txt_value = $4,
			ssl_txt_name = $5, ssl_txt_value = $6,
			updated_at = now()
		 WHERE id = $7
		 RETURNING `+orgColumns,
		verificationStatus, sslStatus,
		verificationTxtName, verificationTxtValue, sslTxtName, sslTxtValue,
		orgID,
	))
	if err != nil {
		return nil, fmt.Errorf("failed to update domain status: %w", err)
	}
	return org, nil
}

// --- User operations ---

type User struct {
	ID        uuid.UUID `json:"id"`
	OrgID     uuid.UUID `json:"orgId"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"createdAt"`
}

func (s *Store) CreateUser(ctx context.Context, orgID uuid.UUID, email, name, role string) (*User, error) {
	user := &User{}
	err := s.pool.QueryRow(ctx,
		`INSERT INTO users (org_id, email, name, role) VALUES ($1, $2, $3, $4)
		 RETURNING id, org_id, email, name, role, created_at`,
		orgID, email, name, role,
	).Scan(&user.ID, &user.OrgID, &user.Email, &user.Name, &user.Role, &user.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("failed to create user: %w", err)
	}
	return user, nil
}

func (s *Store) GetUserByEmail(ctx context.Context, email string) (*User, error) {
	user := &User{}
	err := s.pool.QueryRow(ctx,
		`SELECT id, org_id, email, name, role, created_at FROM users WHERE email = $1`, email,
	).Scan(&user.ID, &user.OrgID, &user.Email, &user.Name, &user.Role, &user.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("user not found: %w", err)
	}
	return user, nil
}

// --- API Key operations ---

type APIKey struct {
	ID        uuid.UUID  `json:"id"`
	OrgID     uuid.UUID  `json:"orgId"`
	CreatedBy *uuid.UUID `json:"createdBy,omitempty"`
	KeyPrefix string     `json:"keyPrefix"`
	Name      string     `json:"name"`
	Scopes    []string   `json:"scopes"`
	LastUsed  *time.Time `json:"lastUsed,omitempty"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
}

// HashAPIKey returns the SHA-256 hash of a plaintext API key.
func HashAPIKey(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

func (s *Store) CreateAPIKey(ctx context.Context, orgID uuid.UUID, createdBy *uuid.UUID, keyHash, keyPrefix, name string, scopes []string) (*APIKey, error) {
	apiKey := &APIKey{}
	err := s.pool.QueryRow(ctx,
		`INSERT INTO api_keys (org_id, created_by, key_hash, key_prefix, name, scopes)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id, org_id, created_by, key_prefix, name, scopes, created_at`,
		orgID, createdBy, keyHash, keyPrefix, name, scopes,
	).Scan(&apiKey.ID, &apiKey.OrgID, &apiKey.CreatedBy, &apiKey.KeyPrefix, &apiKey.Name,
		&apiKey.Scopes, &apiKey.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("failed to create API key: %w", err)
	}
	return apiKey, nil
}

// ValidateAPIKey looks up an API key by hash and returns the associated org ID.
func (s *Store) ValidateAPIKey(ctx context.Context, keyPlaintext string) (uuid.UUID, error) {
	hash := HashAPIKey(keyPlaintext)
	var orgID uuid.UUID
	var expiresAt *time.Time
	err := s.pool.QueryRow(ctx,
		`SELECT org_id, expires_at FROM api_keys WHERE key_hash = $1`, hash,
	).Scan(&orgID, &expiresAt)
	if err != nil {
		return uuid.Nil, fmt.Errorf("invalid API key")
	}
	if expiresAt != nil && expiresAt.Before(time.Now()) {
		return uuid.Nil, fmt.Errorf("API key expired")
	}
	// Update last_used
	_, _ = s.pool.Exec(ctx, `UPDATE api_keys SET last_used = now() WHERE key_hash = $1`, hash)
	return orgID, nil
}

func (s *Store) ListAPIKeys(ctx context.Context, orgID uuid.UUID) ([]APIKey, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, org_id, created_by, key_prefix, name, scopes, last_used, expires_at, created_at
		 FROM api_keys WHERE org_id = $1 ORDER BY created_at DESC`, orgID)
	if err != nil {
		return nil, fmt.Errorf("failed to list API keys: %w", err)
	}
	defer rows.Close()

	var keys []APIKey
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.ID, &k.OrgID, &k.CreatedBy, &k.KeyPrefix, &k.Name,
			&k.Scopes, &k.LastUsed, &k.ExpiresAt, &k.CreatedAt); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, nil
}

func (s *Store) DeleteAPIKey(ctx context.Context, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM api_keys WHERE id = $1`, id)
	return err
}

// DeleteAPIKeyForOrg deletes an API key only if it belongs to the given org.
func (s *Store) DeleteAPIKeyForOrg(ctx context.Context, id uuid.UUID, orgID uuid.UUID) error {
	result, err := s.pool.Exec(ctx, `DELETE FROM api_keys WHERE id = $1 AND org_id = $2`, id, orgID)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return fmt.Errorf("api key not found or not owned by this org")
	}
	return nil
}

// --- Sandbox Session operations ---

type SandboxSession struct {
	ID                   uuid.UUID       `json:"id"`
	SandboxID            string          `json:"sandboxId"`
	OrgID                uuid.UUID       `json:"orgId"`
	UserID               *uuid.UUID      `json:"userId,omitempty"`
	Template             string          `json:"template"`
	Region               string          `json:"region"`
	WorkerID             string          `json:"workerId"`
	Status               string          `json:"status"`
	Config               json.RawMessage `json:"config"`
	Metadata             json.RawMessage `json:"metadata,omitempty"`
	StartedAt            time.Time       `json:"startedAt"`
	StoppedAt            *time.Time      `json:"stoppedAt,omitempty"`
	ErrorMsg             *string         `json:"errorMsg,omitempty"`
	BasedOnCheckpointID  *uuid.UUID      `json:"basedOnCheckpointId,omitempty"`
	LastPatchSequence    int             `json:"lastPatchSequence"`
}

func (s *Store) CreateSandboxSession(ctx context.Context, sandboxID string, orgID uuid.UUID, userID *uuid.UUID, template, region, workerID string, config, metadata json.RawMessage) (*SandboxSession, error) {
	session := &SandboxSession{}
	err := s.pool.QueryRow(ctx,
		`INSERT INTO sandbox_sessions (sandbox_id, org_id, user_id, template, region, worker_id, config, metadata)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 RETURNING id, sandbox_id, org_id, user_id, template, region, worker_id, status, config, metadata, started_at, based_on_checkpoint_id, last_patch_sequence`,
		sandboxID, orgID, userID, template, region, workerID, config, metadata,
	).Scan(&session.ID, &session.SandboxID, &session.OrgID, &session.UserID, &session.Template,
		&session.Region, &session.WorkerID, &session.Status, &session.Config, &session.Metadata, &session.StartedAt,
		&session.BasedOnCheckpointID, &session.LastPatchSequence)
	if err != nil {
		return nil, fmt.Errorf("failed to create sandbox session: %w", err)
	}
	return session, nil
}

func (s *Store) UpdateSandboxSessionStatus(ctx context.Context, sandboxID, status string, errorMsg *string) error {
	var query string
	var args []interface{}
	if status == "stopped" || status == "error" {
		query = `UPDATE sandbox_sessions SET status = $1, stopped_at = now(), error_msg = $2 WHERE sandbox_id = $3 AND status = 'running'`
		args = []interface{}{status, errorMsg, sandboxID}
	} else if status == "hibernated" {
		// Hibernated sandboxes are not stopped — don't set stopped_at
		query = `UPDATE sandbox_sessions SET status = $1 WHERE sandbox_id = $2 AND status = 'running'`
		args = []interface{}{status, sandboxID}
	} else {
		query = `UPDATE sandbox_sessions SET status = $1 WHERE sandbox_id = $2 AND status = 'running'`
		args = []interface{}{status, sandboxID}
	}
	_, err := s.pool.Exec(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("failed to update sandbox session: %w", err)
	}
	return nil
}

func (s *Store) GetSandboxSession(ctx context.Context, sandboxID string) (*SandboxSession, error) {
	session := &SandboxSession{}
	err := s.pool.QueryRow(ctx,
		`SELECT id, sandbox_id, org_id, user_id, template, region, worker_id, status, config, metadata, started_at, stopped_at, error_msg, based_on_checkpoint_id, last_patch_sequence
		 FROM sandbox_sessions WHERE sandbox_id = $1 ORDER BY started_at DESC LIMIT 1`, sandboxID,
	).Scan(&session.ID, &session.SandboxID, &session.OrgID, &session.UserID, &session.Template,
		&session.Region, &session.WorkerID, &session.Status, &session.Config, &session.Metadata,
		&session.StartedAt, &session.StoppedAt, &session.ErrorMsg, &session.BasedOnCheckpointID, &session.LastPatchSequence)
	if err != nil {
		return nil, fmt.Errorf("sandbox session not found: %w", err)
	}
	return session, nil
}

func (s *Store) ListSandboxSessions(ctx context.Context, orgID uuid.UUID, status string, limit, offset int) ([]SandboxSession, error) {
	var rows pgx.Rows
	var err error
	if status != "" {
		rows, err = s.pool.Query(ctx,
			`SELECT id, sandbox_id, org_id, user_id, template, region, worker_id, status, config, metadata, started_at, stopped_at, error_msg, based_on_checkpoint_id, last_patch_sequence
			 FROM sandbox_sessions WHERE org_id = $1 AND status = $2 ORDER BY started_at DESC LIMIT $3 OFFSET $4`,
			orgID, status, limit, offset)
	} else {
		rows, err = s.pool.Query(ctx,
			`SELECT id, sandbox_id, org_id, user_id, template, region, worker_id, status, config, metadata, started_at, stopped_at, error_msg, based_on_checkpoint_id, last_patch_sequence
			 FROM sandbox_sessions WHERE org_id = $1 ORDER BY started_at DESC LIMIT $2 OFFSET $3`,
			orgID, limit, offset)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to list sandbox sessions: %w", err)
	}
	defer rows.Close()

	var sessions []SandboxSession
	for rows.Next() {
		var sess SandboxSession
		if err := rows.Scan(&sess.ID, &sess.SandboxID, &sess.OrgID, &sess.UserID, &sess.Template,
			&sess.Region, &sess.WorkerID, &sess.Status, &sess.Config, &sess.Metadata,
			&sess.StartedAt, &sess.StoppedAt, &sess.ErrorMsg, &sess.BasedOnCheckpointID, &sess.LastPatchSequence); err != nil {
			return nil, err
		}
		sessions = append(sessions, sess)
	}
	return sessions, nil
}

func (s *Store) CountActiveSandboxes(ctx context.Context, orgID uuid.UUID) (int, error) {
	var count int
	err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM sandbox_sessions WHERE org_id = $1 AND status = 'running'`, orgID,
	).Scan(&count)
	return count, err
}

// --- Command Log operations (for NATS sync consumer) ---

type CommandLog struct {
	ID         uuid.UUID `json:"id"`
	SandboxID  string    `json:"sandboxId"`
	Command    string    `json:"command"`
	Args       []string  `json:"args,omitempty"`
	Cwd        string    `json:"cwd,omitempty"`
	ExitCode   *int      `json:"exitCode,omitempty"`
	DurationMs *int      `json:"durationMs,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
}

func (s *Store) InsertCommandLog(ctx context.Context, sandboxID, command string, args []string, cwd string, exitCode, durationMs *int) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO command_logs (sandbox_id, command, args, cwd, exit_code, duration_ms)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		sandboxID, command, args, cwd, exitCode, durationMs)
	return err
}

func (s *Store) InsertCommandLogBatch(ctx context.Context, logs []CommandLog) error {
	if len(logs) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, l := range logs {
		batch.Queue(
			`INSERT INTO command_logs (sandbox_id, command, args, cwd, exit_code, duration_ms, created_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			l.SandboxID, l.Command, l.Args, l.Cwd, l.ExitCode, l.DurationMs, l.CreatedAt)
	}
	br := s.pool.SendBatch(ctx, batch)
	defer br.Close()
	for range logs {
		if _, err := br.Exec(); err != nil {
			return fmt.Errorf("failed to insert command log batch: %w", err)
		}
	}
	return nil
}

// --- Worker Registry operations ---

type Worker struct {
	ID            string     `json:"id"`
	Region        string     `json:"region"`
	GRPCAddr      string     `json:"grpcAddr"`
	HTTPAddr      string     `json:"httpAddr"`
	Capacity      int        `json:"capacity"`
	CurrentCount  int        `json:"currentCount"`
	Status        string     `json:"status"`
	LastHeartbeat *time.Time `json:"lastHeartbeat,omitempty"`
	CreatedAt     time.Time  `json:"createdAt"`
}

func (s *Store) UpsertWorker(ctx context.Context, w *Worker) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO workers (id, region, grpc_addr, http_addr, capacity, current_count, status, last_heartbeat)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, now())
		 ON CONFLICT (id) DO UPDATE SET
		   current_count = EXCLUDED.current_count,
		   status = EXCLUDED.status,
		   last_heartbeat = now()`,
		w.ID, w.Region, w.GRPCAddr, w.HTTPAddr, w.Capacity, w.CurrentCount, w.Status)
	return err
}

func (s *Store) ListHealthyWorkers(ctx context.Context, region string) ([]Worker, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, region, grpc_addr, http_addr, capacity, current_count, status, last_heartbeat, created_at
		 FROM workers WHERE region = $1 AND status = 'healthy'
		 ORDER BY (capacity - current_count) DESC`, region)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var workers []Worker
	for rows.Next() {
		var w Worker
		if err := rows.Scan(&w.ID, &w.Region, &w.GRPCAddr, &w.HTTPAddr, &w.Capacity, &w.CurrentCount,
			&w.Status, &w.LastHeartbeat, &w.CreatedAt); err != nil {
			return nil, err
		}
		workers = append(workers, w)
	}
	return workers, nil
}

// --- User Session (access token) operations ---

// StoreAccessToken stores a WorkOS access token mapped to a user ID.
// Replaces any existing token for the user.
func (s *Store) StoreAccessToken(ctx context.Context, userID uuid.UUID, accessToken string) error {
	// Delete old sessions for this user
	_, _ = s.pool.Exec(ctx, `DELETE FROM user_sessions WHERE user_id = $1`, userID)
	// Insert new session
	_, err := s.pool.Exec(ctx,
		`INSERT INTO user_sessions (user_id, access_token) VALUES ($1, $2)`,
		userID, accessToken)
	return err
}

// GetUserByAccessToken looks up a user by their active access token.
func (s *Store) GetUserByAccessToken(ctx context.Context, accessToken string) (*User, error) {
	user := &User{}
	err := s.pool.QueryRow(ctx,
		`SELECT u.id, u.org_id, u.email, u.name, u.role, u.created_at
		 FROM users u
		 INNER JOIN user_sessions s ON s.user_id = u.id
		 WHERE s.access_token = $1 AND s.expires_at > now()`,
		accessToken,
	).Scan(&user.ID, &user.OrgID, &user.Email, &user.Name, &user.Role, &user.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("session not found or expired: %w", err)
	}
	return user, nil
}

// DeleteAccessTokensForUser removes all sessions for a user (logout).
func (s *Store) DeleteAccessTokensForUser(ctx context.Context, userID uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM user_sessions WHERE user_id = $1`, userID)
	return err
}

// Pool returns the underlying pgx pool for advanced use cases.
func (s *Store) Pool() *pgxpool.Pool {
	return s.pool
}

// --- Hibernation operations ---

// SandboxHibernation represents a hibernated sandbox's record.
type SandboxHibernation struct {
	ID             uuid.UUID       `json:"id"`
	SandboxID      string          `json:"sandboxId"`
	OrgID          uuid.UUID       `json:"orgId"`
	HibernationKey string          `json:"hibernationKey"`
	SizeBytes      int64           `json:"sizeBytes"`
	Region         string          `json:"region"`
	Template       string          `json:"template"`
	SandboxConfig  json.RawMessage `json:"sandboxConfig"`
	HibernatedAt   time.Time       `json:"hibernatedAt"`
	RestoredAt     *time.Time      `json:"restoredAt,omitempty"`
	ExpiredAt      *time.Time      `json:"expiredAt,omitempty"`
}

// CreateHibernation inserts a new hibernation record.
func (s *Store) CreateHibernation(ctx context.Context, sandboxID string, orgID uuid.UUID, hibernationKey string, sizeBytes int64, region, template string, sandboxConfig json.RawMessage) (*SandboxHibernation, error) {
	cp := &SandboxHibernation{}
	err := s.pool.QueryRow(ctx,
		`INSERT INTO sandbox_hibernations (sandbox_id, org_id, hibernation_key, size_bytes, region, template, sandbox_config)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 RETURNING id, sandbox_id, org_id, hibernation_key, size_bytes, region, template, sandbox_config, hibernated_at`,
		sandboxID, orgID, hibernationKey, sizeBytes, region, template, sandboxConfig,
	).Scan(&cp.ID, &cp.SandboxID, &cp.OrgID, &cp.HibernationKey, &cp.SizeBytes,
		&cp.Region, &cp.Template, &cp.SandboxConfig, &cp.HibernatedAt)
	if err != nil {
		return nil, fmt.Errorf("failed to create hibernation: %w", err)
	}
	return cp, nil
}

// GetActiveHibernation returns the active (not restored, not expired) hibernation for a sandbox.
func (s *Store) GetActiveHibernation(ctx context.Context, sandboxID string) (*SandboxHibernation, error) {
	cp := &SandboxHibernation{}
	err := s.pool.QueryRow(ctx,
		`SELECT id, sandbox_id, org_id, hibernation_key, size_bytes, region, template, sandbox_config, hibernated_at, restored_at, expired_at
		 FROM sandbox_hibernations
		 WHERE sandbox_id = $1 AND restored_at IS NULL AND expired_at IS NULL`, sandboxID,
	).Scan(&cp.ID, &cp.SandboxID, &cp.OrgID, &cp.HibernationKey, &cp.SizeBytes,
		&cp.Region, &cp.Template, &cp.SandboxConfig, &cp.HibernatedAt, &cp.RestoredAt, &cp.ExpiredAt)
	if err != nil {
		return nil, fmt.Errorf("active hibernation not found: %w", err)
	}
	return cp, nil
}

// MarkHibernationRestored marks the active hibernation for a sandbox as restored.
func (s *Store) MarkHibernationRestored(ctx context.Context, sandboxID string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE sandbox_hibernations SET restored_at = now()
		 WHERE sandbox_id = $1 AND restored_at IS NULL AND expired_at IS NULL`,
		sandboxID)
	return err
}

// UpdateSandboxSessionForWake changes a hibernated session back to running on a new worker.
func (s *Store) UpdateSandboxSessionForWake(ctx context.Context, sandboxID, newWorkerID string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE sandbox_sessions SET status = 'running', worker_id = $1, stopped_at = NULL
		 WHERE sandbox_id = $2 AND status = 'hibernated'`,
		newWorkerID, sandboxID)
	return err
}

// ReconcileWorkerSessions marks stale "running" sessions for a worker on startup.
// Sessions with an active checkpoint are set to "hibernated" (recoverable via wake-on-request).
// Sessions without a checkpoint are set to "stopped" (VM is gone, no recovery possible).
// Returns the count of sessions transitioned to each state.
func (s *Store) ReconcileWorkerSessions(ctx context.Context, workerID string) (hibernated, stopped int, err error) {
	// First: mark sessions that have an active hibernation as "hibernated"
	res1, err := s.pool.Exec(ctx,
		`UPDATE sandbox_sessions SET status = 'hibernated'
		 WHERE worker_id = $1 AND status = 'running'
		 AND sandbox_id IN (
		     SELECT sandbox_id FROM sandbox_hibernations
		     WHERE restored_at IS NULL AND expired_at IS NULL
		 )`, workerID)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to reconcile hibernated sessions: %w", err)
	}

	// Second: mark remaining "running" sessions as "stopped"
	res2, err := s.pool.Exec(ctx,
		`UPDATE sandbox_sessions SET status = 'stopped', stopped_at = now(),
		 error_msg = 'worker restarted'
		 WHERE worker_id = $1 AND status = 'running'`, workerID)
	if err != nil {
		return int(res1.RowsAffected()), 0, fmt.Errorf("failed to reconcile stopped sessions: %w", err)
	}

	return int(res1.RowsAffected()), int(res2.RowsAffected()), nil
}

// UpsertWorkspaceBackup creates or updates a workspace-only backup record for a sandbox.
// Uses hibernation_key prefix "workspace-backups/" to distinguish from full hibernation records.
// Only one workspace backup is kept per sandbox (previous is overwritten).
func (s *Store) UpsertWorkspaceBackup(ctx context.Context, sandboxID string, orgID uuid.UUID, backupKey string, sizeBytes int64, region, template string, sandboxConfig json.RawMessage) error {
	// Expire any existing workspace backups for this sandbox
	_, _ = s.pool.Exec(ctx,
		`UPDATE sandbox_hibernations SET expired_at = now()
		 WHERE sandbox_id = $1 AND hibernation_key LIKE 'workspace-backups/%'
		 AND expired_at IS NULL AND restored_at IS NULL`, sandboxID)

	// Insert the new backup
	_, err := s.pool.Exec(ctx,
		`INSERT INTO sandbox_hibernations (sandbox_id, org_id, hibernation_key, size_bytes, region, template, sandbox_config)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		sandboxID, orgID, backupKey, sizeBytes, region, template, sandboxConfig)
	if err != nil {
		return fmt.Errorf("failed to upsert workspace backup: %w", err)
	}
	return nil
}

// --- Checkpoint operations ---

// Checkpoint represents a named checkpoint for a sandbox.
type Checkpoint struct {
	ID              uuid.UUID       `json:"id"`
	SandboxID       string          `json:"sandboxId"`
	OrgID           uuid.UUID       `json:"orgId"`
	Name            string          `json:"name"`
	RootfsS3Key     *string         `json:"rootfsS3Key,omitempty"`
	WorkspaceS3Key  *string         `json:"workspaceS3Key,omitempty"`
	SandboxConfig   json.RawMessage `json:"sandboxConfig"`
	Status          string          `json:"status"`
	SizeBytes       int64           `json:"sizeBytes"`
	CreatedAt       time.Time       `json:"createdAt"`
}

// CreateCheckpoint inserts a new checkpoint record.
func (s *Store) CreateCheckpoint(ctx context.Context, cp *Checkpoint) error {
	return s.pool.QueryRow(ctx,
		`INSERT INTO sandbox_checkpoints (id, sandbox_id, org_id, name, sandbox_config)
		 VALUES ($1, $2, $3, $4, $5)
		 RETURNING created_at`,
		cp.ID, cp.SandboxID, cp.OrgID, cp.Name, cp.SandboxConfig,
	).Scan(&cp.CreatedAt)
}

// SetCheckpointReady marks a checkpoint as ready after async S3 upload completes.
func (s *Store) SetCheckpointReady(ctx context.Context, checkpointID uuid.UUID, rootfsKey, workspaceKey string, sizeBytes int64) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE sandbox_checkpoints SET status = 'ready', rootfs_s3_key = $2, workspace_s3_key = $3, size_bytes = $4
		 WHERE id = $1`,
		checkpointID, rootfsKey, workspaceKey, sizeBytes)
	return err
}

// SetCheckpointFailed marks a checkpoint as failed.
func (s *Store) SetCheckpointFailed(ctx context.Context, checkpointID uuid.UUID, reason string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE sandbox_checkpoints SET status = 'failed' WHERE id = $1`,
		checkpointID)
	return err
}

// GetCheckpoint returns a checkpoint by ID.
func (s *Store) GetCheckpoint(ctx context.Context, checkpointID uuid.UUID) (*Checkpoint, error) {
	cp := &Checkpoint{}
	err := s.pool.QueryRow(ctx,
		`SELECT id, sandbox_id, org_id, name, rootfs_s3_key, workspace_s3_key, sandbox_config, status, size_bytes, created_at
		 FROM sandbox_checkpoints WHERE id = $1`, checkpointID,
	).Scan(&cp.ID, &cp.SandboxID, &cp.OrgID, &cp.Name, &cp.RootfsS3Key, &cp.WorkspaceS3Key,
		&cp.SandboxConfig, &cp.Status, &cp.SizeBytes, &cp.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("checkpoint not found: %w", err)
	}
	return cp, nil
}

// ListCheckpoints returns all checkpoints for a sandbox, newest first.
func (s *Store) ListCheckpoints(ctx context.Context, sandboxID string) ([]Checkpoint, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, sandbox_id, org_id, name, rootfs_s3_key, workspace_s3_key, sandbox_config, status, size_bytes, created_at
		 FROM sandbox_checkpoints WHERE sandbox_id = $1 ORDER BY created_at DESC`, sandboxID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var checkpoints []Checkpoint
	for rows.Next() {
		var cp Checkpoint
		if err := rows.Scan(&cp.ID, &cp.SandboxID, &cp.OrgID, &cp.Name, &cp.RootfsS3Key, &cp.WorkspaceS3Key,
			&cp.SandboxConfig, &cp.Status, &cp.SizeBytes, &cp.CreatedAt); err != nil {
			return nil, err
		}
		checkpoints = append(checkpoints, cp)
	}
	return checkpoints, rows.Err()
}

// CheckpointWithForks extends Checkpoint with a count of active forked sandboxes.
type CheckpointWithForks struct {
	Checkpoint
	ActiveForks int `json:"activeForks"`
	TotalForks  int `json:"totalForks"`
}

// ListOrgCheckpoints returns all checkpoints for an org with fork counts, paginated.
func (s *Store) ListOrgCheckpoints(ctx context.Context, orgID uuid.UUID, limit, offset int) ([]CheckpointWithForks, int, error) {
	// Total count for pagination
	var total int
	if err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM sandbox_checkpoints WHERE org_id = $1`, orgID).Scan(&total); err != nil {
		return nil, 0, err
	}

	rows, err := s.pool.Query(ctx,
		`SELECT c.id, c.sandbox_id, c.org_id, c.name, c.rootfs_s3_key, c.workspace_s3_key,
		        c.sandbox_config, c.status, c.size_bytes, c.created_at,
		        (SELECT COUNT(*) FROM sandbox_sessions ss WHERE ss.based_on_checkpoint_id = c.id AND ss.status IN ('running', 'hibernated')) AS active_forks,
		        (SELECT COUNT(*) FROM sandbox_sessions ss WHERE ss.based_on_checkpoint_id = c.id) AS total_forks
		 FROM sandbox_checkpoints c WHERE c.org_id = $1
		 ORDER BY c.created_at DESC LIMIT $2 OFFSET $3`, orgID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var results []CheckpointWithForks
	for rows.Next() {
		var cf CheckpointWithForks
		if err := rows.Scan(&cf.ID, &cf.SandboxID, &cf.OrgID, &cf.Name, &cf.RootfsS3Key, &cf.WorkspaceS3Key,
			&cf.SandboxConfig, &cf.Status, &cf.SizeBytes, &cf.CreatedAt,
			&cf.ActiveForks, &cf.TotalForks); err != nil {
			return nil, 0, err
		}
		results = append(results, cf)
	}
	return results, total, rows.Err()
}

// GetCheckpointByName looks up a checkpoint by sandbox-scoped name.
func (s *Store) GetCheckpointByName(ctx context.Context, sandboxID, name string) (*Checkpoint, error) {
	cp := &Checkpoint{}
	err := s.pool.QueryRow(ctx,
		`SELECT id, sandbox_id, org_id, name, rootfs_s3_key, workspace_s3_key, sandbox_config, status, size_bytes, created_at
		 FROM sandbox_checkpoints WHERE sandbox_id = $1 AND name = $2`, sandboxID, name,
	).Scan(&cp.ID, &cp.SandboxID, &cp.OrgID, &cp.Name, &cp.RootfsS3Key, &cp.WorkspaceS3Key,
		&cp.SandboxConfig, &cp.Status, &cp.SizeBytes, &cp.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("checkpoint not found: %w", err)
	}
	return cp, nil
}

// CountCheckpoints returns the number of checkpoints for a sandbox.
func (s *Store) CountCheckpoints(ctx context.Context, sandboxID string) (int, error) {
	var count int
	err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM sandbox_checkpoints WHERE sandbox_id = $1`, sandboxID).Scan(&count)
	return count, err
}

// DeleteCheckpoint deletes a checkpoint (only if owned by org).
// Clears any sandbox_sessions FK references first to avoid constraint violations.
func (s *Store) DeleteCheckpoint(ctx context.Context, orgID uuid.UUID, checkpointID uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// Clear FK references from sandboxes forked from this checkpoint
	_, err = tx.Exec(ctx,
		`UPDATE sandbox_sessions SET based_on_checkpoint_id = NULL WHERE based_on_checkpoint_id = $1`,
		checkpointID)
	if err != nil {
		return fmt.Errorf("clear checkpoint references: %w", err)
	}

	tag, err := tx.Exec(ctx,
		`DELETE FROM sandbox_checkpoints WHERE id = $1 AND org_id = $2`,
		checkpointID, orgID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("checkpoint not found or not owned by org")
	}

	return tx.Commit(ctx)
}

// --- Checkpoint Patch operations ---

// CheckpointPatch represents a patch script attached to a checkpoint.
type CheckpointPatch struct {
	ID           uuid.UUID `json:"id"`
	CheckpointID uuid.UUID `json:"checkpointId"`
	Sequence     int       `json:"sequence"`
	Script       string    `json:"script"`
	Description  string    `json:"description"`
	Strategy     string    `json:"strategy"`
	CreatedAt    time.Time `json:"createdAt"`
}

// CreateCheckpointPatch inserts a new patch, auto-assigning the next sequence number.
func (s *Store) CreateCheckpointPatch(ctx context.Context, patch *CheckpointPatch) error {
	return s.pool.QueryRow(ctx,
		`INSERT INTO checkpoint_patches (id, checkpoint_id, sequence, script, description, strategy)
		 VALUES ($1, $2, COALESCE((SELECT MAX(sequence) FROM checkpoint_patches WHERE checkpoint_id = $2), 0) + 1, $3, $4, $5)
		 RETURNING sequence, created_at`,
		patch.ID, patch.CheckpointID, patch.Script, patch.Description, patch.Strategy,
	).Scan(&patch.Sequence, &patch.CreatedAt)
}

// ListCheckpointPatches returns all patches for a checkpoint, ordered by sequence.
func (s *Store) ListCheckpointPatches(ctx context.Context, checkpointID uuid.UUID) ([]CheckpointPatch, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, checkpoint_id, sequence, script, description, strategy, created_at
		 FROM checkpoint_patches WHERE checkpoint_id = $1 ORDER BY sequence ASC`, checkpointID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var patches []CheckpointPatch
	for rows.Next() {
		var p CheckpointPatch
		if err := rows.Scan(&p.ID, &p.CheckpointID, &p.Sequence, &p.Script, &p.Description, &p.Strategy, &p.CreatedAt); err != nil {
			return nil, err
		}
		patches = append(patches, p)
	}
	return patches, rows.Err()
}

// GetPendingPatches returns patches with sequence > afterSequence for a checkpoint.
func (s *Store) GetPendingPatches(ctx context.Context, checkpointID uuid.UUID, afterSequence int) ([]CheckpointPatch, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, checkpoint_id, sequence, script, description, strategy, created_at
		 FROM checkpoint_patches WHERE checkpoint_id = $1 AND sequence > $2 ORDER BY sequence ASC`,
		checkpointID, afterSequence)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var patches []CheckpointPatch
	for rows.Next() {
		var p CheckpointPatch
		if err := rows.Scan(&p.ID, &p.CheckpointID, &p.Sequence, &p.Script, &p.Description, &p.Strategy, &p.CreatedAt); err != nil {
			return nil, err
		}
		patches = append(patches, p)
	}
	return patches, rows.Err()
}

// DeleteCheckpointPatch deletes a patch by ID, scoped to a checkpoint.
func (s *Store) DeleteCheckpointPatch(ctx context.Context, checkpointID, patchID uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM checkpoint_patches WHERE id = $1 AND checkpoint_id = $2`,
		patchID, checkpointID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("patch not found")
	}
	return nil
}

// UpdateSandboxPatchSequence updates the last_patch_sequence for a sandbox session.
func (s *Store) UpdateSandboxPatchSequence(ctx context.Context, sandboxID string, sequence int) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE sandbox_sessions SET last_patch_sequence = $1 WHERE sandbox_id = $2`,
		sequence, sandboxID)
	return err
}

// SetSandboxCheckpointID sets the based_on_checkpoint_id for a sandbox session.
func (s *Store) SetSandboxCheckpointID(ctx context.Context, sandboxID string, checkpointID uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE sandbox_sessions SET based_on_checkpoint_id = $1 WHERE sandbox_id = $2`,
		checkpointID, sandboxID)
	return err
}

// ListSandboxesByCheckpoint returns all non-stopped sandboxes forked from a checkpoint.
func (s *Store) ListSandboxesByCheckpoint(ctx context.Context, checkpointID uuid.UUID) ([]SandboxSession, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, sandbox_id, org_id, user_id, template, region, worker_id, status, config, metadata, started_at, stopped_at, error_msg, based_on_checkpoint_id, last_patch_sequence
		 FROM sandbox_sessions WHERE based_on_checkpoint_id = $1 AND status IN ('running', 'hibernated') ORDER BY started_at DESC`,
		checkpointID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sessions []SandboxSession
	for rows.Next() {
		var sess SandboxSession
		if err := rows.Scan(&sess.ID, &sess.SandboxID, &sess.OrgID, &sess.UserID, &sess.Template,
			&sess.Region, &sess.WorkerID, &sess.Status, &sess.Config, &sess.Metadata,
			&sess.StartedAt, &sess.StoppedAt, &sess.ErrorMsg, &sess.BasedOnCheckpointID, &sess.LastPatchSequence); err != nil {
			return nil, err
		}
		sessions = append(sessions, sess)
	}
	return sessions, rows.Err()
}

// --- Template operations ---

// DBTemplate represents a template record in the database.
type DBTemplate struct {
	ID                 uuid.UUID  `json:"id"`
	OrgID              *uuid.UUID `json:"orgId,omitempty"`
	Name               string     `json:"name"`
	Tag                string     `json:"tag"`
	ImageRef           string     `json:"-"`
	Dockerfile         *string    `json:"dockerfile,omitempty"`
	IsPublic           bool       `json:"isPublic"`
	TemplateType       string     `json:"templateType"` // "dockerfile" or "sandbox"
	RootfsS3Key        *string    `json:"-"`
	WorkspaceS3Key     *string    `json:"-"`
	CreatedBySandboxID *string    `json:"createdBySandboxId,omitempty"`
	Status             string     `json:"status"` // "ready" or "processing"
	CreatedAt          time.Time  `json:"createdAt"`
}

// templateColumns is the standard column list for template queries.
const templateColumns = `id, org_id, name, tag, COALESCE(image_ref,''), dockerfile, is_public, template_type, rootfs_s3_key, workspace_s3_key, created_by_sandbox_id, COALESCE(status,'ready'), created_at`

func scanTemplate(row interface{ Scan(...any) error }, t *DBTemplate) error {
	return row.Scan(&t.ID, &t.OrgID, &t.Name, &t.Tag, &t.ImageRef, &t.Dockerfile, &t.IsPublic, &t.TemplateType, &t.RootfsS3Key, &t.WorkspaceS3Key, &t.CreatedBySandboxID, &t.Status, &t.CreatedAt)
}

// CreateSandboxTemplate inserts a new sandbox-snapshot template record (status=processing).
func (s *Store) CreateSandboxTemplate(ctx context.Context, id uuid.UUID, orgID *uuid.UUID, name, tag, rootfsS3Key, workspaceS3Key, createdBySandboxID string) (*DBTemplate, error) {
	t := &DBTemplate{}
	err := scanTemplate(s.pool.QueryRow(ctx,
		`INSERT INTO templates (id, org_id, name, tag, image_ref, is_public, template_type, rootfs_s3_key, workspace_s3_key, created_by_sandbox_id, status)
		 VALUES ($1, $2, $3, $4, '', false, 'sandbox', $5, $6, $7, 'processing')
		 RETURNING `+templateColumns,
		id, orgID, name, tag, rootfsS3Key, workspaceS3Key, createdBySandboxID,
	), t)
	if err != nil {
		return nil, fmt.Errorf("failed to create sandbox template: %w", err)
	}
	return t, nil
}

// SetTemplateReady marks a template as ready for use.
func (s *Store) SetTemplateReady(ctx context.Context, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `UPDATE templates SET status = 'ready' WHERE id = $1`, id)
	return err
}

// GetTemplateByID finds a template by its UUID.
func (s *Store) GetTemplateByID(ctx context.Context, id uuid.UUID) (*DBTemplate, error) {
	t := &DBTemplate{}
	err := scanTemplate(s.pool.QueryRow(ctx,
		`SELECT `+templateColumns+` FROM templates WHERE id = $1`, id,
	), t)
	if err != nil {
		return nil, fmt.Errorf("template %s not found: %w", id, err)
	}
	return t, nil
}

// UpdateSandboxSessionTemplate sets the based_on_template_id for a sandbox session.
func (s *Store) UpdateSandboxSessionTemplate(ctx context.Context, sandboxID string, templateID uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE sandbox_sessions SET based_on_template_id = $1 WHERE sandbox_id = $2`,
		templateID, sandboxID)
	return err
}

// GetTemplateByName finds a template by name, preferring org-specific over public.
func (s *Store) GetTemplateByName(ctx context.Context, orgID uuid.UUID, name string) (*DBTemplate, error) {
	t := &DBTemplate{}
	err := scanTemplate(s.pool.QueryRow(ctx,
		`SELECT `+templateColumns+`
		 FROM templates
		 WHERE name = $1 AND (org_id = $2 OR (is_public = true AND org_id IS NULL))
		 ORDER BY org_id IS NULL ASC
		 LIMIT 1`,
		name, orgID,
	), t)
	if err != nil {
		return nil, fmt.Errorf("template %q not found: %w", name, err)
	}
	return t, nil
}

// ListTemplates returns all templates visible to an org (org-specific + public).
func (s *Store) ListTemplates(ctx context.Context, orgID uuid.UUID) ([]DBTemplate, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+templateColumns+`
		 FROM templates
		 WHERE org_id = $1 OR (is_public = true AND org_id IS NULL)
		 ORDER BY is_public DESC, name ASC`,
		orgID)
	if err != nil {
		return nil, fmt.Errorf("failed to list templates: %w", err)
	}
	defer rows.Close()

	var templates []DBTemplate
	for rows.Next() {
		var t DBTemplate
		if err := scanTemplate(rows, &t); err != nil {
			return nil, err
		}
		templates = append(templates, t)
	}
	return templates, nil
}

// DeleteTemplateForOrg deletes a template only if it belongs to the given org (not public).
func (s *Store) DeleteTemplateForOrg(ctx context.Context, id uuid.UUID, orgID uuid.UUID) error {
	result, err := s.pool.Exec(ctx, `DELETE FROM templates WHERE id = $1 AND org_id = $2`, id, orgID)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return fmt.Errorf("template not found or not owned by this org")
	}
	return nil
}

// --- Image Cache operations ---

// ImageCache represents a cached image build (content-hashed manifest → checkpoint).
type ImageCache struct {
	ID           uuid.UUID       `json:"id"`
	OrgID        uuid.UUID       `json:"orgId"`
	ContentHash  string          `json:"contentHash"`
	CheckpointID *uuid.UUID      `json:"checkpointId,omitempty"`
	Name         *string         `json:"name,omitempty"` // nil for auto-cached, set for named snapshots
	Manifest     json.RawMessage `json:"manifest"`
	Status       string          `json:"status"` // building | ready | failed
	CreatedAt    time.Time       `json:"createdAt"`
	LastUsedAt   time.Time       `json:"lastUsedAt"`
}

// GetImageCacheByHash looks up a cached image by org + content hash.
func (s *Store) GetImageCacheByHash(ctx context.Context, orgID uuid.UUID, contentHash string) (*ImageCache, error) {
	ic := &ImageCache{}
	err := s.pool.QueryRow(ctx,
		`SELECT id, org_id, content_hash, checkpoint_id, name, manifest, status, created_at, last_used_at
		 FROM image_cache WHERE org_id = $1 AND content_hash = $2`,
		orgID, contentHash,
	).Scan(&ic.ID, &ic.OrgID, &ic.ContentHash, &ic.CheckpointID, &ic.Name, &ic.Manifest, &ic.Status, &ic.CreatedAt, &ic.LastUsedAt)
	if err != nil {
		return nil, fmt.Errorf("image cache not found: %w", err)
	}
	return ic, nil
}

// GetImageCacheByName looks up a named snapshot by org + name.
func (s *Store) GetImageCacheByName(ctx context.Context, orgID uuid.UUID, name string) (*ImageCache, error) {
	ic := &ImageCache{}
	err := s.pool.QueryRow(ctx,
		`SELECT id, org_id, content_hash, checkpoint_id, name, manifest, status, created_at, last_used_at
		 FROM image_cache WHERE org_id = $1 AND name = $2`,
		orgID, name,
	).Scan(&ic.ID, &ic.OrgID, &ic.ContentHash, &ic.CheckpointID, &ic.Name, &ic.Manifest, &ic.Status, &ic.CreatedAt, &ic.LastUsedAt)
	if err != nil {
		return nil, fmt.Errorf("snapshot %q not found: %w", name, err)
	}
	return ic, nil
}

// CreateImageCache inserts a new image cache record.
func (s *Store) CreateImageCache(ctx context.Context, ic *ImageCache) error {
	return s.pool.QueryRow(ctx,
		`INSERT INTO image_cache (id, org_id, content_hash, checkpoint_id, name, manifest, status)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 RETURNING created_at, last_used_at`,
		ic.ID, ic.OrgID, ic.ContentHash, ic.CheckpointID, ic.Name, ic.Manifest, ic.Status,
	).Scan(&ic.CreatedAt, &ic.LastUsedAt)
}

// SetImageCacheReady marks an image cache entry as ready with its checkpoint ID.
func (s *Store) SetImageCacheReady(ctx context.Context, id uuid.UUID, checkpointID uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE image_cache SET status = 'ready', checkpoint_id = $2, last_used_at = now() WHERE id = $1`,
		id, checkpointID)
	return err
}

// SetImageCacheFailed marks an image cache entry as failed.
func (s *Store) SetImageCacheFailed(ctx context.Context, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE image_cache SET status = 'failed' WHERE id = $1`, id)
	return err
}

// TouchImageCacheUsage updates the last_used_at timestamp.
func (s *Store) TouchImageCacheUsage(ctx context.Context, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE image_cache SET last_used_at = now() WHERE id = $1`, id)
	return err
}

// ListImageCacheByOrg returns all image cache entries for an org, newest first.
func (s *Store) ListImageCacheByOrg(ctx context.Context, orgID uuid.UUID, namedOnly bool) ([]ImageCache, error) {
	query := `SELECT id, org_id, content_hash, checkpoint_id, name, manifest, status, created_at, last_used_at
		 FROM image_cache WHERE org_id = $1`
	if namedOnly {
		query += ` AND name IS NOT NULL`
	}
	query += ` ORDER BY created_at DESC`

	rows, err := s.pool.Query(ctx, query, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []ImageCache
	for rows.Next() {
		var ic ImageCache
		if err := rows.Scan(&ic.ID, &ic.OrgID, &ic.ContentHash, &ic.CheckpointID, &ic.Name, &ic.Manifest, &ic.Status, &ic.CreatedAt, &ic.LastUsedAt); err != nil {
			return nil, err
		}
		results = append(results, ic)
	}
	return results, rows.Err()
}

// DeleteImageCache deletes an image cache entry by ID (org-scoped).
func (s *Store) DeleteImageCache(ctx context.Context, orgID uuid.UUID, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM image_cache WHERE id = $1 AND org_id = $2`, id, orgID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("image cache entry not found or not owned by org")
	}
	return nil
}

// DeleteImageCacheByName deletes a named snapshot by org + name.
func (s *Store) DeleteImageCacheByName(ctx context.Context, orgID uuid.UUID, name string) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM image_cache WHERE org_id = $1 AND name = $2`, orgID, name)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("snapshot %q not found", name)
	}
	return nil
}

// --- Preview URL operations ---

// PreviewURL represents an on-demand preview URL for a sandbox.
type PreviewURL struct {
	ID           uuid.UUID       `json:"id"`
	SandboxID    string          `json:"sandboxId"`
	OrgID        uuid.UUID       `json:"orgId"`
	Hostname     string          `json:"hostname"`
	Port         int             `json:"port"`
	CFHostnameID *string         `json:"cfHostnameId,omitempty"`
	SSLStatus    string          `json:"sslStatus"`
	AuthConfig   json.RawMessage `json:"authConfig"`
	CreatedAt    time.Time       `json:"createdAt"`
}

// CreatePreviewURL inserts a new preview URL record for a specific port.
func (s *Store) CreatePreviewURL(ctx context.Context, sandboxID string, orgID uuid.UUID, hostname string, port int, cfHostnameID *string, sslStatus string, authConfig json.RawMessage) (*PreviewURL, error) {
	p := &PreviewURL{}
	err := s.pool.QueryRow(ctx,
		`INSERT INTO sandbox_preview_urls (sandbox_id, org_id, hostname, port, cf_hostname_id, ssl_status, auth_config)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 RETURNING id, sandbox_id, org_id, hostname, port, cf_hostname_id, ssl_status, auth_config, created_at`,
		sandboxID, orgID, hostname, port, cfHostnameID, sslStatus, authConfig,
	).Scan(&p.ID, &p.SandboxID, &p.OrgID, &p.Hostname, &p.Port, &p.CFHostnameID, &p.SSLStatus, &p.AuthConfig, &p.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("failed to create preview URL: %w", err)
	}
	return p, nil
}

// ListPreviewURLs returns all preview URLs for a sandbox.
func (s *Store) ListPreviewURLs(ctx context.Context, sandboxID string) ([]PreviewURL, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, sandbox_id, org_id, hostname, port, cf_hostname_id, ssl_status, auth_config, created_at
		 FROM sandbox_preview_urls WHERE sandbox_id = $1 ORDER BY port`, sandboxID)
	if err != nil {
		return nil, fmt.Errorf("failed to list preview URLs: %w", err)
	}
	defer rows.Close()

	var urls []PreviewURL
	for rows.Next() {
		var p PreviewURL
		if err := rows.Scan(&p.ID, &p.SandboxID, &p.OrgID, &p.Hostname, &p.Port, &p.CFHostnameID, &p.SSLStatus, &p.AuthConfig, &p.CreatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan preview URL: %w", err)
		}
		urls = append(urls, p)
	}
	return urls, nil
}

// GetPreviewURLByPort returns the preview URL for a sandbox on a specific port.
func (s *Store) GetPreviewURLByPort(ctx context.Context, sandboxID string, port int) (*PreviewURL, error) {
	p := &PreviewURL{}
	err := s.pool.QueryRow(ctx,
		`SELECT id, sandbox_id, org_id, hostname, port, cf_hostname_id, ssl_status, auth_config, created_at
		 FROM sandbox_preview_urls WHERE sandbox_id = $1 AND port = $2`, sandboxID, port,
	).Scan(&p.ID, &p.SandboxID, &p.OrgID, &p.Hostname, &p.Port, &p.CFHostnameID, &p.SSLStatus, &p.AuthConfig, &p.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("preview URL not found: %w", err)
	}
	return p, nil
}

// DeletePreviewURL deletes a preview URL by ID.
func (s *Store) DeletePreviewURL(ctx context.Context, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM sandbox_preview_urls WHERE id = $1`, id)
	return err
}

// DeletePreviewURLsBySandbox deletes all preview URLs for a sandbox (cleanup on kill).
// Returns the deleted records so callers can clean up CF hostnames.
func (s *Store) DeletePreviewURLsBySandbox(ctx context.Context, sandboxID string) ([]PreviewURL, error) {
	rows, err := s.pool.Query(ctx,
		`DELETE FROM sandbox_preview_urls WHERE sandbox_id = $1
		 RETURNING id, sandbox_id, org_id, hostname, port, cf_hostname_id, ssl_status, auth_config, created_at`,
		sandboxID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var urls []PreviewURL
	for rows.Next() {
		var p PreviewURL
		if err := rows.Scan(&p.ID, &p.SandboxID, &p.OrgID, &p.Hostname, &p.Port, &p.CFHostnameID, &p.SSLStatus, &p.AuthConfig, &p.CreatedAt); err != nil {
			return nil, err
		}
		urls = append(urls, p)
	}
	return urls, nil
}
