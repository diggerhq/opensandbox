package observability

import (
	"fmt"
	"log"
	"runtime/debug"

	"github.com/getsentry/sentry-go"
)

// CaptureError sends err to Sentry. Safe to call when Sentry is not
// initialized (no-ops). Extra tags can be supplied as key/value pairs.
func CaptureError(err error, tags ...string) {
	if err == nil {
		return
	}
	hub := sentry.CurrentHub()
	if hub.Client() == nil {
		return
	}
	hub.WithScope(func(scope *sentry.Scope) {
		for i := 0; i+1 < len(tags); i += 2 {
			scope.SetTag(tags[i], tags[i+1])
		}
		hub.CaptureException(err)
	})
}

// Go runs fn in a new goroutine, capturing any panic to Sentry. Use this
// instead of bare `go func(){...}()` for long-lived background loops so that
// a panic in the loop is reported before the goroutine dies.
//
// The name argument is attached as a tag so the event is easy to find.
func Go(name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("observability: goroutine %q panicked: %v\n%s", name, r, debug.Stack())
				hub := sentry.CurrentHub()
				if hub.Client() == nil {
					return
				}
				hub.WithScope(func(scope *sentry.Scope) {
					scope.SetTag("goroutine", name)
					hub.Recover(r)
				})
			}
		}()
		fn()
	}()
}

// Errorf captures a formatted error message to Sentry as a new error and
// logs it locally. Useful at error sites that currently use log.Printf but
// want to also surface the event in Sentry.
func Errorf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	log.Print(msg)
	CaptureError(fmt.Errorf("%s", msg))
}
