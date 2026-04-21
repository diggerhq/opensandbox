// Package observability wires Sentry error reporting into the control plane
// and worker. Init is a no-op when OPENSANDBOX_SENTRY_DSN is unset, so the
// package is safe to call unconditionally at startup.
package observability

import (
	"log"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/opensandbox/opensandbox/internal/config"
)

// Init configures the global Sentry hub. Returns a flush function that the
// caller should defer so buffered events get delivered on shutdown. When the
// DSN is empty, Init returns a no-op flush.
func Init(cfg *config.Config, service, release string) func() {
	if cfg == nil || cfg.SentryDSN == "" {
		return func() {}
	}

	err := sentry.Init(sentry.ClientOptions{
		Dsn:              cfg.SentryDSN,
		Environment:      cfg.SentryEnvironment,
		Release:          release,
		ServerName:       cfg.WorkerID,
		SampleRate:       cfg.SentrySampleRate,
		TracesSampleRate: cfg.SentryTracesSampleRate,
		AttachStacktrace: true,
	})
	if err != nil {
		log.Printf("sentry: init failed: %v (error reporting disabled)", err)
		return func() {}
	}

	sentry.ConfigureScope(func(scope *sentry.Scope) {
		scope.SetTag("service", service)
		if cfg.Region != "" {
			scope.SetTag("region", cfg.Region)
		}
		if cfg.WorkerID != "" {
			scope.SetTag("worker_id", cfg.WorkerID)
		}
		if cfg.Mode != "" {
			scope.SetTag("mode", cfg.Mode)
		}
	})

	log.Printf("sentry: enabled (service=%s, env=%s, release=%s)", service, cfg.SentryEnvironment, release)

	return func() {
		sentry.Flush(5 * time.Second)
	}
}

// Recover captures a panic to Sentry and re-panics. Use at the top of main():
//
//	defer observability.Recover()
//
// It is a no-op when Sentry was not initialized.
func Recover() {
	if r := recover(); r != nil {
		sentry.CurrentHub().Recover(r)
		sentry.Flush(5 * time.Second)
		panic(r)
	}
}
