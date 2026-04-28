package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ResourceMemoryGB is the only capacity resource in v1. CPU, fractional GB,
// and region are spec'd as out-of-scope until product calls land.
const ResourceMemoryGB = "memory_gb"

// CapacityInterval is one 15-minute slice of a reservation.
type CapacityInterval struct {
	StartsAt   time.Time `json:"startsAt"`
	EndsAt     time.Time `json:"endsAt"`
	CapacityGB int       `json:"capacityGb"`
}

// Reservation aggregates intervals that share a reservation_id.
type Reservation struct {
	ReservationID uuid.UUID          `json:"reservationId"`
	CreatedAt     time.Time          `json:"createdAt"`
	Intervals     []CapacityInterval `json:"intervals"`
}

// CalendarBucket is one 15-minute interval as returned by the calendar
// endpoint. ReservedGB is the per-org sum across all reservations for the
// bucket; the handler layers reservationLimitGb (org.MaxMemoryGB) on top.
type CalendarBucket struct {
	StartsAt   time.Time `json:"startsAt"`
	EndsAt     time.Time `json:"endsAt"`
	ReservedGB int       `json:"reservedGb"`
}

// IntervalCapacityShortfall is one entry in the capacity_not_available 409
// response — naming each interval that didn't fit, with the requested vs.
// remaining headroom.
type IntervalCapacityShortfall struct {
	StartsAt     time.Time `json:"startsAt"`
	RequestedGB  int       `json:"requestedGb"`
	ReservableGB int       `json:"reservableGb"`
	Reason       string    `json:"reason"`
}

// CapacityShortfallError carries per-interval shortfalls so the handler can
// build the 409 capacity_not_available body.
type CapacityShortfallError struct {
	Intervals []IntervalCapacityShortfall
}

func (e *CapacityShortfallError) Error() string {
	return fmt.Sprintf("capacity not available for %d interval(s)", len(e.Intervals))
}

// IdempotencyConflictError is returned when the same Idempotency-Key was
// previously used with a different request body. The handler maps it to
// 409 idempotency_key_conflict per design/001.
type IdempotencyConflictError struct{}

func (e *IdempotencyConflictError) Error() string { return "idempotency key conflict" }

// IdempotencyReplay is returned when the cached row exists for this key
// with a matching body hash. Handler responds with the cached status code
// and body verbatim — a cached 409 stays a 409.
type IdempotencyReplay struct {
	StatusCode   int
	ResponseBody json.RawMessage
}

func (e *IdempotencyReplay) Error() string { return "idempotency replay" }

// GetCapacityCalendar returns one bucket per 15-minute slice in [from, to).
// Both bounds must be UTC and aligned to :00/:15/:30/:45. The caller is
// responsible for span enforcement; the SQL just walks the series.
func (s *Store) GetCapacityCalendar(ctx context.Context, orgID uuid.UUID, from, to time.Time) ([]CalendarBucket, error) {
	rows, err := s.pool.Query(ctx, `
		WITH bucket(starts_at) AS (
			SELECT generate_series(
				$2::timestamptz,
				$3::timestamptz - INTERVAL '15 minutes',
				INTERVAL '15 minutes'
			)
		)
		SELECT
			bucket.starts_at,
			COALESCE(SUM(cri.capacity_gb), 0)::int AS reserved_gb
		FROM bucket
		LEFT JOIN capacity_reservation_intervals cri
			ON cri.org_id = $1
			AND cri.resource = $4
			AND cri.starts_at = bucket.starts_at
		GROUP BY bucket.starts_at
		ORDER BY bucket.starts_at
	`, orgID, from, to, ResourceMemoryGB)
	if err != nil {
		return nil, fmt.Errorf("calendar query: %w", err)
	}
	defer rows.Close()

	var out []CalendarBucket
	for rows.Next() {
		var b CalendarBucket
		if err := rows.Scan(&b.StartsAt, &b.ReservedGB); err != nil {
			return nil, fmt.Errorf("calendar scan: %w", err)
		}
		b.EndsAt = b.StartsAt.Add(15 * time.Minute)
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("calendar rows: %w", err)
	}
	return out, nil
}

// CreateReservationRequest is the validated, normalised input to the write
// path. Validation (alignment, lead time, % 4 grain, non-empty, no
// duplicate (resource, startsAt)) lives in the handler — the Store assumes
// the input is well-formed.
type CreateReservationRequest struct {
	OrgID            uuid.UUID
	IdempotencyKey   string // empty when no header was sent
	IdempotencyEndpt string // e.g. "POST /api/capacity/reservations"
	RequestBodyHash  []byte
	Intervals        []CapacityInterval
}

// CreateReservation writes the ledger rows for one reservation, atomically.
// Per-interval contention is serialised by org-scoped advisory locks
// acquired in deterministic (resource, startsAt) order to avoid ABBA.
//
// Returns the new reservationId and createdAt on success. On contention or
// over-cap, returns *CapacityShortfallError (the handler emits 409
// capacity_not_available). On idempotency replay, returns *IdempotencyReplay
// with the cached body. On idempotency-body mismatch, returns
// *IdempotencyConflictError.
func (s *Store) CreateReservation(ctx context.Context, req CreateReservationRequest) (uuid.UUID, time.Time, error) {
	if req.IdempotencyKey != "" {
		var (
			cachedHash []byte
			cachedCode int
			cachedBody json.RawMessage
		)
		err := s.pool.QueryRow(ctx, `
			SELECT request_body_hash, status_code, response_body
			FROM capacity_idempotency_keys
			WHERE org_id = $1 AND endpoint = $2 AND key = $3 AND expires_at > now()
		`, req.OrgID, req.IdempotencyEndpt, req.IdempotencyKey).Scan(&cachedHash, &cachedCode, &cachedBody)
		if err == nil {
			if !bytesEq(cachedHash, req.RequestBodyHash) {
				return uuid.Nil, time.Time{}, &IdempotencyConflictError{}
			}
			return uuid.Nil, time.Time{}, &IdempotencyReplay{StatusCode: cachedCode, ResponseBody: cachedBody}
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, time.Time{}, fmt.Errorf("idempotency lookup: %w", err)
		}
	}

	maxMemoryGB, err := s.getOrgMaxMemoryGB(ctx, req.OrgID)
	if err != nil {
		return uuid.Nil, time.Time{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, time.Time{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// Deterministic lock order: (resource, startsAt). Caller guarantees no
	// duplicate (resource, startsAt) pairs, so order is total.
	for _, iv := range req.Intervals {
		key := advisoryLockKey(req.OrgID, ResourceMemoryGB, iv.StartsAt)
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, key); err != nil {
			return uuid.Nil, time.Time{}, fmt.Errorf("advisory lock: %w", err)
		}
	}

	// Re-read existing reserved sums under lock — anything that wasn't held
	// by us at lookup time can't change for the rest of this tx.
	shortfalls := make([]IntervalCapacityShortfall, 0)
	for _, iv := range req.Intervals {
		var existingGB int
		err := tx.QueryRow(ctx, `
			SELECT COALESCE(SUM(capacity_gb), 0)::int
			FROM capacity_reservation_intervals
			WHERE org_id = $1 AND resource = $2 AND starts_at = $3
		`, req.OrgID, ResourceMemoryGB, iv.StartsAt).Scan(&existingGB)
		if err != nil {
			return uuid.Nil, time.Time{}, fmt.Errorf("read existing reserved: %w", err)
		}
		reservable := maxMemoryGB - existingGB
		if reservable < 0 {
			reservable = 0
		}
		if iv.CapacityGB > reservable {
			shortfalls = append(shortfalls, IntervalCapacityShortfall{
				StartsAt:     iv.StartsAt,
				RequestedGB:  iv.CapacityGB,
				ReservableGB: reservable,
				Reason:       "insufficient_capacity",
			})
		}
	}
	if len(shortfalls) > 0 {
		return uuid.Nil, time.Time{}, &CapacityShortfallError{Intervals: shortfalls}
	}

	reservationID := uuid.New()
	createdAt := time.Now().UTC()

	var idemRowID *uuid.UUID
	if req.IdempotencyKey != "" {
		// Reserve the idempotency row first so the FK pointer on the ledger
		// rows can reference it. Insert is deferred until we have a body to
		// cache, but we need its id now for the FK column.
		newID := uuid.New()
		idemRowID = &newID
	}

	for _, iv := range req.Intervals {
		_, err := tx.Exec(ctx, `
			INSERT INTO capacity_reservation_intervals
				(reservation_id, org_id, resource, starts_at, ends_at, capacity_gb, created_at, idempotency_key_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		`, reservationID, req.OrgID, ResourceMemoryGB, iv.StartsAt, iv.EndsAt, iv.CapacityGB, createdAt, idemRowID)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				// Defence in depth — the advisory lock + read-then-write should
				// preclude PK collisions, but if one slips through, surface as
				// shortfall rather than 500.
				return uuid.Nil, time.Time{}, &CapacityShortfallError{
					Intervals: []IntervalCapacityShortfall{{
						StartsAt:    iv.StartsAt,
						RequestedGB: iv.CapacityGB,
						Reason:      "concurrent_write",
					}},
				}
			}
			return uuid.Nil, time.Time{}, fmt.Errorf("insert interval: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, time.Time{}, fmt.Errorf("commit: %w", err)
	}
	return reservationID, createdAt, nil
}

// SaveIdempotencyResult writes the cached response for an Idempotency-Key
// after the handler has built the body. Called once per write request that
// carried an Idempotency-Key, regardless of outcome (200, 409, etc.).
func (s *Store) SaveIdempotencyResult(ctx context.Context, orgID uuid.UUID, endpoint, key string, bodyHash []byte, statusCode int, responseBody json.RawMessage) error {
	if key == "" {
		return nil
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO capacity_idempotency_keys
			(org_id, endpoint, key, request_body_hash, status_code, response_body, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, now() + INTERVAL '24 hours')
		ON CONFLICT (org_id, endpoint, key) DO NOTHING
	`, orgID, endpoint, key, bodyHash, statusCode, responseBody)
	if err != nil {
		return fmt.Errorf("save idempotency: %w", err)
	}
	return nil
}

// ListReservationsRequest carries the filters for the audit-list endpoint.
type ListReservationsRequest struct {
	OrgID  uuid.UUID
	From   time.Time
	To     time.Time
	Limit  int
	Cursor *ReservationCursor // nil on the first page
}

// ReservationCursor is the opaque pagination cursor decoded by the handler.
// Encoding lives in the handler so the storage layer doesn't need to know
// about base64.
type ReservationCursor struct {
	CreatedAt     time.Time
	ReservationID uuid.UUID
}

// ListReservations returns reservations in (createdAt DESC, reservationId
// DESC) order. The result is one row per reservation_id with its intervals
// aggregated. NextCursor is non-nil when more rows exist past the limit.
func (s *Store) ListReservations(ctx context.Context, req ListReservationsRequest) ([]Reservation, *ReservationCursor, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = 50
	}

	args := []any{req.OrgID, req.From, req.To, ResourceMemoryGB, limit + 1}
	cursorClause := ""
	if req.Cursor != nil {
		cursorClause = `AND (created_at, reservation_id) < ($6, $7)`
		args = append(args, req.Cursor.CreatedAt, req.Cursor.ReservationID)
	}

	query := fmt.Sprintf(`
		SELECT
			reservation_id,
			MIN(created_at) AS created_at,
			jsonb_agg(
				jsonb_build_object(
					'startsAt',   starts_at,
					'endsAt',     ends_at,
					'capacityGb', capacity_gb
				) ORDER BY starts_at
			) AS intervals
		FROM capacity_reservation_intervals
		WHERE org_id = $1
			AND resource = $4
			AND created_at >= $2
			AND created_at < $3
			%s
		GROUP BY reservation_id
		ORDER BY MIN(created_at) DESC, reservation_id DESC
		LIMIT $5
	`, cursorClause)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("list reservations: %w", err)
	}
	defer rows.Close()

	out := make([]Reservation, 0, limit)
	for rows.Next() {
		var r Reservation
		var intervalsJSON []byte
		if err := rows.Scan(&r.ReservationID, &r.CreatedAt, &intervalsJSON); err != nil {
			return nil, nil, fmt.Errorf("scan reservation: %w", err)
		}
		if err := json.Unmarshal(intervalsJSON, &r.Intervals); err != nil {
			return nil, nil, fmt.Errorf("decode intervals: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("list rows: %w", err)
	}

	var next *ReservationCursor
	if len(out) > limit {
		last := out[limit-1]
		next = &ReservationCursor{CreatedAt: last.CreatedAt, ReservationID: last.ReservationID}
		out = out[:limit]
	}
	return out, next, nil
}

func (s *Store) getOrgMaxMemoryGB(ctx context.Context, orgID uuid.UUID) (int, error) {
	var v int
	err := s.pool.QueryRow(ctx, `SELECT max_memory_gb FROM orgs WHERE id = $1`, orgID).Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("read max_memory_gb: %w", err)
	}
	return v, nil
}

// advisoryLockKey hashes (org, resource, startsAt) into the int64 keyspace
// used by pg_advisory_xact_lock. fnv64 is collision-prone at scale, but a
// collision here just means two unrelated tuples wait on each other for one
// transaction — a perf nit, not a correctness bug.
func advisoryLockKey(orgID uuid.UUID, resource string, startsAt time.Time) int64 {
	h := fnv.New64a()
	h.Write(orgID[:])
	h.Write([]byte{0})
	h.Write([]byte(resource))
	h.Write([]byte{0})
	var ts [8]byte
	t := startsAt.UTC().Unix()
	for i := 0; i < 8; i++ {
		ts[i] = byte(t >> (i * 8))
	}
	h.Write(ts[:])
	return int64(h.Sum64())
}

func bytesEq(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
