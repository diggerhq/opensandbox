package api_test

import (
	"net/http"
	"testing"
)

// TestAuth_RejectsBadKeys covers the three negative auth paths against an
// authenticated endpoint (/api/workers, which requires a valid API key in PG).
func TestAuth_RejectsBadKeys(t *testing.T) {
	c := newClient(t)
	cases := []struct {
		name    string
		headers map[string]string
		wantMin int // accept >= this
		wantMax int // accept <= this (so 401 or 403 both pass)
	}{
		{"no key", map[string]string{}, 401, 403},
		{"empty key", map[string]string{"X-API-Key": ""}, 401, 403},
		{"wrong key", map[string]string{"X-API-Key": "osb_ci_definitely_not_a_real_key"}, 401, 403},
		{"bearer of api key", map[string]string{"Authorization": "Bearer " + c.apiKey}, 401, 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := c.raw(t, http.MethodGet, "/api/workers", tc.headers)
			if code < tc.wantMin || code > tc.wantMax {
				t.Fatalf("want %d-%d, got %d (body=%q)", tc.wantMin, tc.wantMax, code, body)
			}
		})
	}
}

func TestAuth_AcceptsValidKey(t *testing.T) {
	c := newClient(t)
	code, body := c.raw(t, http.MethodGet, "/api/workers", map[string]string{
		"X-API-Key": c.apiKey,
	})
	if code != http.StatusOK {
		t.Fatalf("/api/workers with valid key: want 200, got %d (body=%q)", code, body)
	}
}
