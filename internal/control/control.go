// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

// Package control is drivel's control & status API (M14): HTTP/1.1 + JSON over
// a unix socket, so that software this repository does not ship can ask what a
// running drivel is doing and tell it to stop doing it.
//
// It is the first compatibility surface with code we do not control, and the
// schema — docs/dev/control-api.md, not the Go types here — is the artefact that
// is versioned. Paths are /v1/…, and v1 changes only by adding.
//
// Four rules carry it, and each is the reason for a shape below.
//
//   - It is a view, never an authority. Nothing in the sync path reads anything
//     back out of this package or out of syncengine.Monitor.
//   - A status read never touches the engine's run loop and never calls a
//     provider: local state only, so it is answered while the outbound path is
//     saturated, and a file manager repainting a folder costs no requests.
//   - `unknown` is a first-class answer. A path with no record is not synced.
//   - Shutdown runs the signal path. POST /v1/shutdown cancels the same context
//     SIGTERM does, so the three-phase Close and its bounded drain still run.
//
// Authentication is the filesystem: the socket lives in a 0700 directory and is
// itself 0600, and the peer's uid is checked against ours where the platform can
// report it. An Authorization header is accepted and ignored, so that a token
// scheme for sandboxed clients can be added later without a client having had
// to avoid sending one.
//
// It is NOT merged with the plugin protocol (M9), whose trust runs the other
// way: a plugin is code drivel launches and trusts, a control client is a user
// drivel serves, and one transport serving both is how a plugin ends up able to
// stop the daemon.
package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"strconv"
	"time"

	"github.com/syshlted/drivel/internal/syncengine"
)

// APIVersion is the schema version every path is prefixed with.
const APIVersion = 1

// Mount is what the server needs from one mount; *app.Mount satisfies it.
type Mount interface {
	Name() string
	Mountpoint() string
	Provider() string
	Lazy() bool
	Stats() syncengine.Stats
	Monitor() *syncengine.Monitor
	Status(rel string) (syncengine.PathStatus, error)
	Children(rel, after string, limit int) ([]syncengine.PathStatus, bool, error)
	Pause(ctx context.Context, d time.Duration) (time.Time, error)
	Resume(ctx context.Context) error
}

// Server serves the control API for one process's mounts.
type Server struct {
	mounts   []Mount
	byName   map[string]Mount
	shutdown func(reason string)
	lg       *log.Logger
	started  time.Time
	srv      *http.Server
	uid      int
}

// New builds a server over mounts. shutdown is called, once per request, to
// stop the process the way a signal would; reason says who asked.
func New(mounts []Mount, shutdown func(reason string), lg *log.Logger) *Server {
	if lg == nil {
		lg = log.Default()
	}
	s := &Server{
		mounts:   mounts,
		byName:   map[string]Mount{},
		shutdown: shutdown,
		lg:       lg,
		started:  time.Now(),
		uid:      os.Getuid(),
	}
	for _, m := range mounts {
		s.byName[m.Name()] = m
	}
	s.srv = &http.Server{
		Handler:           s.Handler(),
		ConnContext:       withPeer,
		ReadHeaderTimeout: 10 * time.Second,
		// WriteTimeout is 0 because /v1/events streams for as long as the client
		// stays; the stream sets a deadline per write instead, so a client that
		// stops reading is dropped rather than holding a goroutine forever.
		IdleTimeout: 60 * time.Second,
	}
	return s
}

// Serve accepts connections on ln until Close.
func (s *Server) Serve(ln net.Listener) error {
	err := s.srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Close stops serving, giving in-flight requests a moment. Streams are ended.
func (s *Server) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.srv.Shutdown(ctx); err != nil {
		return s.srv.Close()
	}
	return nil
}

// Handler is the API's routes, with the peer check in front of all of them.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/version", s.version)
	mux.HandleFunc("GET /v1/mounts", s.listMounts)
	mux.HandleFunc("GET /v1/mounts/{name}", s.mount)
	mux.HandleFunc("GET /v1/mounts/{name}/status", s.status)
	mux.HandleFunc("GET /v1/mounts/{name}/children", s.children)
	mux.HandleFunc("POST /v1/mounts/{name}/pause", s.pause)
	mux.HandleFunc("POST /v1/mounts/{name}/resume", s.resume)
	mux.HandleFunc("POST /v1/shutdown", s.stop)
	mux.HandleFunc("GET /v1/events", s.events)
	mux.HandleFunc("GET /v1/metrics", s.metrics)

	// Profiling over the socket, so it needs no port: filesystem permissions are
	// the authorisation and nothing is left for a port forwarder to publish
	// (DESIGN.md §9/M14, "the profiling endpoint's remaining hardening"). Only
	// the snapshot profiles: `profile` and `trace` let a caller make a busy mount
	// slower on request, and `cmdline` names the credentials files.
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	for _, p := range []string{"profile", "trace", "cmdline"} {
		mux.HandleFunc("GET /debug/pprof/"+p, func(w http.ResponseWriter, _ *http.Request) {
			writeErr(w, http.StatusNotFound, "not_served", "/debug/pprof/"+p+" is not served over the control socket; use -pprof for it")
		})
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "not_found", "no such endpoint: "+r.Method+" "+r.URL.Path)
	})
	return s.checkPeer(mux)
}

// --- peer check ---------------------------------------------------------------

type peer struct{ uid, pid int }

type peerKey struct{}

func withPeer(ctx context.Context, c net.Conn) context.Context {
	if p, ok := peerOf(c); ok {
		return context.WithValue(ctx, peerKey{}, p)
	}
	return ctx
}

func peerFrom(ctx context.Context) (peer, bool) {
	p, ok := ctx.Value(peerKey{}).(peer)
	return p, ok
}

// checkPeer refuses a connection from another user where the platform reports
// one. The directory mode is the primary check; this is the backup for
// platforms where a socket's mode bits are advisory. Root is let through: root
// can read the socket's directory regardless, so refusing it would be theatre.
func (s *Server) checkPeer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := peerFrom(r.Context()); ok && p.uid != s.uid && p.uid != 0 {
			writeErr(w, http.StatusForbidden, "forbidden", fmt.Sprintf("uid %d may not use the control socket of uid %d", p.uid, s.uid))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- helpers ------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeErr(w http.ResponseWriter, code int, kind, msg string) {
	writeJSON(w, code, errorBody{Error: errorDetail{Code: kind, Message: msg}})
}

func (s *Server) lookup(w http.ResponseWriter, r *http.Request) (Mount, bool) {
	name := r.PathValue("name")
	m, ok := s.byName[name]
	if !ok {
		writeErr(w, http.StatusNotFound, "no_such_mount", fmt.Sprintf("no mount named %q", name))
	}
	return m, ok
}

// ts renders a time for the wire; the zero time is omitted rather than sent as
// year one.
func ts(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// --- handlers -----------------------------------------------------------------

func (s *Server) version(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, versionBody{
		API:                 APIVersion,
		PID:                 os.Getpid(),
		Started:             ts(s.started),
		DrainTimeoutSeconds: syncengine.DefaultDrainTimeout.Seconds(),
	})
}

func (s *Server) listMounts(w http.ResponseWriter, _ *http.Request) {
	out := mountsBody{Mounts: []mountSummary{}}
	for _, m := range s.mounts {
		out.Mounts = append(out.Mounts, summaryOf(m, m.Stats()))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) mount(w http.ResponseWriter, r *http.Request) {
	m, ok := s.lookup(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, detailOf(m, m.Stats()))
}

func pathErr(w http.ResponseWriter, err error, rel string) {
	switch {
	case errors.Is(err, syncengine.ErrBadPath):
		writeErr(w, http.StatusBadRequest, "bad_path", fmt.Sprintf("%q is not a clean path relative to the mount root", rel))
	case errors.Is(err, syncengine.ErrNoSuchPath):
		writeErr(w, http.StatusNotFound, "no_such_path", fmt.Sprintf("%q does not exist in this mount and nothing is recorded for it", rel))
	default:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	m, ok := s.lookup(w, r)
	if !ok {
		return
	}
	rel := r.URL.Query().Get("path")
	ps, err := m.Status(rel)
	if err != nil {
		pathErr(w, err, rel)
		return
	}
	writeJSON(w, http.StatusOK, pathOf(ps))
}

func (s *Server) children(w http.ResponseWriter, r *http.Request) {
	m, ok := s.lookup(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	rel := q.Get("path")
	limit := 1000
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > syncengine.MaxChildren {
			writeErr(w, http.StatusBadRequest, "bad_limit", fmt.Sprintf("limit must be between 1 and %d", syncengine.MaxChildren))
			return
		}
		limit = n
	}
	entries, truncated, err := m.Children(rel, q.Get("after"), limit)
	if err != nil {
		pathErr(w, err, rel)
		return
	}
	out := childrenBody{Path: rel, Entries: make([]pathBody, 0, len(entries)), Truncated: truncated}
	for _, e := range entries {
		out.Entries = append(out.Entries, pathOf(e))
	}
	if truncated && len(entries) > 0 {
		out.NextAfter = baseName(entries[len(entries)-1].Path)
	}
	writeJSON(w, http.StatusOK, out)
}

func baseName(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}

// requestTimeout bounds how long a pause or resume waits for the run loop. It
// is answered between events, and while the outbound path is saturated, from
// inside the blocked send — so this is only reached if the engine has stopped.
const requestTimeout = 10 * time.Second

func (s *Server) pause(w http.ResponseWriter, r *http.Request) {
	m, ok := s.lookup(w, r)
	if !ok {
		return
	}
	var d time.Duration
	if v := r.URL.Query().Get("for"); v != "" {
		var err error
		if d, err = time.ParseDuration(v); err != nil || d < time.Second || d > syncengine.MaxPauseFor {
			// Refused rather than clamped: a pause silently shorter or longer than
			// the one asked for is a setting that looks applied and is not.
			writeErr(w, http.StatusBadRequest, "bad_duration",
				fmt.Sprintf("for=%q: a pause lasts between 1s and %s", v, syncengine.MaxPauseFor))
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	until, err := m.Pause(ctx, d)
	if err != nil {
		controlErr(w, err)
		return
	}
	s.lg.Printf("[control] %s paused until %s%s", m.Name(), until.Format(time.RFC3339), peerNote(r))
	writeJSON(w, http.StatusOK, pauseBody{Paused: true, Until: ts(until)})
}

func (s *Server) resume(w http.ResponseWriter, r *http.Request) {
	m, ok := s.lookup(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	if err := m.Resume(ctx); err != nil {
		controlErr(w, err)
		return
	}
	s.lg.Printf("[control] %s resumed%s", m.Name(), peerNote(r))
	writeJSON(w, http.StatusOK, pauseBody{Paused: false})
}

func controlErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, syncengine.ErrLogOnly):
		writeErr(w, http.StatusConflict, "log_only", err.Error())
	case errors.Is(err, syncengine.ErrNotRunning):
		writeErr(w, http.StatusServiceUnavailable, "not_running", err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		writeErr(w, http.StatusServiceUnavailable, "busy", "the sync engine did not answer in time")
	default:
		writeErr(w, http.StatusBadRequest, "refused", err.Error())
	}
}

// peerNote names who asked, for a log line: "what stopped my sync daemon" is
// an operational question, and the answer must be readable afterwards.
func peerNote(r *http.Request) string {
	p, ok := peerFrom(r.Context())
	switch {
	case !ok:
		return " (over the control socket)"
	case p.pid > 0:
		return fmt.Sprintf(" (over the control socket, by uid %d pid %d)", p.uid, p.pid)
	}
	return fmt.Sprintf(" (over the control socket, by uid %d)", p.uid)
}

func (s *Server) stop(w http.ResponseWriter, r *http.Request) {
	reason := "shutdown requested" + peerNote(r)
	// Logged here, before the cancellation, so that the line distinguishing an
	// API shutdown from a signal precedes everything the shutdown itself prints.
	s.lg.Printf("[control] %s: unmounting and draining, as on SIGTERM", reason)
	writeJSON(w, http.StatusAccepted, shutdownBody{
		ShuttingDown:        true,
		DrainTimeoutSeconds: syncengine.DefaultDrainTimeout.Seconds(),
	})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	s.shutdown(reason)
}
