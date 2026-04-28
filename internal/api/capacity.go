package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/opensandbox/opensandbox/internal/auth"
	"github.com/opensandbox/opensandbox/internal/db"
)

// Reserved-capacity endpoints. Spec lives in
// ws-pricing/design/001-reserved-capacity-squares.md; implementation plan
// in ws-pricing/work/001-reserved-capacity-impl.md.

const (
	capacityHandlerTimeout = 10 * time.Second

	// Closed phase-1 product decisions (ws-pricing work/001).
	capacityMinLeadTime         = 30 * time.Minute
	capacityMaxCalendarSpan     = 35 * 24 * time.Hour
	capacityMaxListSpan         = 35 * 24 * time.Hour
	capacityMaxIntervalsPerPost = 96 * 7 // one week of 15-minute slots — generous; tighter cap can land later
	capacityListDefaultLimit    = 50
	capacityListMaxLimit        = 200
)

const (
	capacityCreateEndpoint = "POST /api/capacity/reservations"
)

// getCapacityCalendar → GET /api/capacity/calendar
func (s *Server) getCapacityCalendar(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}
	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "org context required"})
	}

	from, to, err := parseAlignedRange(c.QueryParam("from"), c.QueryParam("to"), capacityMaxCalendarSpan)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}

	ctx, cancel := context.WithTimeout(c.Request().Context(), capacityHandlerTimeout)
	defer cancel()

	org, err := s.store.GetOrg(ctx, orgID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	buckets, err := s.store.GetCapacityCalendar(ctx, orgID, from, to)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	items := make([]map[string]any, 0, len(buckets))
	for _, b := range buckets {
		items = append(items, map[string]any{
			"startsAt":           b.StartsAt.UTC().Format(time.RFC3339),
			"endsAt":             b.EndsAt.UTC().Format(time.RFC3339),
			"reservedGb":         b.ReservedGB,
			"reservationLimitGb": org.MaxMemoryGB,
			"reservableGb":       max0(org.MaxMemoryGB - b.ReservedGB),
		})
	}

	return c.JSON(http.StatusOK, map[string]any{
		"from":      from.UTC().Format(time.RFC3339),
		"to":        to.UTC().Format(time.RFC3339),
		"resource":  "memory_gb",
		"intervals": items,
	})
}

// createCapacityReservation → POST /api/capacity/reservations
func (s *Server) createCapacityReservation(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}
	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "org context required"})
	}

	body, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "read body: " + err.Error()})
	}
	bodyHash := sha256.Sum256(body)

	intervals, err := parseAndValidateReservationBody(body)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}

	ctx, cancel := context.WithTimeout(c.Request().Context(), capacityHandlerTimeout)
	defer cancel()

	idemKey := c.Request().Header.Get("Idempotency-Key")

	reservationID, createdAt, err := s.store.CreateReservation(ctx, db.CreateReservationRequest{
		OrgID:            orgID,
		IdempotencyKey:   idemKey,
		IdempotencyEndpt: capacityCreateEndpoint,
		RequestBodyHash:  bodyHash[:],
		Intervals:        intervals,
	})
	if err != nil {
		var replay *db.IdempotencyReplay
		if errors.As(err, &replay) {
			return c.JSONBlob(replay.StatusCode, replay.ResponseBody)
		}
		var conflict *db.IdempotencyConflictError
		if errors.As(err, &conflict) {
			body := map[string]any{
				"error":   "idempotency_key_conflict",
				"message": "Idempotency-Key was previously used with a different request body",
			}
			return c.JSON(http.StatusConflict, body)
		}
		var shortfall *db.CapacityShortfallError
		if errors.As(err, &shortfall) {
			body := buildCapacityShortfallBody(shortfall)
			cacheCapacityResponse(ctx, s.store, orgID, idemKey, bodyHash[:], http.StatusConflict, body)
			return c.JSON(http.StatusConflict, body)
		}
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	body200 := map[string]any{
		"reservationId": reservationID.String(),
		"createdAt":     createdAt.UTC().Format(time.RFC3339),
		"intervals":     intervalsToJSON(intervals),
	}
	cacheCapacityResponse(ctx, s.store, orgID, idemKey, bodyHash[:], http.StatusOK, body200)
	return c.JSON(http.StatusOK, body200)
}

// listCapacityReservations → GET /api/capacity/reservations
func (s *Server) listCapacityReservations(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}
	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "org context required"})
	}

	from, to, err := parseRange(c.QueryParam("from"), c.QueryParam("to"), capacityMaxListSpan)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}

	limit := capacityListDefaultLimit
	if s := c.QueryParam("limit"); s != "" {
		var n int
		if _, err := fmt.Sscanf(s, "%d", &n); err != nil || n <= 0 {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": "`limit` must be a positive integer"})
		}
		if n > capacityListMaxLimit {
			n = capacityListMaxLimit
		}
		limit = n
	}

	var cursor *db.ReservationCursor
	if raw := c.QueryParam("cursor"); raw != "" {
		decoded, err := decodeCursor(raw)
		if err != nil {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid cursor"})
		}
		cursor = decoded
	}

	ctx, cancel := context.WithTimeout(c.Request().Context(), capacityHandlerTimeout)
	defer cancel()

	reservations, next, err := s.store.ListReservations(ctx, db.ListReservationsRequest{
		OrgID:  orgID,
		From:   from,
		To:     to,
		Limit:  limit,
		Cursor: cursor,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	items := make([]map[string]any, 0, len(reservations))
	for _, r := range reservations {
		items = append(items, map[string]any{
			"reservationId": r.ReservationID.String(),
			"createdAt":     r.CreatedAt.UTC().Format(time.RFC3339),
			"intervals":     intervalsToJSON(r.Intervals),
		})
	}

	resp := map[string]any{
		"from":         from.UTC().Format(time.RFC3339),
		"to":           to.UTC().Format(time.RFC3339),
		"reservations": items,
		"nextCursor":   nullableString(encodeCursor(next)),
	}
	return c.JSON(http.StatusOK, resp)
}

// --- request parsing / validation ---

type reservationRequestBody struct {
	Intervals []reservationIntervalBody `json:"intervals"`
}

type reservationIntervalBody struct {
	StartsAt   string `json:"startsAt"`
	EndsAt     string `json:"endsAt"`
	CapacityGB int    `json:"capacityGb"`
}

func parseAndValidateReservationBody(body []byte) ([]db.CapacityInterval, error) {
	var req reservationRequestBody
	if len(body) == 0 {
		return nil, fmt.Errorf("request body is required")
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if len(req.Intervals) == 0 {
		return nil, fmt.Errorf("`intervals` must be non-empty")
	}
	if len(req.Intervals) > capacityMaxIntervalsPerPost {
		return nil, fmt.Errorf("`intervals` cannot exceed %d entries per request", capacityMaxIntervalsPerPost)
	}

	now := time.Now().UTC()
	earliest := alignUp15(now.Add(capacityMinLeadTime))

	out := make([]db.CapacityInterval, 0, len(req.Intervals))
	seen := make(map[time.Time]struct{}, len(req.Intervals))
	for i, iv := range req.Intervals {
		startsAt, err := time.Parse(time.RFC3339, iv.StartsAt)
		if err != nil {
			return nil, fmt.Errorf("intervals[%d].startsAt must be RFC3339: %w", i, err)
		}
		endsAt, err := time.Parse(time.RFC3339, iv.EndsAt)
		if err != nil {
			return nil, fmt.Errorf("intervals[%d].endsAt must be RFC3339: %w", i, err)
		}
		startsAt = startsAt.UTC()
		endsAt = endsAt.UTC()

		if !isAligned15(startsAt) {
			return nil, fmt.Errorf("intervals[%d].startsAt must be aligned to 15 minutes UTC", i)
		}
		if !endsAt.Equal(startsAt.Add(15 * time.Minute)) {
			return nil, fmt.Errorf("intervals[%d].endsAt must equal startsAt + 15 minutes", i)
		}
		if iv.CapacityGB <= 0 || iv.CapacityGB%4 != 0 {
			return nil, fmt.Errorf("intervals[%d].capacityGb must be a positive multiple of 4", i)
		}
		if startsAt.Before(earliest) {
			return nil, fmt.Errorf("intervals[%d].startsAt is inside the %s lead-time window", i, capacityMinLeadTime)
		}
		if _, dup := seen[startsAt]; dup {
			return nil, fmt.Errorf("intervals[%d].startsAt duplicates an earlier entry — combine into a single capacityGb", i)
		}
		seen[startsAt] = struct{}{}

		out = append(out, db.CapacityInterval{
			StartsAt:   startsAt,
			EndsAt:     endsAt,
			CapacityGB: iv.CapacityGB,
		})
	}
	return out, nil
}

func parseAlignedRange(fromStr, toStr string, maxSpan time.Duration) (time.Time, time.Time, error) {
	from, to, err := parseRange(fromStr, toStr, maxSpan)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if !isAligned15(from) {
		return time.Time{}, time.Time{}, fmt.Errorf("`from` must be aligned to 15 minutes UTC")
	}
	if !isAligned15(to) {
		return time.Time{}, time.Time{}, fmt.Errorf("`to` must be aligned to 15 minutes UTC")
	}
	return from, to, nil
}

func parseRange(fromStr, toStr string, maxSpan time.Duration) (time.Time, time.Time, error) {
	if fromStr == "" {
		return time.Time{}, time.Time{}, fmt.Errorf("`from` is required")
	}
	if toStr == "" {
		return time.Time{}, time.Time{}, fmt.Errorf("`to` is required")
	}
	from, err := time.Parse(time.RFC3339, fromStr)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("`from` must be RFC3339: %w", err)
	}
	to, err := time.Parse(time.RFC3339, toStr)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("`to` must be RFC3339: %w", err)
	}
	from = from.UTC()
	to = to.UTC()
	if !to.After(from) {
		return time.Time{}, time.Time{}, fmt.Errorf("`to` must be after `from`")
	}
	if to.Sub(from) > maxSpan {
		return time.Time{}, time.Time{}, fmt.Errorf("query window must be <= %s", maxSpan)
	}
	return from, to, nil
}

func isAligned15(t time.Time) bool {
	return t.Equal(t.Truncate(15 * time.Minute))
}

func alignUp15(t time.Time) time.Time {
	floored := t.Truncate(15 * time.Minute)
	if floored.Equal(t) {
		return floored
	}
	return floored.Add(15 * time.Minute)
}

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// --- response helpers ---

func intervalsToJSON(in []db.CapacityInterval) []map[string]any {
	out := make([]map[string]any, 0, len(in))
	for _, iv := range in {
		out = append(out, map[string]any{
			"startsAt":   iv.StartsAt.UTC().Format(time.RFC3339),
			"endsAt":     iv.EndsAt.UTC().Format(time.RFC3339),
			"capacityGb": iv.CapacityGB,
		})
	}
	return out
}

func buildCapacityShortfallBody(s *db.CapacityShortfallError) map[string]any {
	intervals := make([]map[string]any, 0, len(s.Intervals))
	for _, iv := range s.Intervals {
		intervals = append(intervals, map[string]any{
			"startsAt":     iv.StartsAt.UTC().Format(time.RFC3339),
			"requestedGb":  iv.RequestedGB,
			"reservableGb": iv.ReservableGB,
			"reason":       iv.Reason,
		})
	}
	return map[string]any{
		"error":     "capacity_not_available",
		"intervals": intervals,
	}
}

func cacheCapacityResponse(ctx context.Context, store *db.Store, orgID uuid.UUID, key string, bodyHash []byte, status int, body map[string]any) {
	if key == "" {
		return
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return
	}
	// Save best-effort. A failed save means the next replay will hit the
	// write path again and (idempotency aside) take the same outcome on a
	// matching key. Logged but not surfaced to the caller.
	_ = store.SaveIdempotencyResult(ctx, orgID, capacityCreateEndpoint, key, bodyHash, status, encoded)
}

// --- cursor encoding ---

type capacityCursorPayload struct {
	CreatedAt     time.Time `json:"createdAt"`
	ReservationID uuid.UUID `json:"reservationId"`
}

func encodeCursor(c *db.ReservationCursor) string {
	if c == nil {
		return ""
	}
	payload, err := json.Marshal(capacityCursorPayload{
		CreatedAt:     c.CreatedAt,
		ReservationID: c.ReservationID,
	})
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(payload)
}

func decodeCursor(raw string) (*db.ReservationCursor, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, err
	}
	var p capacityCursorPayload
	if err := json.Unmarshal(decoded, &p); err != nil {
		return nil, err
	}
	if p.ReservationID == uuid.Nil {
		return nil, fmt.Errorf("cursor missing reservationId")
	}
	return &db.ReservationCursor{
		CreatedAt:     p.CreatedAt,
		ReservationID: p.ReservationID,
	}, nil
}
