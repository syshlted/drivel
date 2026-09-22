// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// get fetches one profiling URL and returns its body.
func get(t *testing.T, url string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("reading %s: %v", url, err)
	}
	return res.StatusCode, string(b)
}

// The endpoint serves the two profiles MC-53 is about: a goroutine count that
// must be flat over a day, and a heap that says what is retaining memory when it
// is not.
func TestPprofServesTheProfilesTheSoakNeeds(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// Port 0: the OS picks a free one and startPprof reports it back. Choosing a
	// port up front and hoping it is free is a race, and these tests run
	// alongside every other test in the tree.
	addr, stop, err := startPprof(ctx, "localhost:0", false)
	if err != nil {
		t.Fatalf("startPprof: %v", err)
	}
	defer stop()
	base := "http://" + addr.String()

	for _, path := range []string{"/debug/pprof/goroutine?debug=1", "/debug/pprof/heap?debug=1"} {
		code, body := get(t, base+path)
		if code != http.StatusOK {
			t.Errorf("GET %s: status %d", path, code)
			continue
		}
		if !strings.Contains(body, "goroutine") && !strings.Contains(body, "heap profile") {
			t.Errorf("GET %s returned %d bytes that look like neither profile", path, len(body))
		}
	}

	// The index has to be there too: it is what `go tool pprof` and a browser
	// both land on, and it is the only listing of what else is available.
	if code, body := get(t, base+"/debug/pprof/"); code != http.StatusOK || !strings.Contains(body, "goroutine") {
		t.Errorf("the index at /debug/pprof/ returned %d and did not list the profiles", code)
	}
}

// A cancelled context takes the endpoint down. A profiling server that outlives
// the process's shutdown holds the port against the next run, which for a soak
// means the restart comes up with no instrumentation at all.
func TestPprofStopsWithTheContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	addr, stop, err := startPprof(ctx, "localhost:0", false)
	if err != nil {
		t.Fatalf("startPprof: %v", err)
	}
	defer stop()
	base := "http://" + addr.String()

	if code, _ := get(t, base+"/debug/pprof/"); code != http.StatusOK {
		t.Fatalf("endpoint was not up before cancellation: %d", code)
	}
	cancel()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, base+"/debug/pprof/", nil) //nolint:noctx // deliberately outlives the cancelled ctx
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return // refused: the server is down, which is the assertion
		}
		_ = res.Body.Close()
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the pprof endpoint was still serving 10s after its context was cancelled")
}

// A port already in use is an error, not a warning. A run that was started to be
// measured and silently was not is the failure this prevents.
func TestPprofRefusesAnAddressItCannotBind(t *testing.T) {
	busy, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = busy.Close() }()

	_, stop, err := startPprof(t.Context(), busy.Addr().String(), false)
	if err == nil {
		stop()
		t.Fatal("startPprof accepted an address that was already taken; the run would have produced no profiles and said nothing")
	}
	if !strings.Contains(err.Error(), "pprof") {
		t.Errorf("error does not name the endpoint that failed: %v", err)
	}
}

// The refusal fires for a wildcard bind and stays quiet for loopback. It is the
// only thing standing between "I wanted to reach it from outside the container"
// and serving this process's heap — file paths, and somewhere file bytes — to
// whoever else is on that network.
func TestLoopbackRecognisesWhatIsAndIsNotLocal(t *testing.T) {
	for _, tc := range []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:6060", true},
		{"[::1]:6060", true},
		{"localhost:6060", true},
		{"0.0.0.0:6060", false},
		{"[::]:6060", false},
		{"192.168.1.10:6060", false},
	} {
		if got := loopback(fakeAddr(tc.addr)); got != tc.want {
			t.Errorf("loopback(%q) = %v; want %v", tc.addr, got, tc.want)
		}
	}
}

type fakeAddr string

func (a fakeAddr) Network() string { return "tcp" }
func (a fakeAddr) String() string  { return string(a) }

// argv names the credentials file, the token, the backing tree and the account.
// /debug/pprof/cmdline hands all of it to an unauthenticated caller, and nothing
// in MC-53 or `go tool pprof` asks for it, so the route is not registered.
func TestPprofDoesNotServeTheCommandLine(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	addr, stop, err := startPprof(ctx, "localhost:0", false)
	if err != nil {
		t.Fatalf("startPprof: %v", err)
	}
	defer stop()

	code, body := get(t, "http://"+addr.String()+"/debug/pprof/cmdline")
	if code == http.StatusOK {
		t.Errorf("/debug/pprof/cmdline was served (status %d): %q", code, body)
	}
	// Not just "some non-200": assert the argv itself did not come back, which is
	// the thing being withheld. os.Args[0] is this test binary's path.
	if strings.Contains(body, os.Args[0]) {
		t.Errorf("/debug/pprof/cmdline returned this process's command line: %q", body)
	}
}

// A non-loopback bind is refused rather than served-with-a-warning. The address
// under test is a wildcard, which is what a user types when they want to reach
// the endpoint from outside a container — the case where a warning is read after
// the heap is already exposed.
func TestPprofRefusesANonLoopbackBind(t *testing.T) {
	_, stop, err := startPprof(t.Context(), "0.0.0.0:0", false)
	if err == nil {
		stop()
		t.Fatal("startPprof served a wildcard bind without -pprof-allow-remote")
	}
	// The error has to name the way out, or the only remaining move is to give up
	// on profiling entirely.
	if !strings.Contains(err.Error(), "-pprof-allow-remote") {
		t.Errorf("the refusal does not name the flag that permits it: %v", err)
	}
}

// ...and the flag is a real escape hatch, not decoration: someone profiling a
// headless machine has to be able to say yes.
func TestPprofAllowRemoteServesTheBindItRefusedBefore(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	addr, stop, err := startPprof(ctx, "0.0.0.0:0", true)
	if err != nil {
		t.Fatalf("startPprof with -pprof-allow-remote: %v", err)
	}
	defer stop()

	// Reached over loopback, which a wildcard listener also answers on.
	_, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := get(t, "http://"+net.JoinHostPort("127.0.0.1", port)+"/debug/pprof/"); code != http.StatusOK {
		t.Errorf("the endpoint did not serve after remote binding was permitted: %d", code)
	}
}

// A bare port is the shape people type. It used to be a bind error ("missing
// port in address"); it now means loopback, which is the safe reading and the
// intended one.
func TestPprofReadsABarePortAsLoopback(t *testing.T) {
	if got := withLoopbackHost("6060"); got != "127.0.0.1:6060" {
		t.Errorf(`withLoopbackHost("6060") = %q; want "127.0.0.1:6060"`, got)
	}
	// A wildcard is left alone rather than quietly rewritten to loopback: it is a
	// valid address that means something else, and silently changing what a user
	// asked for is how a flag stops being trustworthy.
	for _, addr := range []string{":6060", "0.0.0.0:6060", "localhost:6060", ""} {
		if got := withLoopbackHost(addr); got != addr {
			t.Errorf("withLoopbackHost(%q) rewrote it to %q", addr, got)
		}
	}
	if !remoteBind(":6060") {
		t.Error(`remoteBind(":6060") = false; a wildcard binds every interface`)
	}
	if remoteBind("127.0.0.1:6060") {
		t.Error(`remoteBind("127.0.0.1:6060") = true`)
	}
}

// -pprof-allow-remote alone does nothing at all. Accepting it silently would
// leave someone believing they had asked for something.
func TestAllowRemoteWithoutPprofIsRefused(t *testing.T) {
	if err := validatePprofFlags("", true); err == nil {
		t.Error("-pprof-allow-remote was accepted without -pprof")
	}
	if err := validatePprofFlags("localhost:6060", true); err != nil {
		t.Errorf("the two flags together were refused: %v", err)
	}
	if err := validatePprofFlags("", false); err != nil {
		t.Errorf("neither flag given was refused: %v", err)
	}
}
