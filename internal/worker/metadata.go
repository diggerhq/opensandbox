package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/opensandbox/opensandbox/internal/sandbox"
	"github.com/opensandbox/opensandbox/pkg/types"
)

// MetadataServer serves the instance metadata API at 169.254.169.254:80 (via DNAT to :8888).
// Sandboxes can access it to discover their own identity, resource limits, and scale up/down.
type MetadataServer struct {
	manager sandbox.Manager
	region  string
	onScale func(sandboxID string, memoryMB, cpuPercent int) // billing callback

	mu         sync.RWMutex
	byGuestIP  map[string]sandboxEntry // guestIP → entry
	bySandbox  map[string]string       // sandboxID → guestIP (for reverse lookup on unregister)

	server *http.Server
}

type sandboxEntry struct {
	SandboxID string
	Template  string
	StartedAt time.Time
}

// NewMetadataServer creates a new metadata server.
func NewMetadataServer(manager sandbox.Manager, region string) *MetadataServer {
	return &MetadataServer{
		manager:   manager,
		region:    region,
		byGuestIP: make(map[string]sandboxEntry),
		bySandbox: make(map[string]string),
	}
}

// SetOnScale registers a callback invoked after a successful in-VM scale operation.
func (ms *MetadataServer) SetOnScale(fn func(sandboxID string, memoryMB, cpuPercent int)) {
	ms.onScale = fn
}

// RegisterSandbox maps a guest IP to a sandbox ID so the metadata handler can identify callers.
func (ms *MetadataServer) RegisterSandbox(sandboxID, guestIP, template string, startedAt time.Time) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	ms.byGuestIP[guestIP] = sandboxEntry{
		SandboxID: sandboxID,
		Template:  template,
		StartedAt: startedAt,
	}
	ms.bySandbox[sandboxID] = guestIP
}

// UnregisterSandbox removes a sandbox from the metadata lookup maps.
func (ms *MetadataServer) UnregisterSandbox(sandboxID string) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if guestIP, ok := ms.bySandbox[sandboxID]; ok {
		delete(ms.byGuestIP, guestIP)
		delete(ms.bySandbox, sandboxID)
	}
}

// Start starts the HTTP metadata server on the given address (e.g. ":8888").
func (ms *MetadataServer) Start(addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/status", ms.handleStatus)
	mux.HandleFunc("/v1/limits", ms.handleLimits)
	mux.HandleFunc("/v1/scale", ms.handleScale)
	mux.HandleFunc("/v1/metadata", ms.handleMetadata)
	// Root returns a list of available endpoints
	mux.HandleFunc("/", ms.handleIndex)

	ms.server = &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	go func() {
		log.Printf("metadata: starting metadata server on %s", addr)
		if err := ms.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("metadata: server error: %v", err)
		}
	}()
}

// Close shuts down the metadata server.
func (ms *MetadataServer) Close() {
	if ms.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ms.server.Shutdown(ctx)
	}
}

// lookupSandbox resolves the calling sandbox from the HTTP request's source IP.
func (ms *MetadataServer) lookupSandbox(r *http.Request) (sandboxEntry, bool) {
	// Extract the IP from RemoteAddr (format is "ip:port")
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// Try using RemoteAddr directly (no port)
		host = r.RemoteAddr
	}

	ms.mu.RLock()
	entry, ok := ms.byGuestIP[host]
	ms.mu.RUnlock()
	return entry, ok
}

func (ms *MetadataServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"endpoints": []string{
			"/v1/status",
			"/v1/limits",
			"/v1/scale",
			"/v1/metadata",
		},
	})
}

// GET /v1/status → {"sandboxId", "uptime"}
func (ms *MetadataServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	entry, ok := ms.lookupSandbox(r)
	if !ok {
		http.Error(w, "sandbox not found for source IP", http.StatusNotFound)
		return
	}

	uptime := time.Since(entry.StartedAt).Seconds()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"sandboxId": entry.SandboxID,
		"uptime":    uptime,
	})
}

// GET /v1/limits → returns current cgroup limits via agent Stats
func (ms *MetadataServer) handleLimits(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	entry, ok := ms.lookupSandbox(r)
	if !ok {
		http.Error(w, "sandbox not found for source IP", http.StatusNotFound)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	stats, err := ms.manager.Stats(ctx, entry.SandboxID)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to get stats: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"cpuPercent": stats.CPUPercent,
		"memUsage":   stats.MemUsage,
		"memLimit":   stats.MemLimit,
		"pids":       stats.PIDs,
		"netInput":   stats.NetInput,
		"netOutput":  stats.NetOutput,
	})
}

// POST /v1/scale → accepts {"memoryMB", "cpuPercent", "pids"}, applies cgroup limits.
// Pricing model: 1 vCPU per 4GB RAM. memoryMB=4096 → cpuPercent=100 (1 vCPU).
func (ms *MetadataServer) handleScale(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	entry, ok := ms.lookupSandbox(r)
	if !ok {
		http.Error(w, "sandbox not found for source IP", http.StatusNotFound)
		return
	}

	var req struct {
		MemoryMB   int `json:"memoryMB"`
		CPUPercent int `json:"cpuPercent"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
		return
	}

	// Validate memory against allowed tiers and auto-calculate CPU
	if req.MemoryMB > 0 {
		vcpus, err := types.ValidateMemoryMB(req.MemoryMB)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.CPUPercent == 0 {
			req.CPUPercent = vcpus * 100
		}
	}

	// Convert to cgroup values
	var maxMemoryBytes, cpuMaxUsec, cpuPeriodUsec int64
	if req.MemoryMB > 0 {
		maxMemoryBytes = int64(req.MemoryMB) * 1024 * 1024
	}
	if req.CPUPercent > 0 {
		cpuPeriodUsec = 100000
		cpuMaxUsec = int64(req.CPUPercent) * 1000
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	if err := ms.manager.SetResourceLimits(ctx, entry.SandboxID, 0, maxMemoryBytes, cpuMaxUsec, cpuPeriodUsec); err != nil {
		http.Error(w, fmt.Sprintf("failed to set limits: %v", err), http.StatusInternalServerError)
		return
	}

	// Notify billing of scale event
	if ms.onScale != nil && req.MemoryMB > 0 {
		ms.onScale(entry.SandboxID, req.MemoryMB, req.CPUPercent)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":         true,
		"memoryMB":   req.MemoryMB,
		"cpuPercent": req.CPUPercent,
	})
}

// GET /v1/metadata → returns {"region", "template"}
func (ms *MetadataServer) handleMetadata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	entry, ok := ms.lookupSandbox(r)
	if !ok {
		http.Error(w, "sandbox not found for source IP", http.StatusNotFound)
		return
	}

	// Build metadata response
	resp := map[string]interface{}{
		"region":   ms.region,
		"template": entry.Template,
	}

	// Strip any prefix to get clean template name
	if strings.Contains(entry.Template, "/") {
		parts := strings.Split(entry.Template, "/")
		resp["templateBase"] = parts[len(parts)-1]
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
