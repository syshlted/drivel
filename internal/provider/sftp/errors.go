package sftp

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"syscall"

	libsftp "github.com/pkg/sftp"
)

// transientError marks a failure the engine may retry. It satisfies the anonymous
// interface{ Retryable() bool } that provider.IsRetryable probes for, so the sync
// engine classifies retryability without importing this package.
type transientError struct{ err error }

func (e transientError) Error() string   { return e.err.Error() }
func (e transientError) Unwrap() error   { return e.err }
func (e transientError) Retryable() bool { return true }

// transient wraps err as retryable unconditionally. Use it where the call site
// already knows the failure is one — a refused dial, a broken subsystem — rather
// than re-deriving it from the error's shape.
func transient(err error) error {
	if err == nil {
		return nil
	}
	return transientError{err}
}

// classify wraps err as retryable when it is a transient failure and returns it
// unchanged otherwise. nil passes through.
//
// The split is sharper here than on an HTTP backend, because SFTP's status codes
// are few and coarse. What is retryable is essentially "the session died"; what is
// not is every status the *server* deliberately returned, because a server that
// says "permission denied" or "no such file" will say it again just as fast.
// SSH_FX_FAILURE is the awkward one and is treated as permanent: it is the
// server's catch-all for "I will not do that" (rename onto an existing name, rmdir
// on a non-empty directory), so retrying it burns the backoff budget on an answer
// that will not change.
func classify(err error) error {
	if err == nil || !isTransient(err) {
		return err
	}
	return transientError{err}
}

func isTransient(err error) bool {
	// Cancellation and deadlines are intent, not faults — never retry them. A
	// cancel means shutdown; a deadline means we already bounded the wait.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// A dead session, reported by the protocol.
	if errors.Is(err, libsftp.ErrSSHFxConnectionLost) || errors.Is(err, libsftp.ErrSSHFxNoConnection) {
		return true
	}
	// A dead session, reported by the transport rather than by the protocol. A
	// server that goes away mid-request surfaces as an EOF on the next packet.
	//
	// Plain io.EOF counts here, which is only safe because of where this function
	// is reached from. Every caller is an operation that ran to completion or did
	// not: Get hands the caller the remote handle itself, so an ordinary
	// end-of-file on a read never passes through here, and Put and PutRange copy
	// from a *local* reader, whose EOF io.Copy and ReadFrom both absorb into a nil
	// error. So an EOF arriving here means the transport is gone — which, before
	// this case existed, was classified permanent, leaving the engine to give up on
	// a mount whose only problem was a reconnect.
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	var nerr net.Error
	if errors.As(err, &nerr) {
		return true
	}
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ETIMEDOUT)
}

// sessionDead reports whether err means this connection is no longer usable, so
// the Store should discard it and let the next operation dial a fresh one.
//
// It is deliberately narrower than isTransient: a rate-limited or timed-out
// *operation* is worth retrying without throwing away a working connection, while
// these say the transport itself is gone.
func sessionDead(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return errors.Is(err, libsftp.ErrSSHFxConnectionLost) ||
		errors.Is(err, libsftp.ErrSSHFxNoConnection) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE)
}

// isNotExist reports whether err says the path is not there.
//
// Both spellings are checked on purpose. The client normalises most status
// replies into the fs.ErrNotExist family, but not every path through the library
// does, and a missed not-exist here is not cosmetic: Stat would report an error
// instead of ok=false, and Move would fail instead of returning ErrNotExist for
// the engine to turn into a fresh upload.
func isNotExist(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, libsftp.ErrSSHFxNoSuchFile)
}
