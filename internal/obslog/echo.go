package obslog

import (
	"log/slog"
	"time"

	"github.com/labstack/echo/v4"
)

// EchoMiddleware stashes the X-Request-Id header and the :id URL param
// (sandbox_id) into the request context so downstream handler logs are
// automatically tagged. On request completion it emits a single structured
// access-log line.
//
// Place this AFTER middleware.RequestID() — that middleware generates the
// X-Request-Id if absent. Replaces middleware.Logger().
func EchoMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			start := time.Now()
			req := c.Request()

			// middleware.RequestID() (which runs before us) sets the id on
			// the RESPONSE header — that's the authoritative location whether
			// the id was forwarded inbound or generated here. Falling back to
			// the request header is defensive: a future caller might want
			// only the request header set.
			rid := c.Response().Header().Get(echo.HeaderXRequestID)
			if rid == "" {
				rid = req.Header.Get(echo.HeaderXRequestID)
			}
			rf := RequestFields{
				RequestID: rid,
				SandboxID: c.Param("id"),
			}
			ctx := WithRequest(req.Context(), rf)
			c.SetRequest(req.WithContext(ctx))

			err := next(c)

			res := c.Response()
			attrs := []any{
				slog.String("event", "http_request"),
				slog.String("method", req.Method),
				slog.String("path", c.Path()),
				slog.String("uri", req.RequestURI),
				slog.Int("status", res.Status),
				slog.Int64("duration_ms", time.Since(start).Milliseconds()),
				slog.String("remote_ip", c.RealIP()),
				slog.Int64("bytes_in", req.ContentLength),
				slog.Int64("bytes_out", res.Size),
			}
			if err != nil {
				attrs = append(attrs, slog.String("err", err.Error()))
			}

			level := slog.LevelInfo
			switch {
			case res.Status >= 500:
				level = slog.LevelError
			case res.Status >= 400:
				level = slog.LevelWarn
			}
			slog.LogAttrs(ctx, level, "http_request", toSlogAttrs(attrs)...)
			return err
		}
	}
}

func toSlogAttrs(in []any) []slog.Attr {
	out := make([]slog.Attr, 0, len(in))
	for _, a := range in {
		if attr, ok := a.(slog.Attr); ok {
			out = append(out, attr)
		}
	}
	return out
}
