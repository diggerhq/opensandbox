package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// lifecycleBuckets covers from sub-second warm forks to slow checkpoint uploads.
var lifecycleBuckets = []float64{0.1, 0.25, 0.5, 1.0, 2.0, 5.0, 10.0, 30.0, 60.0}

// Worker metrics
var (
	SandboxesActive = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "opensandbox_sandboxes_active",
			Help: "Number of currently active sandboxes",
		},
		[]string{"region", "worker_id", "template"},
	)

	SandboxCreateDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "opensandbox_sandbox_create_duration_seconds",
			Help:    "Time to create a sandbox",
			Buckets: lifecycleBuckets,
		},
		[]string{"region", "template", "status"},
	)

	CheckpointDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "opensandbox_checkpoint_duration_seconds",
			Help:    "Time to create a sandbox checkpoint (savevm + archive + upload)",
			Buckets: lifecycleBuckets,
		},
		[]string{"region", "template", "status"},
	)

	// Exists alongside CheckpointDuration{status="failure"} so failures can be
	// attributed by *cause* without log parsing.
	CheckpointFailuresTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "opensandbox_checkpoint_failures_total",
			Help: "Checkpoint failures by classified reason",
		},
		[]string{"region", "template", "reason"},
	)

	HibernateDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "opensandbox_hibernate_duration_seconds",
			Help:    "Time to hibernate a sandbox (quiesce + savevm + upload)",
			Buckets: lifecycleBuckets,
		},
		[]string{"region", "template", "status"},
	)

	// source=warm_cache: local snapshot reused. source=s3: downloaded archive.
	WakeDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "opensandbox_wake_duration_seconds",
			Help:    "Time to create a sandbox from a checkpoint",
			Buckets: lifecycleBuckets,
		},
		[]string{"region", "template", "source", "status"},
	)

	ExecDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "opensandbox_exec_duration_seconds",
			Help:    "Time to execute a command in a sandbox",
			Buckets: []float64{0.01, 0.05, 0.1, 0.5, 1.0, 5.0, 30.0, 60.0},
		},
		[]string{"region"},
	)

	PTYSessionsActive = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "opensandbox_pty_sessions_active",
			Help: "Number of active PTY sessions",
		},
		[]string{"region", "worker_id"},
	)

	WorkerUtilization = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "opensandbox_worker_utilization",
			Help: "Worker utilization (0-1)",
		},
		[]string{"region", "worker_id"},
	)

	// Worker resource gauges. Populated by a periodic tick in cmd/worker/main.go.
	WorkerDiskUsedBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "opensandbox_worker_disk_used_bytes", Help: "Disk bytes used on the worker's data mount"},
		[]string{"region", "worker_id", "mount"},
	)
	WorkerDiskAvailableBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "opensandbox_worker_disk_available_bytes", Help: "Disk bytes available on the worker's data mount"},
		[]string{"region", "worker_id", "mount"},
	)
	WorkerDiskTotalBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "opensandbox_worker_disk_total_bytes", Help: "Total disk bytes on the worker's data mount"},
		[]string{"region", "worker_id", "mount"},
	)

	WorkerMemoryTotalBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "opensandbox_worker_memory_total_bytes", Help: "Total physical memory (MemTotal)"},
		[]string{"region", "worker_id"},
	)
	WorkerMemoryAvailableBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "opensandbox_worker_memory_available_bytes", Help: "Memory available to userspace (MemAvailable)"},
		[]string{"region", "worker_id"},
	)
	// Sum of MemoryMB committed to running sandboxes. Measures oversubscription
	// independent of actual guest workload.
	WorkerMemoryAllocatedBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "opensandbox_worker_memory_allocated_bytes", Help: "Sum of memory allocated to running sandboxes"},
		[]string{"region", "worker_id"},
	)

	// CPU pressure percent. window: avg10/avg60/avg300.
	// source=psi: PSI "some" stall pct from /proc/pressure/cpu (preferred).
	// source=loadavg: loadavg/nproc*100 fallback (older kernels / macOS dev hosts);
	// different semantics — interpret with care.
	WorkerCPUPressure = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "opensandbox_worker_cpu_pressure", Help: "CPU pressure percent (PSI 'some' or loadavg/nproc*100 fallback)"},
		[]string{"region", "worker_id", "window", "source"},
	)

	DirectConnectionsActive = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "opensandbox_direct_connections_active",
			Help: "Number of active direct SDK connections to worker",
		},
		[]string{"region", "worker_id"},
	)

	SQLiteSyncLag = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "opensandbox_sqlite_sync_lag_seconds",
			Help: "Time since last NATS sync",
		},
		[]string{"region", "worker_id"},
	)
)

// Control plane metrics
var (
	HTTPRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "opensandbox_http_requests_total",
			Help: "Total HTTP requests",
		},
		[]string{"method", "path", "status"},
	)

	HTTPRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "opensandbox_http_request_duration_seconds",
			Help:    "HTTP request handler latency",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
		},
		[]string{"method", "path", "status"},
	)

	HTTPRequestsInFlight = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "opensandbox_http_requests_in_flight",
			Help: "Currently in-flight HTTP requests",
		},
		[]string{"method"},
	)

	SandboxCreatesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "opensandbox_sandbox_creates_total",
			Help: "Total sandbox creations",
		},
		[]string{"region", "template", "status"},
	)

	AuthAttemptsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "opensandbox_auth_attempts_total",
			Help: "Total auth attempts",
		},
		[]string{"type", "result"},
	)

	WorkersTotal = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "opensandbox_workers_total",
			Help: "Number of workers",
		},
		[]string{"region", "status"},
	)

	ScaleEventsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "opensandbox_scale_events_total",
			Help: "Total scaling events",
		},
		[]string{"region", "direction"},
	)
)

func init() {
	prometheus.MustRegister(
		SandboxesActive,
		SandboxCreateDuration,
		CheckpointDuration,
		CheckpointFailuresTotal,
		HibernateDuration,
		WakeDuration,
		ExecDuration,
		PTYSessionsActive,
		WorkerUtilization,
		WorkerDiskUsedBytes,
		WorkerDiskAvailableBytes,
		WorkerDiskTotalBytes,
		WorkerMemoryTotalBytes,
		WorkerMemoryAvailableBytes,
		WorkerMemoryAllocatedBytes,
		WorkerCPUPressure,
		DirectConnectionsActive,
		SQLiteSyncLag,
		HTTPRequestsTotal,
		HTTPRequestDuration,
		HTTPRequestsInFlight,
		SandboxCreatesTotal,
		AuthAttemptsTotal,
		WorkersTotal,
		ScaleEventsTotal,
	)
}

// Handler returns an HTTP handler for the /metrics endpoint.
func Handler() http.Handler {
	return promhttp.Handler()
}

// EchoMiddleware returns Echo middleware that instruments HTTP requests.
//
// path label uses c.Path() (the route template, e.g. /api/sandboxes/:id) rather
// than c.Request().URL.Path so high-cardinality IDs don't explode the metric.
func EchoMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			method := c.Request().Method
			HTTPRequestsInFlight.WithLabelValues(method).Inc()
			defer HTTPRequestsInFlight.WithLabelValues(method).Dec()

			start := time.Now()
			err := next(c)
			duration := time.Since(start)

			status := c.Response().Status
			if err != nil {
				if he, ok := err.(*echo.HTTPError); ok {
					status = he.Code
				}
			}

			statusStr := strconv.Itoa(status)
			path := c.Path()
			HTTPRequestsTotal.WithLabelValues(method, path, statusStr).Inc()
			HTTPRequestDuration.WithLabelValues(method, path, statusStr).Observe(duration.Seconds())

			return err
		}
	}
}

// StartMetricsServer starts a standalone HTTP server serving /metrics on the given address.
func StartMetricsServer(addr string) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", Handler())
	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			// Log but don't crash — metrics are non-critical
		}
	}()
	return srv
}
