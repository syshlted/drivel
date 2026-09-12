package plugin

import (
	"errors"
	"io"
)

// chunkReader turns a gRPC stream of content messages into an io.Reader,
// without buffering the whole transfer.
//
// It exists once rather than twice because Put and HashContent differ only in
// which message type carries the bytes, and that difference is a one-line recv
// function at each call site.
type chunkReader struct {
	recv func() ([]byte, error)

	rest []byte // what the last message had left over
	err  error  // sticky: the stream ended, for good or ill
}

func (r *chunkReader) Read(p []byte) (int, error) {
	for len(r.rest) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		data, err := r.recv()
		if err != nil {
			// A stream that ends cleanly is EOF to the reader. Anything else is
			// decoded first, so a backend reading its own upload sees the host's
			// classification of the failure rather than a gRPC status string.
			if errors.Is(err, io.EOF) {
				r.err = io.EOF
			} else {
				r.err = decodeError(err)
			}
			return 0, r.err
		}
		r.rest = data
	}
	n := copy(p, r.rest)
	r.rest = r.rest[n:]
	return n, nil
}

// drain consumes whatever is left of the stream.
//
// It is called after a successful call, and it is not redundant: a backend may
// legitimately return without having read everything — an unchanged-content
// shortcut, a store that stops at a size it already knows — and returning from
// the handler with the sender mid-send makes gRPC cancel the stream. The host
// would then see its own successful upload reported as a transport failure and
// retry it.
func (r *chunkReader) drain() {
	r.rest = nil
	for r.err == nil {
		if _, err := r.recv(); err != nil {
			r.err = err
			return
		}
	}
}

// sendError marks a failure that came from the wire rather than from the local
// reader.
//
// The distinction is not cosmetic: it decides whether a half-sent transfer may be
// finished. If the *wire* failed, the backend already has its own opinion and
// that opinion is the useful error. If the *local reader* failed, the stream must
// NOT be closed cleanly — a clean close tells the backend the file ended there,
// and it would store a truncated object and report success. The host would then
// record an echo for content that is not what the file holds, which is silent
// corruption and is also a behaviour an in-process backend does not have: there,
// a read error propagates out of Put as a failure.
type sendError struct{ err error }

func (e sendError) Error() string { return e.err.Error() }
func (e sendError) Unwrap() error { return e.err }

// chunked splits a read into messages of at most chunkSize and hands each to
// send. It is the mirror of chunkReader and the only place the host encodes
// content for the wire.
//
// A failure from send is wrapped in sendError; a failure from the reader is
// returned as it is. See sendError for why the caller must tell them apart.
func chunked(r io.Reader, send func([]byte) error) error {
	buf := make([]byte, chunkSize)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if serr := send(buf[:n]); serr != nil {
				return sendError{err: serr}
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
