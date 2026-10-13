// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package transport

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go/http3"
)

// rtFunc adapts a function to http.RoundTripper.
type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func okResp() *http.Response {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok"))}
}

func newReq(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "https://www.googleapis.com/drive/v3/files", nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestPrefersH3WhenAvailable(t *testing.T) {
	var h3Calls, h2Calls int32
	f := &fallback{
		h3:            rtFunc(func(*http.Request) (*http.Response, error) { atomic.AddInt32(&h3Calls, 1); return okResp(), nil }),
		h2:            rtFunc(func(*http.Request) (*http.Response, error) { atomic.AddInt32(&h2Calls, 1); return okResp(), nil }),
		isUnavailable: quicUnavailable,
		down:          map[string]time.Time{},
	}
	if _, err := f.RoundTrip(newReq(t)); err != nil {
		t.Fatal(err)
	}
	if h3Calls != 1 || h2Calls != 0 {
		t.Fatalf("expected h3=1 h2=0, got h3=%d h2=%d", h3Calls, h2Calls)
	}
}

func TestFallsBackAndRemembersHost(t *testing.T) {
	var h3Calls, h2Calls int32
	dialErr := errors.New("quic: handshake did not complete in time")
	f := &fallback{
		h3:            rtFunc(func(*http.Request) (*http.Response, error) { atomic.AddInt32(&h3Calls, 1); return nil, dialErr }),
		h2:            rtFunc(func(*http.Request) (*http.Response, error) { atomic.AddInt32(&h2Calls, 1); return okResp(), nil }),
		isUnavailable: quicUnavailable,
		down:          map[string]time.Time{},
	}
	// First request: probes h3, fails, falls back to h2.
	if _, err := f.RoundTrip(newReq(t)); err != nil {
		t.Fatal(err)
	}
	// Second request: host is remembered as down, so h3 is skipped.
	if _, err := f.RoundTrip(newReq(t)); err != nil {
		t.Fatal(err)
	}
	if h3Calls != 1 {
		t.Fatalf("expected h3 probed once then skipped, got h3=%d", h3Calls)
	}
	if h2Calls != 2 {
		t.Fatalf("expected both requests served by h2, got h2=%d", h2Calls)
	}
}

func TestGenuineErrorPropagatesWithoutFallback(t *testing.T) {
	var h2Calls int32
	realErr := errors.New("stream reset by peer") // not a QUIC-availability error
	f := &fallback{
		h3:            rtFunc(func(*http.Request) (*http.Response, error) { return nil, realErr }),
		h2:            rtFunc(func(*http.Request) (*http.Response, error) { atomic.AddInt32(&h2Calls, 1); return okResp(), nil }),
		isUnavailable: quicUnavailable,
		down:          map[string]time.Time{},
	}
	_, err := f.RoundTrip(newReq(t))
	if !errors.Is(err, realErr) {
		t.Fatalf("expected the genuine error to propagate, got %v", err)
	}
	if h2Calls != 0 {
		t.Fatalf("expected no h2 fallback on a genuine error, got h2=%d", h2Calls)
	}
}

func TestFallbackRewindsReplayableBody(t *testing.T) {
	const payload = "important-bytes"
	var got string
	f := &fallback{
		h3: rtFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("quic: no recent network activity")
		}),
		h2: rtFunc(func(r *http.Request) (*http.Response, error) {
			b, _ := io.ReadAll(r.Body)
			got = string(b)
			return okResp(), nil
		}),
		isUnavailable: quicUnavailable,
		down:          map[string]time.Time{},
	}
	// http.NewRequest with a *strings.Reader sets GetBody automatically.
	req, err := http.NewRequest(http.MethodPost, "https://www.googleapis.com/upload", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	if got != payload {
		t.Fatalf("h2 fallback saw body %q, want %q", got, payload)
	}
}

func TestFallbackRefusesUnreplayableBody(t *testing.T) {
	f := &fallback{
		h3: rtFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("quic: handshake timeout") }),
		h2: rtFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("h2 must not be called with an unrewindable body")
			return nil, nil
		}),
		isUnavailable: quicUnavailable,
		down:          map[string]time.Time{},
	}
	// A bare io.Reader body has no GetBody, so it cannot be replayed.
	req, err := http.NewRequest(http.MethodPost, "https://www.googleapis.com/upload", io.NopCloser(strings.NewReader("x")))
	if err != nil {
		t.Fatal(err)
	}
	req.GetBody = nil
	if _, err := f.RoundTrip(req); err == nil {
		t.Fatal("expected an error when body cannot be replayed for fallback")
	}
}

// The downgrade is remembered, and then it expires. MC-44 took UDP/443 away from
// a running process for forty seconds and gave it back; the process stayed on
// HTTP/2 for the rest of its life, because the memory of the failure had no end.
// A mount runs for weeks, so that is one blip becoming a permanent downgrade of
// the transport this project requires.
func TestHTTP3IsReprobedAfterTheRetryWindow(t *testing.T) {
	var h3Calls, h2Calls int32
	var h3Fails atomic.Bool
	h3Fails.Store(true)

	clock := time.Now()
	f := &fallback{
		h3: rtFunc(func(*http.Request) (*http.Response, error) {
			atomic.AddInt32(&h3Calls, 1)
			if h3Fails.Load() {
				return nil, errors.New("quic: handshake did not complete in time")
			}
			return okResp(), nil
		}),
		h2:            rtFunc(func(*http.Request) (*http.Response, error) { atomic.AddInt32(&h2Calls, 1); return okResp(), nil }),
		isUnavailable: quicUnavailable,
		retry:         5 * time.Minute,
		now:           func() time.Time { return clock },
		down:          map[string]time.Time{},
	}
	get := func() {
		t.Helper()
		if _, err := f.RoundTrip(newReq(t)); err != nil {
			t.Fatal(err)
		}
	}

	// Probe, fail, fall back — and then skip the probe while the window is open.
	get()
	get()
	clock = clock.Add(4 * time.Minute)
	get()
	if h3Calls != 1 {
		t.Errorf("h3 was probed %d times inside the window; want 1, or every request pays a failed dial", h3Calls)
	}
	if h2Calls != 3 {
		t.Errorf("h2 served %d of 3 requests", h2Calls)
	}

	// Past the window, HTTP/3 is tried again. It is still broken, so the mark is
	// refreshed rather than cleared: the next request must not probe again.
	clock = clock.Add(2 * time.Minute)
	get()
	get()
	if h3Calls != 2 {
		t.Errorf("h3 probed %d times; want 2 — once per window, and a failed re-probe must re-arm the window", h3Calls)
	}

	// Now QUIC comes back. The next re-probe succeeds, and because only a failure
	// re-arms the window, every request after it goes to HTTP/3.
	h3Fails.Store(false)
	clock = clock.Add(6 * time.Minute)
	get()
	get()
	get()
	if h3Calls != 5 {
		t.Errorf("h3 handled %d calls; want 5 — a working QUIC path must stop falling back", h3Calls)
	}
	// 5 = the 3 while the first window was open, plus the 2 after the failed
	// re-probe; the 3 since recovery must not add to it.
	if h2Calls != 5 {
		t.Errorf("h2 served %d requests; want 5 — nothing should reach it after recovery", h2Calls)
	}
}

// New's own wiring, because the window only works if it is configured: a
// fallback with no retry duration remembers a downgrade forever, which is
// exactly the behaviour being fixed.
// The two timeouts that decide fallback latency belong to this package, because
// MC-44 measured them and the numbers are quoted in the docs: 30s for a block
// that arrives mid-session, 5s for a dial into one. MaxIdleTimeout is the one
// that was previously left to quic-go's protocol.DefaultIdleTimeout, where a
// dependency bump would have moved a documented, measured property without
// anything here noticing.
func TestNewPinsTheQUICTimeouts(t *testing.T) {
	c, closer := New()
	defer closer.Close() //nolint:errcheck // test
	f, ok := c.Transport.(*fallback)
	if !ok {
		t.Fatalf("New returned transport %T, want the fallback", c.Transport)
	}
	h3, ok := f.h3.(*http3.Transport)
	if !ok {
		t.Fatalf("the HTTP/3 leg is %T, want *http3.Transport", f.h3)
	}
	q := h3.QUICConfig
	if q == nil {
		t.Fatal("QUICConfig is nil, so every timeout is quic-go's default")
	}
	if q.HandshakeIdleTimeout != h3HandshakeIdle {
		t.Errorf("HandshakeIdleTimeout = %v, want %v", q.HandshakeIdleTimeout, h3HandshakeIdle)
	}
	if q.MaxIdleTimeout != h3MaxIdle {
		t.Errorf("MaxIdleTimeout = %v, want %v (0 means quic-go's default decides how long a mid-session block stalls a request)", q.MaxIdleTimeout, h3MaxIdle)
	}
	if q.KeepAlivePeriod != h3KeepAlive {
		t.Errorf("KeepAlivePeriod = %v, want %v", q.KeepAlivePeriod, h3KeepAlive)
	}
	// A keep-alive period at or above the idle timeout would be silently
	// replaced by quic-go's derived interval; it already is here (idleTimeout/2),
	// and the assertion records which way round the two have to be read.
	if h3KeepAlive < h3MaxIdle/2 {
		t.Errorf("KeepAlivePeriod %v is below idleTimeout/2 (%v), so it is the ping rate rather than a ceiling on it", h3KeepAlive, h3MaxIdle/2)
	}
}

func TestNewConfiguresTheRetryWindow(t *testing.T) {
	c, closer := New()
	defer closer.Close() //nolint:errcheck // test
	f, ok := c.Transport.(*fallback)
	if !ok {
		t.Fatalf("New returned transport %T, want the fallback", c.Transport)
	}
	if f.retry != h3Retry || f.retry <= 0 {
		t.Errorf("retry = %v, want %v", f.retry, h3Retry)
	}
	if f.now == nil {
		t.Error("now is nil; isDown would fall back to time.Now and the window would be untestable")
	}
}
