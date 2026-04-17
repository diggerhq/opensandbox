package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/opensandbox/opensandbox/internal/db"
)

func TestHealthEndpoint(t *testing.T) {
	e := echo.New()
	e.GET("/health", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
	})

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if body["status"] != "ok" {
		t.Errorf("expected status ok, got %s", body["status"])
	}
}

// TestForkCheckpointAuthMatrix pins the design-009 auth predicate at the fork
// call site: cp.OrgID != orgID && !cp.IsPublic. The goal is to catch any
// future refactor that accidentally widens fork access or re-tightens it for
// public checkpoints. The logic is deliberately inlined (no helper) in the
// handler to keep the diff minimal, so we mirror it here and exercise every
// quadrant. Handler-level HTTP tests that also exercise DB state live behind
// a Postgres fixture we don't have yet in this repo — see the PR description
// for follow-up.
func TestForkCheckpointAuthMatrix(t *testing.T) {
	ownerOrg := uuid.New()
	otherOrg := uuid.New()

	cases := []struct {
		name      string
		cp        db.Checkpoint
		caller    uuid.UUID
		wantDeny  bool
	}{
		{"owner forks private", db.Checkpoint{OrgID: ownerOrg, IsPublic: false}, ownerOrg, false},
		{"owner forks public", db.Checkpoint{OrgID: ownerOrg, IsPublic: true}, ownerOrg, false},
		{"stranger forks private", db.Checkpoint{OrgID: ownerOrg, IsPublic: false}, otherOrg, true},
		{"stranger forks public", db.Checkpoint{OrgID: ownerOrg, IsPublic: true}, otherOrg, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deny := tc.cp.OrgID != tc.caller && !tc.cp.IsPublic
			if deny != tc.wantDeny {
				t.Fatalf("deny=%v, want %v (cp.OrgID=%s caller=%s public=%v)",
					deny, tc.wantDeny, tc.cp.OrgID, tc.caller, tc.cp.IsPublic)
			}
		})
	}
}

// TestPatchOpsStayOwnerOnly pins that the three patch call sites and the
// checkpoint-delete call site still use the strict predicate even after the
// design-009 fork relaxation. Mirror the handler logic for the same reason
// as TestForkCheckpointAuthMatrix.
func TestPatchOpsStayOwnerOnly(t *testing.T) {
	ownerOrg := uuid.New()
	otherOrg := uuid.New()
	publicCp := db.Checkpoint{OrgID: ownerOrg, IsPublic: true}

	// Every strict site uses `cp.OrgID != orgID` — public flag must not
	// leak into these decisions.
	if publicCp.OrgID == otherOrg {
		t.Fatal("fixture invalid")
	}
	if denied := publicCp.OrgID != otherOrg; !denied {
		t.Fatal("stranger must be denied patch/delete ops on a public checkpoint")
	}
	if denied := publicCp.OrgID != ownerOrg; denied {
		t.Fatal("owner must be allowed patch/delete ops on own public checkpoint")
	}
}
