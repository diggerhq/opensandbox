package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opensandbox/opensandbox/internal/db"
)

func validRequestBody(t *testing.T) []byte {
	t.Helper()
	start := alignUp15(time.Now().UTC().Add(2 * time.Hour))
	body := reservationRequestBody{
		Intervals: []reservationIntervalBody{{
			StartsAt:   start.Format(time.RFC3339),
			EndsAt:     start.Add(15 * time.Minute).Format(time.RFC3339),
			CapacityGB: 8,
		}},
	}
	out, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return out
}

func TestParseAndValidateReservationBody_happy(t *testing.T) {
	out, err := parseAndValidateReservationBody(validRequestBody(t))
	if err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 interval, got %d", len(out))
	}
	if out[0].CapacityGB != 8 {
		t.Errorf("capacity_gb = %d, want 8", out[0].CapacityGB)
	}
}

func TestParseAndValidateReservationBody_rejections(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantSub string
	}{
		{
			name:    "empty body",
			body:    ``,
			wantSub: "request body is required",
		},
		{
			name:    "intervals empty",
			body:    `{"intervals": []}`,
			wantSub: "must be non-empty",
		},
		{
			name: "non-aligned startsAt",
			body: func() string {
				start := alignUp15(time.Now().UTC().Add(2 * time.Hour)).Add(7 * time.Minute)
				return `{"intervals":[{"startsAt":"` + start.Format(time.RFC3339) + `","endsAt":"` + start.Add(15*time.Minute).Format(time.RFC3339) + `","capacityGb":8}]}`
			}(),
			wantSub: "aligned to 15 minutes",
		},
		{
			name: "endsAt mismatch",
			body: func() string {
				start := alignUp15(time.Now().UTC().Add(2 * time.Hour))
				return `{"intervals":[{"startsAt":"` + start.Format(time.RFC3339) + `","endsAt":"` + start.Add(30*time.Minute).Format(time.RFC3339) + `","capacityGb":8}]}`
			}(),
			wantSub: "endsAt must equal startsAt + 15 minutes",
		},
		{
			name: "capacity not multiple of 4",
			body: func() string {
				start := alignUp15(time.Now().UTC().Add(2 * time.Hour))
				return `{"intervals":[{"startsAt":"` + start.Format(time.RFC3339) + `","endsAt":"` + start.Add(15*time.Minute).Format(time.RFC3339) + `","capacityGb":3}]}`
			}(),
			wantSub: "multiple of 4",
		},
		{
			name: "capacity zero",
			body: func() string {
				start := alignUp15(time.Now().UTC().Add(2 * time.Hour))
				return `{"intervals":[{"startsAt":"` + start.Format(time.RFC3339) + `","endsAt":"` + start.Add(15*time.Minute).Format(time.RFC3339) + `","capacityGb":0}]}`
			}(),
			wantSub: "multiple of 4",
		},
		{
			name: "lead time violation",
			body: func() string {
				start := alignUp15(time.Now().UTC().Add(5 * time.Minute))
				return `{"intervals":[{"startsAt":"` + start.Format(time.RFC3339) + `","endsAt":"` + start.Add(15*time.Minute).Format(time.RFC3339) + `","capacityGb":8}]}`
			}(),
			wantSub: "lead-time window",
		},
		{
			name: "duplicate startsAt",
			body: func() string {
				start := alignUp15(time.Now().UTC().Add(2 * time.Hour))
				ends := start.Add(15 * time.Minute)
				one := `{"startsAt":"` + start.Format(time.RFC3339) + `","endsAt":"` + ends.Format(time.RFC3339) + `","capacityGb":4}`
				return `{"intervals":[` + one + `,` + one + `]}`
			}(),
			wantSub: "duplicates an earlier entry",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseAndValidateReservationBody([]byte(tc.body))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestCursorRoundTrip(t *testing.T) {
	in := &db.ReservationCursor{
		CreatedAt:     time.Date(2026, 4, 28, 18, 0, 5, 0, time.UTC),
		ReservationID: uuid.MustParse("9f67b8f7-7b91-4d2d-b1cb-19d0d0a14562"),
	}
	encoded := encodeCursor(in)
	if encoded == "" {
		t.Fatal("encoded cursor is empty")
	}
	if strings.ContainsAny(encoded, "+/=") {
		t.Errorf("cursor %q must be url-safe (no +, /, =)", encoded)
	}

	out, err := decodeCursor(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.CreatedAt.Equal(in.CreatedAt) {
		t.Errorf("createdAt: got %v, want %v", out.CreatedAt, in.CreatedAt)
	}
	if out.ReservationID != in.ReservationID {
		t.Errorf("reservationId: got %v, want %v", out.ReservationID, in.ReservationID)
	}
}

func TestDecodeCursor_invalid(t *testing.T) {
	cases := []string{
		"!!!",                  // not base64
		"YWJj",                 // base64 but not JSON
		"e30",                  // {} — missing fields
		"eyJjcmVhdGVkQXQiOjF9", // partial JSON, missing reservationId
	}
	for _, raw := range cases {
		if _, err := decodeCursor(raw); err == nil {
			t.Errorf("expected error for cursor %q", raw)
		}
	}
}

func TestAlignUp15(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"2026-04-28T18:00:00Z", "2026-04-28T18:00:00Z"}, // already aligned
		{"2026-04-28T18:00:01Z", "2026-04-28T18:15:00Z"}, // round up
		{"2026-04-28T18:14:59Z", "2026-04-28T18:15:00Z"},
		{"2026-04-28T18:15:00Z", "2026-04-28T18:15:00Z"},
		{"2026-04-28T18:59:59Z", "2026-04-28T19:00:00Z"},
	}
	for _, tc := range cases {
		in, _ := time.Parse(time.RFC3339, tc.in)
		want, _ := time.Parse(time.RFC3339, tc.want)
		got := alignUp15(in)
		if !got.Equal(want) {
			t.Errorf("alignUp15(%s) = %s, want %s", tc.in, got.Format(time.RFC3339), tc.want)
		}
	}
}

func TestIsAligned15(t *testing.T) {
	if !isAligned15(time.Date(2026, 4, 28, 18, 15, 0, 0, time.UTC)) {
		t.Error(":15 must be aligned")
	}
	if isAligned15(time.Date(2026, 4, 28, 18, 7, 0, 0, time.UTC)) {
		t.Error(":07 must not be aligned")
	}
	if isAligned15(time.Date(2026, 4, 28, 18, 0, 1, 0, time.UTC)) {
		t.Error("non-zero seconds must not be aligned")
	}
}
