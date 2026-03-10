package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

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
			Buckets: []float64{0.1, 0.25, 0.5, 1.0, 2.0, 5.0, 10.0},
		},
		[]string{"region", "template"},
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
	// Register all metrics
	prometheus.MustRegister(
		SandboxesActive,
		SandboxCreateDuration,
		ExecDuration,
		PTYSessionsActive,
		WorkerUtilization,
		DirectConnectionsActive,
		SQLiteSyncLag,
		HTTPRequestsTotal,
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
func EchoMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			start := time.Now()
			err := next(c)
			duration := time.Since(start)

			status := c.Response().Status
			if err != nil {
				if he, ok := err.(*echo.HTTPError); ok {
					status = he.Code
				}
			}

			HTTPRequestsTotal.WithLabelValues(
				c.Request().Method,
				c.Path(),
				strconv.Itoa(status),
			).Inc()

			_ = duration // Could add request duration histogram here
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
