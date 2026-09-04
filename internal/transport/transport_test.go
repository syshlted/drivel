package transport

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
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
		down:          map[string]bool{},
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
		down:          map[string]bool{},
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
		down:          map[string]bool{},
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
		down:          map[string]bool{},
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
		down:          map[string]bool{},
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
