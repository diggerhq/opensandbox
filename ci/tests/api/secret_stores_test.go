package api_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

// TestSecretStores_CRUD covers the full lifecycle of a secret store and the
// secret entries inside it. The store is created with a unique name per test
// run so concurrent runs don't collide.
func TestSecretStores_CRUD(t *testing.T) {
	c := newClient(t)
	storeName := fmt.Sprintf("ci-test-store-%d", time.Now().UnixNano())

	// Create store
	var created struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	code, err := c.do(t, http.MethodPost, "/api/secret-stores",
		map[string]any{"name": storeName}, &created)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if code/100 != 2 {
		t.Fatalf("create: want 2xx, got %d", code)
	}
	if created.ID == "" || created.Name != storeName {
		t.Fatalf("create response: %+v", created)
	}
	storeID := created.ID
	t.Cleanup(func() { cleanupStore(t, c, storeID) })

	t.Run("list includes new store", func(t *testing.T) {
		var stores []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		code, err := c.do(t, http.MethodGet, "/api/secret-stores", nil, &stores)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if code != http.StatusOK {
			t.Fatalf("list: want 200, got %d", code)
		}
		var found bool
		for _, s := range stores {
			if s.ID == storeID {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("created store %s not in list (got %d stores)", storeID, len(stores))
		}
	})

	t.Run("get", func(t *testing.T) {
		var s struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		code, err := c.do(t, http.MethodGet, "/api/secret-stores/"+storeID, nil, &s)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if code != http.StatusOK || s.ID != storeID {
			t.Fatalf("get: code=%d resp=%+v", code, s)
		}
	})

	t.Run("set + list + delete entry", func(t *testing.T) {
		// Set
		code, err := c.do(t, http.MethodPut,
			"/api/secret-stores/"+storeID+"/secrets/MY_KEY",
			map[string]any{"value": "super-secret-value"}, nil)
		if err != nil {
			t.Fatalf("set entry: %v", err)
		}
		if code/100 != 2 {
			t.Fatalf("set entry: want 2xx, got %d", code)
		}

		// List (try both shapes)
		var entries []struct {
			Name string `json:"name"`
		}
		code, err = c.do(t, http.MethodGet,
			"/api/secret-stores/"+storeID+"/secrets", nil, &entries)
		if err != nil {
			var wrapped struct {
				Entries []struct {
					Name string `json:"name"`
				} `json:"entries"`
			}
			code2, err2 := c.do(t, http.MethodGet,
				"/api/secret-stores/"+storeID+"/secrets", nil, &wrapped)
			if err2 != nil {
				t.Fatalf("list entries: %v / %v", err, err2)
			}
			code = code2
			entries = wrapped.Entries
		}
		if code != http.StatusOK {
			t.Fatalf("list entries: want 200, got %d", code)
		}
		var found bool
		for _, e := range entries {
			if e.Name == "MY_KEY" {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("MY_KEY not in entries: %+v", entries)
		}

		// Delete
		code, err = c.do(t, http.MethodDelete,
			"/api/secret-stores/"+storeID+"/secrets/MY_KEY", nil, nil)
		if err != nil {
			t.Fatalf("delete entry: %v", err)
		}
		if code/100 != 2 {
			t.Fatalf("delete entry: want 2xx, got %d", code)
		}
	})
}

func cleanupStore(t *testing.T, c *client, storeID string) {
	if storeID == "" {
		return
	}
	code, _ := c.do(t, http.MethodDelete, "/api/secret-stores/"+storeID, nil, nil)
	if code/100 != 2 && code != http.StatusNotFound {
		t.Logf("cleanup: failed to delete store %s (code=%d)", storeID, code)
	}
}
