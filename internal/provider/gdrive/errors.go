package gdrive

import (
	"context"
	"errors"
	"net"
	"net/http"
	"syscall"

	"google.golang.org/api/googleapi"
)

// transientError marks a Drive/network failure the engine may retry. It satisfies
// the anonymous interface{ Retryable() bool } that provider.IsRetryable probes for,
// so the sync engine classifies retryability without importing this package.
type transientError struct{ err error }

func (e transientError) Error() string   { return e.err.Error() }
func (e transientError) Unwrap() error   { return e.err }
func (e transientError) Retryable() bool { return true }

// classify wraps err as retryable when it's a transient failure (rate limit, 5xx,
// dropped connection) and returns it unchanged otherwise. nil passes through.
// Every Store mutation returns classify(err) so the uploader's backoff loop can
// tell "try again" from "give up".
func classify(err error) error {
	if err == nil || !isTransient(err) {
		return err
	}
	return transientError{err}
}

func isTransient(err error) bool {
	// Context cancellation / deadline is intent, not a transient fault — never retry
	// it (a cancel means shutdown; a deadline means we already bounded the wait).
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	var ae *googleapi.Error
	if errors.As(err, &ae) {
		switch ae.Code {
		case http.StatusTooManyRequests, // 429
			http.StatusInternalServerError, // 500
			http.StatusBadGateway,          // 502
			http.StatusServiceUnavailable,  // 503
			http.StatusGatewayTimeout:      // 504
			return true
		case http.StatusForbidden: // 403 — Drive rate limits masquerade as 403
			for _, e := range ae.Errors {
				if e.Reason == "rateLimitExceeded" || e.Reason == "userRateLimitExceeded" {
					return true
				}
			}
		}
		return false
	}

	// Network-level failures: QUIC/UDP dial trouble, resets, refused conns, timeouts.
	var nerr net.Error
	if errors.As(err, &nerr) {
		return true
	}
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED)
}
