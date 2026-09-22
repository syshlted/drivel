// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/pprof"
	"strings"
	"time"
)

// The debug profiling endpoint (docs/dev/multiclient-test-plan.md §2.7).
//
// /proc answers "did it grow" — RSS, CPU, disk, fds, threads — and nothing else.
// It cannot answer "what grew", and the questions the soak case (MC-53) is
// actually asking are of the second kind: is the goroutine count flat over 24
// hours, and if the heap is climbing, what is retaining it. Both need
// in-process instrumentation, and net/http/pprof is the one every Go tool
// already speaks.
//
// It is off unless an address is given, and that is not merely a default. This
// endpoint hands anyone who can reach it the process's heap contents — which for
// a filesystem means path names, and in a buffer somewhere, file bytes — and
// lets them start a CPU profile or an execution trace, which is a plausible way
// to make a busy mount slower on request.
//
// So a bind that is not loopback is *refused*, and takes an explicit
// -pprof-allow-remote to proceed. It was warned about and served until the
// warning was found to be arguing with the wrong threat: typing `:6060` is a
// muscle-memory default rather than a decision, and inside a dev container even
// a loopback bind can be published by an editor's port forwarder, which the
// warning never fires for. What the refusal buys is that the exposure is now
// something someone typed a second flag to get.

// startPprof serves the profiling endpoints on addr until ctx is cancelled. It
// returns the address it actually bound — which is not the one passed in when
// that named port 0 — and a function that shuts the server down.
//
// A bind failure is returned rather than logged and swallowed. The whole reason
// to run this is to be measuring during a long run, and a soak that spent a day
// producing nothing because the port was busy is worse than one that refused to
// start.
func startPprof(ctx context.Context, addr string, allowRemote bool) (bound net.Addr, stop func(), err error) {
	addr = withLoopbackHost(addr)
	if !allowRemote && remoteBind(addr) {
		return nil, nil, errRemote(addr)
	}

	// ListenConfig rather than net.Listen: ctx bounds the name resolution, and it
	// is the form the rest of the tree uses (internal/gauth's loopback server).
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("pprof endpoint: %w", err)
	}

	// The backstop for what the string could not decide: a hostname resolves at
	// bind time, and only the listener knows what it landed on. Closing it again
	// costs a socket that existed for microseconds and never served a request.
	if !allowRemote && !loopback(ln.Addr()) {
		_ = ln.Close()
		return nil, nil, errRemote(ln.Addr().String())
	}

	// Registered explicitly rather than by importing for side effect. The side
	// effect is registration on http.DefaultServeMux, which is a mux this process
	// does not serve today and might tomorrow; naming the routes here keeps the
	// exposed surface to what is written down.
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index) // also heap, goroutine, allocs, block, mutex
	// pprof.Cmdline is deliberately not registered. It returns this process's
	// argv, which under M8 names the credentials file, the token file, the
	// backing tree and the account — a map to the secrets rather than the
	// secrets, and nothing in the soak (MC-53) or in `go tool pprof` needs it.
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	srv := &http.Server{
		Handler: mux,
		// Without this a peer that opens a connection and dribbles headers holds
		// the server open indefinitely (Slowloris).
		ReadHeaderTimeout: 10 * time.Second,
		// WriteTimeout is deliberately unset. A CPU profile or an execution trace
		// is a streaming response that lasts as long as the ?seconds= the caller
		// asked for, and a write deadline is a hard cap on it that never resets on
		// progress — so any value here would silently become the longest profile
		// this build can ever take. Same shape as gdrive's ChunkTransferTimeout.
		WriteTimeout: 0,
		// Between requests, though, an idle keep-alive connection should not be
		// held open forever — and it would be, since IdleTimeout falls back to
		// ReadTimeout, which is also unset. A sampler polling every few seconds
		// reconnects without noticing; nothing here streams between requests.
		IdleTimeout: 60 * time.Second,
	}

	log.Printf("pprof endpoint on http://%s/debug/pprof/", ln.Addr())
	// Reachable only with -pprof-allow-remote, which is consent rather than a
	// mistake — so this is a reminder of what is now exposed, not a warning about
	// something that can still be taken back.
	if !loopback(ln.Addr()) {
		log.Printf("WARNING: the pprof endpoint at %s is not on a loopback address, as "+
			"-pprof-allow-remote asked for. It serves this process's heap — which holds the "+
			"paths, and somewhere the bytes, of files being synced — to anyone who can reach "+
			"it, and lets them start a CPU profile.", ln.Addr())
	}

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("pprof endpoint: %v", err)
		}
	}()

	go func() {
		<-ctx.Done()
		// Detached: ctx is already cancelled by the time we are here, and the
		// grace period is what lets an in-flight profile finish writing rather
		// than land in the collector as a truncated file.
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(stopCtx)
	}()

	return ln.Addr(), func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(stopCtx)
	}, nil
}

// loopback reports whether addr is reachable only from this host. A wildcard
// bind ("" / 0.0.0.0 / ::) is not, which is the case the refusal is really for:
// it is what a user types when they want to reach the endpoint from outside a
// container and have not thought about who else can.
func loopback(addr net.Addr) bool {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return host == "localhost"
	}
	return ip.IsLoopback()
}

// withLoopbackHost expands a bare port to a loopback address, so `-pprof 6060`
// means the thing a user typing it intends. It defines an input that was
// previously invalid — net.Listen rejects "6060" outright — rather than
// reinterpreting a valid one, which is the line that keeps this from being a
// silent change of what was asked for. A wildcard `:6060` is left exactly as
// written, to be refused by name below.
func withLoopbackHost(addr string) string {
	if addr == "" || strings.Contains(addr, ":") {
		return addr
	}
	return net.JoinHostPort("127.0.0.1", addr)
}

// remoteBind reports whether addr names something reachable from off this host,
// when the string alone settles it: a wildcard host (the empty half of ":6060")
// binds every interface, and an IP literal answers for itself. A hostname does
// not settle it — that waits for the address the listener actually took.
func remoteBind(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false // malformed: let Listen produce the error, which says more
	}
	if host == "" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return !ip.IsLoopback()
	}
	return false
}

func errRemote(addr string) error {
	return fmt.Errorf("pprof endpoint: refusing to serve %s, which is not a loopback address. "+
		"It serves this process's heap — which holds the paths, and somewhere the bytes, of files "+
		"being synced — to anyone who can reach it, and lets them start a CPU profile. Bind it to "+
		"localhost and tunnel, or pass -pprof-allow-remote to accept that", addr)
}

// validatePprofFlags rejects -pprof-allow-remote given without -pprof. A flag
// that is accepted and does nothing is M8 rule 6's failure wearing a new hat:
// the user believes they asked for something, and nothing says otherwise.
func validatePprofFlags(addr string, allowRemoteGiven bool) error {
	if allowRemoteGiven && addr == "" {
		return errors.New("-pprof-allow-remote has no effect without -pprof ADDR")
	}
	return nil
}
