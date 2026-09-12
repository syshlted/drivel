package plugin

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/zishmusic/drivel/plugin/internal/pb"
	"github.com/zishmusic/drivel/provider"
)

// An error crossing the seam has to survive as more than its text, because the
// engine switches on three things and every one of them is load-bearing:
//
//   - provider.ErrNotExist means "the source of this Move was never uploaded",
//     which the engine answers by pushing the destination as fresh content. Lost,
//     it becomes a hard failure and a rename stops syncing.
//   - provider.ErrCursorExpired means "this cursor can never be answered again",
//     which the engine answers by re-enumerating (M7b). Lost, it looks like a
//     transient failure, the pull loop backs off and retries forever, and inbound
//     sync silently stops — which is exactly the bug that made the sentinel exist.
//   - provider.IsRetryable means "back off and try again". Lost, the first rate
//     limit becomes permanent.
//
// So a plugin classifies its own error — it is the only side that can, since the
// backend's error types are its own — and sends the classification alongside the
// message as a gRPC status detail. The host rebuilds an error that answers
// errors.Is and provider.IsRetryable the same way the original did.
//
// What is deliberately NOT preserved is the error's concrete type. A host cannot
// type-assert its way into a plugin's package, and pretending otherwise would
// invite exactly the kind of coupling the seam exists to prevent.

// retryableError is the host-side rebuild of an error a plugin classified as
// transient. It satisfies the same `interface{ Retryable() bool }` convention
// provider.IsRetryable looks for, so the engine's backoff needs no knowledge
// that the error came from another process.
type retryableError struct{ err error }

func (e retryableError) Error() string   { return e.err.Error() }
func (e retryableError) Unwrap() error   { return e.err }
func (e retryableError) Retryable() bool { return true }

// encodeError turns a provider-side error into a gRPC status carrying its
// classification. A nil error stays nil.
//
// Context cancellation is mapped to its own code rather than to a detail: it is
// the host's own doing, and dressing it up as a backend failure would put
// "provider: …" in front of a message about a mount shutting down.
func encodeError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, err.Error())
	}

	detail := &pb.Error{
		Message:   err.Error(),
		Retryable: provider.IsRetryable(err),
	}
	code := codes.Unknown
	switch {
	case errors.Is(err, provider.ErrNotExist):
		detail.Kind = pb.Error_KIND_NOT_EXIST
		code = codes.NotFound
	case errors.Is(err, provider.ErrCursorExpired):
		detail.Kind = pb.Error_KIND_CURSOR_EXPIRED
		code = codes.FailedPrecondition
	}

	st := status.New(code, err.Error())
	withDetail, derr := st.WithDetails(detail)
	if derr != nil {
		// Attaching a detail can only fail if the detail cannot be marshalled,
		// which for a message of our own with three scalar fields it cannot. Fall
		// back to the plain status rather than replacing the backend's error with a
		// complaint about our own plumbing.
		return st.Err()
	}
	return withDetail.Err()
}

// decodeError is encodeError's inverse: it rebuilds an error that answers
// errors.Is and provider.IsRetryable as the plugin's original did.
//
// A status with no detail still produces a usable error — an older plugin, or a
// failure raised by gRPC itself rather than by the backend — and in that case the
// transport's own view is used: codes.Unavailable and codes.ResourceExhausted
// are the two gRPC raises on its own behalf that a retry can actually fix, and
// treating them as retryable is what lets the engine ride out a plugin restart
// instead of failing the push that was in flight.
func decodeError(err error) error {
	if err == nil {
		return nil
	}
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	switch st.Code() {
	case codes.Canceled:
		return context.Canceled
	case codes.DeadlineExceeded:
		return context.DeadlineExceeded
	}

	var detail *pb.Error
	for _, d := range st.Details() {
		if e, is := d.(*pb.Error); is {
			detail = e
			break
		}
	}
	if detail == nil {
		out := errors.New(st.Message())
		if st.Code() == codes.Unavailable || st.Code() == codes.ResourceExhausted {
			return retryableError{err: out}
		}
		return out
	}

	var out error
	switch detail.GetKind() {
	case pb.Error_KIND_NOT_EXIST:
		out = fmt.Errorf("%w: %s", provider.ErrNotExist, detail.GetMessage())
	case pb.Error_KIND_CURSOR_EXPIRED:
		out = fmt.Errorf("%w: %s", provider.ErrCursorExpired, detail.GetMessage())
	default:
		out = errors.New(detail.GetMessage())
	}
	if detail.GetRetryable() {
		return retryableError{err: out}
	}
	return out
}

// transportError wraps a failure of the plugin mechanism itself — the process
// died, the socket went away, the executable could not be launched — as a
// retryable error.
//
// Retryable is the right classification and it is not a hedge. A plugin process
// is something the host can bring back; the engine's existing backoff is exactly
// the loop that should be waiting while it does, and it already knows how to
// give up. The alternative — a permanent error — would fail every pending push
// at the moment a backend crashed, which is the outcome out-of-process loading
// was supposed to prevent.
func transportError(format string, args ...any) error {
	return retryableError{err: fmt.Errorf(format, args...)}
}
