// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

// Package transport builds the *http.Client used for all cloud-provider traffic.
//
// Project requirement: prefer HTTP/3 (QUIC). Go's stdlib has no HTTP/3 client, so
// the HTTP/3 leg is quic-go's http3.Transport. Because QUIC runs over UDP/443 and
// quic-go does not fall back to TCP on its own, New returns a composite
// RoundTripper that is HTTP/3-*preferred*: it tries HTTP/3 and, when the QUIC path
// is unavailable for a host (UDP blocked, handshake timeout), transparently retries
// over HTTP/2 and remembers that host so later requests skip the QUIC probe. That
// memory expires (see h3Retry), because a mount runs for weeks and the thing that
// blocked QUIC usually does not.
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
	// The stdlib always makes DefaultTransport a *http.Transport. If a program
	// has replaced it globally, panicking is the honest outcome: a bare
	// &http.Transport{} fallback would silently drop proxy and timeout settings
	// that the default carries.
	h2 := http.DefaultTransport.(*http.Transport).Clone() //nolint:errcheck // see above
	h2.ForceAttemptHTTP2 = true

	rt := &fallback{
		h3:            h3,
		h2:            h2,
		isUnavailable: quicUnavailable,
		retry:         h3Retry,
		now:           time.Now,
		down:          make(map[string]time.Time),
	}
	return &http.Client{Transport: rt}, closerFunc(h3.Close)
}

// h3Retry is how long a host stays written off before HTTP/3 is probed again.
//
// Remembering the downgrade is necessary — without it every request pays a failed
// QUIC dial — but remembering it *forever* was wrong, and MC-44 measured the
// consequence: UDP/443 taken away for forty seconds and then restored left the
// process on HTTP/2 for the rest of its life, with HTTP/3 never retried. Over a
// mount that runs for weeks, usually from /etc/fstab, that turns one transient
// blip into a permanent downgrade of the transport this project made a
// requirement.
//
// The window is a cost ceiling rather than a guess at how long a network stays
// broken. When QUIC is genuinely gone the re-probe costs one failed dial per
// window per host, bounded by HandshakeIdleTimeout above — and a *local* block
// costs nothing at all, because an output-chain drop is reported to the sender as
// EPERM and fails immediately. Five minutes keeps that under a couple of percent
// of a poll loop's time while recovering HTTP/3 promptly enough to matter. No
// FUSE operation is behind it either way: §2.1 keeps the network off that path,
// so the worst case delays a sync the engine would have retried anyway.
const h3Retry = 5 * time.Minute

// fallback is a RoundTripper that prefers h3 and downgrades to h2 per host when
// the QUIC path is unavailable. The h3/h2 fields are interfaces so the selection
// logic is unit-testable with fakes, and now is injectable for the same reason —
// a test for the re-probe window must not sleep through it.
type fallback struct {
	h3, h2        http.RoundTripper
	isUnavailable func(error) bool
	retry         time.Duration
	now           func() time.Time

	mu   sync.RWMutex
	down map[string]time.Time // host -> when HTTP/3 was last found unavailable
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

// isDown reports whether this host's HTTP/3 path is written off *right now*. A
// mark older than the retry window is treated as lapsed rather than deleted: the
// next request probes HTTP/3 again, and only a failure writes a fresh mark.
func (f *fallback) isDown(host string) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	at, ok := f.down[host]
	if !ok {
		return false
	}
	if f.retry <= 0 {
		// No window configured: the mark never expires. New always sets one; this
		// is so a fallback built directly, as a test may, is still coherent.
		return true
	}
	return f.clock().Sub(at) < f.retry
}

// markDown records that HTTP/3 has just been found unusable for this host. Only
// a *failure* writes here, which is what makes recovery need no second
// mechanism: once the window lapses the mark stops being consulted, a successful
// probe leaves it lapsed, and nothing re-arms it until QUIC breaks again. A
// "clear it on success" path was written, measured against a mutation and
// deleted — it changed no behaviour, only the map's size by one entry per host.
func (f *fallback) markDown(host string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down[host] = f.clock()
}

// clock is injectable so a test for the re-probe window does not have to sleep
// through it, and defaulted so a fallback built without one still works.
func (f *fallback) clock() time.Time {
	if f.now == nil {
		return time.Now()
	}
	return f.now()
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
