package observability

import (
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/labstack/echo/v4"
)

// EchoMiddleware returns an echo middleware that:
//   - recovers panics, ships them to Sentry, then re-panics so echo's own
//     Recover middleware can render the 500 response
//   - captures handler errors that resolve to HTTP 5xx (or non-HTTPError values)
//
// It is safe to use when Sentry is not initialized — capture calls no-op.
func EchoMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			hub := sentry.CurrentHub().Clone()
			req := c.Request()

			hub.Scope().SetRequest(req)
			hub.Scope().SetTag("http.route", c.Path())
			hub.Scope().SetTag("http.method", req.Method)

			ctx := sentry.SetHubOnContext(req.Context(), hub)
			c.SetRequest(req.WithContext(ctx))

			defer func() {
				if r := recover(); r != nil {
					hub.RecoverWithContext(ctx, r)
					hub.Flush(2 * time.Second)
					panic(r) // let echo's Recover middleware respond
				}
			}()

			err := next(c)
			if err == nil {
				return nil
			}

			// Only capture server errors. Client errors (4xx) are expected flow.
			if he, ok := err.(*echo.HTTPError); ok && he.Code < 500 {
				return err
			}
			hub.CaptureException(err)
			return err
		}
	}
}
