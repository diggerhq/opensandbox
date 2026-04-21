package observability

import (
	"context"
	"time"

	"github.com/getsentry/sentry-go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// UnaryServerInterceptor returns a gRPC unary interceptor that captures
// server-side errors and panics to Sentry. Client errors (InvalidArgument,
// NotFound, AlreadyExists, PermissionDenied, Unauthenticated, FailedPrecondition,
// OutOfRange, Canceled, DeadlineExceeded) are not captured — they usually
// represent expected flow, not bugs.
func UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp interface{}, err error) {
		hub := sentry.CurrentHub().Clone()
		hub.Scope().SetTag("grpc.method", info.FullMethod)
		ctx = sentry.SetHubOnContext(ctx, hub)

		defer func() {
			if r := recover(); r != nil {
				hub.RecoverWithContext(ctx, r)
				hub.Flush(2 * time.Second)
				panic(r)
			}
		}()

		resp, err = handler(ctx, req)
		if err != nil && shouldCaptureGRPC(err) {
			hub.CaptureException(err)
		}
		return resp, err
	}
}

// StreamServerInterceptor returns a gRPC stream interceptor mirroring
// UnaryServerInterceptor's behavior for streaming RPCs.
func StreamServerInterceptor() grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		hub := sentry.CurrentHub().Clone()
		hub.Scope().SetTag("grpc.method", info.FullMethod)
		hub.Scope().SetTag("grpc.stream", "true")
		ctx := sentry.SetHubOnContext(ss.Context(), hub)

		defer func() {
			if r := recover(); r != nil {
				hub.RecoverWithContext(ctx, r)
				hub.Flush(2 * time.Second)
				panic(r)
			}
		}()

		wrapped := &wrappedStream{ServerStream: ss, ctx: ctx}
		err = handler(srv, wrapped)
		if err != nil && shouldCaptureGRPC(err) {
			hub.CaptureException(err)
		}
		return err
	}
}

// shouldCaptureGRPC returns true for error codes that represent real server
// problems, not expected client-side conditions.
func shouldCaptureGRPC(err error) bool {
	code := status.Code(err)
	switch code {
	case codes.OK,
		codes.Canceled,
		codes.InvalidArgument,
		codes.NotFound,
		codes.AlreadyExists,
		codes.PermissionDenied,
		codes.Unauthenticated,
		codes.FailedPrecondition,
		codes.OutOfRange,
		codes.DeadlineExceeded,
		codes.Aborted:
		return false
	}
	return true
}

type wrappedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *wrappedStream) Context() context.Context { return w.ctx }
