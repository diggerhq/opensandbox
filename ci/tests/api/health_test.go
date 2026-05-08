package api_test

import (
	"net/http"
	"testing"
)

func TestHealthz(t *testing.T) {
	c := newClient(t)
	var resp struct {
		Status string `json:"status"`
	}
	code, err := c.do(t, http.MethodGet, "/healthz", nil, &resp)
	if err != nil {
		t.Fatalf("/healthz: %v", err)
	}
	if code != http.StatusOK {
		t.Fatalf("/healthz: want 200, got %d", code)
	}
	if resp.Status != "alive" {
		t.Fatalf("/healthz: want status=alive, got %q", resp.Status)
	}
}
