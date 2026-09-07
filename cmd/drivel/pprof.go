package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/pprof"
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

// startPprof serves the profiling endpoints on addr until ctx is cancelled. It
// returns the address it actually bound — which is not the one passed in when
// that named port 0 — and a function that shuts the server down.
//
// A bind failure is returned rather than logged and swallowed. The whole reason
// to run this is to be measuring during a long run, and a soak that spent a day
// producing nothing because the port was busy is worse than one that refused to
// start.
func startPprof(ctx context.Context, addr string) (bound net.Addr, stop func(), err error) {
	// ListenConfig rather than net.Listen: ctx bounds the name resolution, and it
	// is the form the rest of the tree uses (internal/gauth's loopback server).
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("pprof endpoint: %w", err)
	}

	// Registered explicitly rather than by importing for side effect. The side
	// effect is registration on http.DefaultServeMux, which is a mux this process
	// does not serve today and might tomorrow; naming the routes here keeps the
	// exposed surface to what is written down.
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index) // also heap, goroutine, allocs, block, mutex
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
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
	}

	log.Printf("pprof endpoint on http://%s/debug/pprof/", ln.Addr())
	if !loopback(ln.Addr()) {
		log.Printf("WARNING: the pprof endpoint at %s is not on a loopback address. "+
			"It serves this process's heap — which holds the paths, and somewhere the bytes, "+
			"of files being synced — to anyone who can reach it, and lets them start a CPU "+
			"profile. Bind it to localhost and tunnel instead.", ln.Addr())
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
// bind ("" / 0.0.0.0 / ::) is not, which is the case the warning is really for:
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
