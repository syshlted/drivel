// Package transport builds the *http.Client used for all cloud-provider traffic.
//
// Project requirement: prefer HTTP/3 (QUIC). Go's stdlib has no HTTP/3 client, so
// the HTTP/3 leg is quic-go's http3.Transport. Because QUIC runs over UDP/443 and
// quic-go does not fall back to TCP on its own, New returns a composite
// RoundTripper that is HTTP/3-*preferred*: it tries HTTP/3 and, when the QUIC path
// is unavailable for a host (UDP blocked, handshake timeout), transparently retries
// over HTTP/2 and remembers that host so later requests skip the QUIC probe.
//
// The transport sits BELOW auth: callers wrap the returned client's transport with
// oauth2 (see internal/provider) rather than passing a separate token source.
package transport

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

// New returns an HTTP/3-preferred client with HTTP/2 fallback, plus a Closer that
// shuts down idle QUIC connections. Construction cannot fail.
func New() (*http.Client, io.Closer) {
	h3 := &http3.Transport{
		TLSClientConfig: &tls.Config{},
		QUICConfig: &quic.Config{
			// Keep dial failures (UDP blocked) fast so fallback is snappy.
			HandshakeIdleTimeout: 5 * time.Second,
			// Keep the connection warm across the frequent changes.list poll loop.
			KeepAlivePeriod: 30 * time.Second,
		},
	}

	// HTTP/2 fallback: a stdlib transport that negotiates h2 via ALPN.
	h2 := http.DefaultTransport.(*http.Transport).Clone()
	h2.ForceAttemptHTTP2 = true

	rt := &fallback{
		h3:            h3,
		h2:            h2,
		isUnavailable: quicUnavailable,
		down:          make(map[string]bool),
	}
	return &http.Client{Transport: rt}, closerFunc(h3.Close)
}

// fallback is a RoundTripper that prefers h3 and downgrades to h2 per host when
// the QUIC path is unavailable. The h3/h2 fields are interfaces so the selection
// logic is unit-testable with fakes.
type fallback struct {
	h3, h2        http.RoundTripper
	isUnavailable func(error) bool

	mu   sync.RWMutex
	down map[string]bool // host -> HTTP/3 known-unavailable
}

func (f *fallback) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.URL.Hostname()
	if f.isDown(host) {
		return f.h2.RoundTrip(req)
	}

	resp, err := f.h3.RoundTrip(req)
	if err == nil {
		return resp, nil
	}
	// A genuine HTTP-level error (bad status is not an error here; only transport
	// failures are) that isn't about QUIC availability must propagate unchanged.
	if !f.isUnavailable(err) {
		return nil, err
	}

	// QUIC is unusable for this host; remember it and retry over HTTP/2.
	f.markDown(host)
	if req.Body != nil {
		if req.GetBody == nil {
			return nil, fmt.Errorf("transport: HTTP/3 unavailable for %s and request body is not replayable: %w", host, err)
		}
		body, berr := req.GetBody()
		if berr != nil {
			return nil, fmt.Errorf("transport: rewinding body for HTTP/2 fallback to %s: %w", host, berr)
		}
		req.Body = body
	}
	return f.h2.RoundTrip(req)
}

func (f *fallback) isDown(host string) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.down[host]
}

func (f *fallback) markDown(host string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down[host] = true
}

// quicUnavailable reports whether err means the QUIC/HTTP-3 path could not be used
// for this host (as opposed to a real HTTP error we should surface). It is
// deliberately conservative-but-broad: a false positive only costs one HTTP/2
// retry, whereas a false negative would surface a spurious failure to the caller.
func quicUnavailable(err error) bool {
	if err == nil {
		return false
	}
	// UDP dial problems (blocked/refused/unreachable) surface as net.OpError.
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	// Handshake/idle timeouts from quic-go.
	var hErr *quic.HandshakeTimeoutError
	if errors.As(err, &hErr) {
		return true
	}
	var iErr *quic.IdleTimeoutError
	if errors.As(err, &iErr) {
		return true
	}
	// Any net.Error timeout during connection establishment.
	var nErr net.Error
	if errors.As(err, &nErr) && nErr.Timeout() {
		return true
	}
	// Last-resort string match for QUIC failures that don't expose a typed error.
	msg := strings.ToLower(err.Error())
	for _, s := range []string{"no recent network activity", "handshake", "quic", "datagram"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

type closerFunc func() error

func (c closerFunc) Close() error { return c() }
