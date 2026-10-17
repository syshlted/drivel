// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package syncengine

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/syshlted/drivel/internal/fsevent"
	"github.com/syshlted/drivel/provider"
)

// Monitor is one mount's observation surface (M14): what the outbound path is
// doing to each path right now, the counters a status API reports, and the
// transitions a subscriber can stream.
//
// It is a VIEW and never an authority. Nothing in this package reads it to
// decide anything — the engine, the pull loop and the sweep only ever write to
// it — and that is the M7 invariant 2 rule applied to a third party: a status a
// tool holds a connection to must not be able to change what gets uploaded. If
// a sync decision ever wants a value that lives here, the value belongs in the
// component that makes the decision, and this gets a copy.
//
// Readers never touch Engine.Run. DESIGN.md §9/M14 asks for "an immutable
// snapshot published on each transition"; what is built is the same property by
// a cheaper route. Copying a map of every pending path on every transition is
// O(paths) per event, so instead every writer takes mu for a handful of map
// operations and never across anything that can block — no channel send, no
// provider call, no I/O — and a reader takes it for one bounded batch of
// lookups. So a status query is answered while dispatch is blocked on a full
// worker queue, which is precisely the moment someone asks "why is it stuck?",
// and is pinned by TestStatusIsServedWhileTheRunLoopIsBlocked.
//
// Every method is safe on a nil *Monitor and does nothing there, so a component
// built without one — every test that predates M14 — is unaffected.
type Monitor struct {
	mu    sync.Mutex
	paths map[string]*pathTrack

	reach reachTrack
	sweep sweepTrack
	pause PauseState
	sat   saturation

	lastPoll    time.Time // last successful change-feed page
	lastAdvance time.Time // last time the cursor moved

	subs     map[*Subscription]struct{}
	debounce time.Duration
	mount    string

	c counters

	// hydrator, if set, is read for its own counters; hydration happens on the
	// FUSE read path in another package, which keeps its numbers itself.
	hydrator HydrationCounter
}

// HydrationCounter is satisfied by *hydrate.Hydrator.
type HydrationCounter interface {
	HydrationStats() (count, bytes, failed int64)
}

// NewMonitor returns an empty monitor for the mount called name.
func NewMonitor(name string) *Monitor {
	return &Monitor{
		paths:    map[string]*pathTrack{},
		subs:     map[*Subscription]struct{}{},
		debounce: DefaultPushDelay,
		mount:    name,
	}
}

// Name is the mount this monitor describes.
func (m *Monitor) Name() string {
	if m == nil {
		return ""
	}
	return m.mount
}

// WatchHydration makes the hydrator's counters part of this mount's statistics.
func (m *Monitor) WatchHydration(h HydrationCounter) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.hydrator = h
	m.mu.Unlock()
}

// pathTrack is the outbound path's knowledge of one path. It exists only while
// something is true of the path; an entry with nothing left to say is deleted,
// so the map's size is the number of paths in motion, not the number ever seen.
//
// The three counts are separate because the stages overlap for one path: a
// second write can land in the debounce map while the first upload is still
// running, and one shared state field would report "synced" the moment the
// first finished.
type pathTrack struct {
	coalescing bool // held in the debounce map
	queued     int  // dispatched (or held by a pause) and not yet started
	active     bool // a worker is executing an operation on it
	attempt    int
	next       time.Time // when the next attempt is due, while retrying
	lastErr    string    // most recent failure; cleared by a success
	failed     bool      // the engine gave up; cleared by a later success
}

func (t *pathTrack) idle() bool {
	return !t.coalescing && t.queued == 0 && !t.active && !t.failed && t.next.IsZero()
}

// Transient is what the outbound path currently holds for one path. Status is
// empty when it holds nothing.
type Transient struct {
	Status      string
	Attempt     int
	NextAttempt time.Time
	Error       string
}

func (t *pathTrack) view() Transient {
	v := Transient{Attempt: t.attempt, NextAttempt: t.next, Error: t.lastErr}
	switch {
	case t.active:
		v.Status = StatusUploading
	case !t.next.IsZero():
		v.Status = StatusRetrying
	case t.coalescing || t.queued > 0:
		v.Status = StatusPending
	case t.failed:
		v.Status = StatusError
	}
	return v
}

// Lookup returns what the outbound path holds for each of paths, in one
// acquisition. Absent paths are absent from the result.
func (m *Monitor) Lookup(paths []string) map[string]Transient {
	out := map[string]Transient{}
	if m == nil {
		return out
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range paths {
		if t, ok := m.paths[p]; ok {
			if v := t.view(); v.Status != "" {
				out[p] = v
			}
		}
	}
	return out
}

// trackLocked returns p's entry, creating it.
func (m *Monitor) trackLocked(p string) *pathTrack {
	t, ok := m.paths[p]
	if !ok {
		t = &pathTrack{}
		m.paths[p] = t
	}
	return t
}

// settleLocked deletes p's entry if it has nothing left to say, and publishes
// the status it now has.
func (m *Monitor) settleLocked(p string, t *pathTrack, final string) {
	status := t.view().Status
	if t.idle() {
		delete(m.paths, p)
	}
	if status == "" {
		status = final
	}
	if status != "" {
		m.publishLocked(Transition{Mount: m.mount, Path: p, Status: status, At: time.Now()})
	}
}

// --- outbound hooks, called by Engine --------------------------------------

// coalescing marks p as held (or no longer held) in the debounce map.
func (m *Monitor) coalescing(p string, on bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.trackLocked(p)
	if t.coalescing == on {
		if !on && t.idle() {
			delete(m.paths, p)
		}
		return
	}
	t.coalescing = on
	m.settleLocked(p, t, "")
}

// pathsOf names the paths an event concerns: a rename is in motion at both ends.
func pathsOf(ev fsevent.Event) []string {
	if ev.Op == fsevent.OpRename && ev.NewPath != "" {
		return []string{ev.Path, ev.NewPath}
	}
	return []string{ev.Path}
}

// enqueued records that ev has left the coalescer for a worker queue — or for
// the held list, while paused, which is still "pending" to anyone asking.
func (m *Monitor) enqueued(ev fsevent.Event) {
	if m == nil {
		return
	}
	m.c[cQueued].Add(1)
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range pathsOf(ev) {
		t := m.trackLocked(p)
		t.queued++
		m.settleLocked(p, t, "")
	}
}

// began records that a worker has started on ev.
func (m *Monitor) began(ev fsevent.Event) {
	if m == nil {
		return
	}
	m.c[cQueued].Add(-1)
	m.c[cInflight].Add(1)
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range pathsOf(ev) {
		t := m.trackLocked(p)
		if t.queued > 0 {
			t.queued--
		}
		t.active = true
		t.attempt = 1
		t.next = time.Time{}
		m.settleLocked(p, t, "")
	}
}

// pushStarted is direct Push (the reconciler's seam), which never queued.
func (m *Monitor) pushStarted(ev fsevent.Event) {
	if m == nil {
		return
	}
	m.c[cInflight].Add(1)
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range pathsOf(ev) {
		t := m.trackLocked(p)
		t.active = true
		t.attempt = 1
		t.next = time.Time{}
		m.settleLocked(p, t, "")
	}
}

// retrying records a failed attempt that will be retried at next.
func (m *Monitor) retrying(ev fsevent.Event, attempt int, next time.Time, err error) {
	if m == nil {
		return
	}
	m.c[cRetries].Add(1)
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range pathsOf(ev) {
		t := m.trackLocked(p)
		t.active = false
		t.attempt = attempt + 1
		t.next = next
		t.lastErr = err.Error()
		m.settleLocked(p, t, "")
	}
}

// attemptAgain marks the retry as running.
func (m *Monitor) attemptAgain(ev fsevent.Event) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range pathsOf(ev) {
		t := m.trackLocked(p)
		t.active = true
		t.next = time.Time{}
		m.settleLocked(p, t, "")
	}
}

// finished records the outcome of an operation on ev: nil is success, anything
// else is an abandonment, and a cancellation (shutdown) is neither.
func (m *Monitor) finished(ev fsevent.Event, err error, cancelled bool) {
	if m == nil {
		return
	}
	m.c[cInflight].Add(-1)
	switch {
	case cancelled:
	case err == nil:
		m.c[cPushed].Add(1)
	default:
		m.c[cAbandoned].Add(1)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, p := range pathsOf(ev) {
		t := m.trackLocked(p)
		t.active = false
		t.next = time.Time{}
		final := ""
		switch {
		case cancelled:
		case err == nil:
			t.failed, t.lastErr, t.attempt = false, "", 0
			final = StatusSynced
			if ev.Op == fsevent.OpUnlink || ev.Op == fsevent.OpRmdir || (ev.Op == fsevent.OpRename && i == 0) {
				final = StatusDeleted
			}
		default:
			t.failed, t.lastErr = true, err.Error()
		}
		m.settleLocked(p, t, final)
	}
}

// count adds n to one counter.
func (m *Monitor) count(k counter, n int64) {
	if m == nil {
		return
	}
	m.c[k].Add(n)
}

// --- inbound hooks, called by Downloader -----------------------------------

// applied publishes an inbound change that reached the backing tree.
func (m *Monitor) applied(p, status string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.publishLocked(Transition{Mount: m.mount, Path: p, Status: status, At: time.Now()})
}

// resyncAll tells every subscriber that more changed than was itemised — a
// subtree moved, a sweep reconciled the tree — so it should re-read.
func (m *Monitor) resyncAll(reason string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for s := range m.subs {
		s.resync(m.mount, reason)
	}
}

// polled records a successful change-feed page.
func (m *Monitor) polled(advanced bool) {
	if m == nil {
		return
	}
	now := time.Now()
	m.mu.Lock()
	m.lastPoll = now
	if advanced {
		m.lastAdvance = now
	}
	m.mu.Unlock()
}

// --- reachability -----------------------------------------------------------

// reachStale is how old the last outcome may be before reachability is
// reported as unknown rather than as whatever it last was. An idle backend we
// have not spoken to in an hour has not been observed, and the design's rule is
// "no traffic, no opinion".
const reachStale = time.Hour

type reachTrack struct {
	lastOK, lastFail time.Time
	fails            int
	lastErr          string
}

// observe classifies the outcome of one provider call. It is the only input to
// reachability, which is OBSERVED and never probed (DESIGN.md §9/M14): a health
// ping spends quota, can succeed while the operation the user cares about fails,
// and invents a failure no user operation would have produced.
//
// A non-retryable error is evidence the backend is reachable — it answered, and
// what it said was "no". Only a retryable one (a timeout, a 5xx, a throttle, a
// dead plugin) counts against it. A cancellation is ours and says nothing.
func (m *Monitor) observe(err error) {
	if m == nil {
		return
	}
	if errors.Is(err, context.Canceled) {
		return
	}
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if err == nil || !provider.IsRetryable(err) {
		m.reach.lastOK = now
		m.reach.fails = 0
		return
	}
	m.reach.lastFail = now
	m.reach.fails++
	m.reach.lastErr = err.Error()
}

// Observe is observe for a provider call made outside this package: lazy
// hydration's fetches, which run on the FUSE read path in internal/hydrate and
// report through hydrate.Hydrator.ReportOutcomes. A lazy mount nobody writes to
// reaches its backend nowhere else.
func (m *Monitor) Observe(err error) { m.observe(err) }

// Reachability values.
const (
	ReachOnline  = "online"
	ReachOffline = "offline"
	ReachUnknown = "unknown"
)

func (r reachTrack) state(now time.Time) string {
	last := r.lastOK
	if r.lastFail.After(last) {
		last = r.lastFail
	}
	switch {
	case last.IsZero() || now.Sub(last) > reachStale:
		return ReachUnknown
	case r.fails > 0:
		return ReachOffline
	}
	return ReachOnline
}

// --- sweeps -------------------------------------------------------------------

type sweepTrack struct {
	running   bool
	started   time.Time
	completed int64
	last      *SweepReport
}

// SweepReport is the outcome of one enumeration sweep, as its summary line
// reports it.
type SweepReport struct {
	Started  time.Time
	Finished time.Time
	// Complete is false for a sweep that ended early (a stalled cursor): it
	// materialised what it listed and concluded nothing.
	Complete bool

	Listed, Reconciled, Pushed, NotFetched, ExportOnly int
	DeletedLocally, DeletedRemotely, Kept, Orphaned    int
	Special, Repaired, Repushed, Unverified            int
	Suppressed, TombstonesExpired                      int
}

func (m *Monitor) sweepStarted() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.sweep.running, m.sweep.started = true, time.Now()
	m.mu.Unlock()
}

func (m *Monitor) sweepEnded(r *SweepReport) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.sweep.running = false
	if r != nil {
		m.sweep.last = r
		if r.Complete {
			m.sweep.completed++
		}
	}
	m.mu.Unlock()
	if r != nil {
		m.resyncAll("sweep finished")
	}
}

// --- back-pressure ----------------------------------------------------------

type saturation struct {
	on       bool
	since    time.Time
	episodes int64
}

func (m *Monitor) saturated(on bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if on && !m.sat.on {
		m.sat.episodes++
		m.sat.since = time.Now()
	}
	m.sat.on = on
}

// coalesced records how many paths the debounce map holds. Set by the run loop
// after every event, which is cheap and keeps the depth readable without asking
// the goroutine that owns the map.
func (m *Monitor) coalesced(n int) {
	if m == nil {
		return
	}
	m.c[cCoalescing].Store(int64(n))
}

// --- pause --------------------------------------------------------------------

// PauseState is what a status reader is told about a pause.
type PauseState struct {
	Paused bool
	Since  time.Time
	Until  time.Time
	// Held is how many operations the pause is holding back, and Cap is the
	// number at which it gives up and resumes on its own.
	Held int
	Cap  int
	// LastAutoResume explains the most recent pause that ended without being
	// asked to: it expired, it overflowed, or the mount shut down.
	LastAutoResume string
}

func (m *Monitor) paused(p PauseState) {
	if m == nil {
		return
	}
	m.mu.Lock()
	prev := m.pause.LastAutoResume
	if p.LastAutoResume == "" {
		p.LastAutoResume = prev
	}
	was := m.pause.Paused
	m.pause = p
	m.mu.Unlock()
	if was != p.Paused {
		status := "resumed"
		if p.Paused {
			status = "paused"
		}
		m.mu.Lock()
		m.publishLocked(Transition{Mount: m.mount, Status: status, At: time.Now()})
		m.mu.Unlock()
	}
}

func (m *Monitor) heldCount(n int) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.pause.Held = n
	m.mu.Unlock()
}

// --- counters & snapshot ----------------------------------------------------

// counter names one of a mount's counters. Monotonic for the life of the
// process, except the three gauges at the end. Atomic so the hot path pays one
// instruction and never the lock; an array indexed by name so that adding to
// one is safe on a nil *Monitor, which taking a field's address is not.
type counter int

const (
	cUploads counter = iota
	cUploadBytes
	cRangePuts
	cRangeBytes
	cUnchangedSkips
	cUnchangedBytes
	cPlaceholderDeclined
	cMkdirs
	cDeletes
	cRenames
	cPushed
	cRetries
	cAbandoned
	cDownloads
	cDownloadBytes
	cPlaceholders
	cRestamps
	cRemoteDeletes
	cRemoteMoves
	cConflicts
	cSuppressed
	cPolls
	cPollErrors
	// gauges
	cQueued
	cInflight
	cCoalescing
	numCounters
)

type counters [numCounters]atomic.Int64

func (c *counters) get(k counter) int64 { return c[k].Load() }

// Stats is one mount's statistics at an instant. Every field is read from
// something the engine maintains for its own reasons; nothing here is computed
// by asking a provider.
type Stats struct {
	At time.Time

	Uploads, UploadBytes            int64
	RangePuts, RangeBytes           int64
	UnchangedSkips, UnchangedBytes  int64
	PlaceholdersDeclined            int64
	Mkdirs, Deletes, Renames        int64
	Pushed, Retries, Abandoned      int64
	Downloads, DownloadBytes        int64
	PlaceholdersCreated, Restamps   int64
	RemoteDeletes, RemoteMoves      int64
	Conflicts, SuppressedOwnDeletes int64
	Polls, PollErrors               int64
	Hydrations, HydratedBytes       int64
	HydrationFailures               int64

	Queued, InFlight, Coalescing int64
	PathsInMotion                int

	Saturated          bool
	SaturatedSince     time.Time
	BackPressureEvents int64

	Reachability     string
	LastSuccess      time.Time
	LastFailure      time.Time
	ConsecutiveFails int
	LastError        string

	LastPoll          time.Time
	LastCursorAdvance time.Time

	Sweeping        bool
	SweepStarted    time.Time
	SweepsCompleted int64
	LastSweep       *SweepReport

	Pause PauseState
}

// Snapshot reads every counter and gauge.
func (m *Monitor) Snapshot() Stats {
	if m == nil {
		return Stats{At: time.Now(), Reachability: ReachUnknown}
	}
	now := time.Now()
	c := &m.c
	s := Stats{
		At:      now,
		Uploads: c.get(cUploads), UploadBytes: c.get(cUploadBytes),
		RangePuts: c.get(cRangePuts), RangeBytes: c.get(cRangeBytes),
		UnchangedSkips: c.get(cUnchangedSkips), UnchangedBytes: c.get(cUnchangedBytes),
		PlaceholdersDeclined: c.get(cPlaceholderDeclined),
		Mkdirs:               c.get(cMkdirs), Deletes: c.get(cDeletes), Renames: c.get(cRenames),
		Pushed: c.get(cPushed), Retries: c.get(cRetries), Abandoned: c.get(cAbandoned),
		Downloads: c.get(cDownloads), DownloadBytes: c.get(cDownloadBytes),
		PlaceholdersCreated: c.get(cPlaceholders), Restamps: c.get(cRestamps),
		RemoteDeletes: c.get(cRemoteDeletes), RemoteMoves: c.get(cRemoteMoves),
		Conflicts: c.get(cConflicts), SuppressedOwnDeletes: c.get(cSuppressed),
		Polls: c.get(cPolls), PollErrors: c.get(cPollErrors),
		Queued: c.get(cQueued), InFlight: c.get(cInflight), Coalescing: c.get(cCoalescing),
	}
	m.mu.Lock()
	s.PathsInMotion = len(m.paths)
	s.Saturated, s.SaturatedSince, s.BackPressureEvents = m.sat.on, m.sat.since, m.sat.episodes
	s.Reachability = m.reach.state(now)
	s.LastSuccess, s.LastFailure = m.reach.lastOK, m.reach.lastFail
	s.ConsecutiveFails, s.LastError = m.reach.fails, m.reach.lastErr
	s.LastPoll, s.LastCursorAdvance = m.lastPoll, m.lastAdvance
	s.Sweeping, s.SweepStarted, s.SweepsCompleted = m.sweep.running, m.sweep.started, m.sweep.completed
	if m.sweep.last != nil {
		r := *m.sweep.last
		s.LastSweep = &r
	}
	s.Pause = m.pause
	h := m.hydrator
	m.mu.Unlock()
	if h != nil {
		s.Hydrations, s.HydratedBytes, s.HydrationFailures = h.HydrationStats()
	}
	return s
}

// --- streaming --------------------------------------------------------------

// Transition is one status change, as the event stream reports it. Path is
// empty for a mount-level transition (paused, resumed).
type Transition struct {
	Mount  string
	Path   string
	Status string
	At     time.Time
}

// Subscription receives transitions from one or more monitors.
//
// The producer rules are what make it safe to hand to a stranger's code
// (DESIGN.md §9/M14): a publisher never blocks on it — it takes the
// subscription's own short lock and returns — transitions are coalesced per path
// so an overlay is not sent 400 events for one write, the number of distinct
// paths waiting is bounded, and a subscriber that falls behind that bound is
// DROPPED rather than allowed to hold anything up. Being dropped is reported
// as a resync marker, which tells the client its picture is stale and it should
// re-read, rather than leaving it to assume it missed nothing.
type Subscription struct {
	mu      sync.Mutex
	pending map[string]Transition // keyed mount + "\x00" + path
	order   []string
	cap     int
	dropped bool
	resyncs []Transition
	notify  chan struct{}
}

// DefaultSubscriptionCap is how many distinct paths may wait for one slow
// subscriber before it is dropped.
const DefaultSubscriptionCap = 1024

// NewSubscription returns a subscription holding at most limit distinct
// pending paths (DefaultSubscriptionCap if limit <= 0).
func NewSubscription(limit int) *Subscription {
	if limit <= 0 {
		limit = DefaultSubscriptionCap
	}
	return &Subscription{pending: map[string]Transition{}, cap: limit, notify: make(chan struct{}, 1)}
}

// Subscribe starts delivering this monitor's transitions to s.
func (m *Monitor) Subscribe(s *Subscription) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.subs[s] = struct{}{}
	m.mu.Unlock()
}

// Unsubscribe stops it.
func (m *Monitor) Unsubscribe(s *Subscription) {
	if m == nil {
		return
	}
	m.mu.Lock()
	delete(m.subs, s)
	m.mu.Unlock()
}

// Debounce is the window a subscriber should coalesce over: the uploader's own.
func (m *Monitor) Debounce() time.Duration {
	if m == nil {
		return DefaultPushDelay
	}
	return m.debounce
}

func (m *Monitor) publishLocked(t Transition) {
	for s := range m.subs {
		s.offer(t)
	}
}

func (s *Subscription) offer(t Transition) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dropped {
		return
	}
	k := t.Mount + "\x00" + t.Path
	if _, ok := s.pending[k]; !ok {
		if len(s.order) >= s.cap {
			s.dropped = true
			s.pending, s.order = nil, nil
			s.signal()
			return
		}
		s.order = append(s.order, k)
	}
	s.pending[k] = t
	s.signal()
}

func (s *Subscription) resync(mount, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dropped {
		return
	}
	s.resyncs = append(s.resyncs, Transition{Mount: mount, Status: reason, At: time.Now()})
	s.signal()
}

func (s *Subscription) signal() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// Ready is signalled whenever there is something to Drain.
func (s *Subscription) Ready() <-chan struct{} { return s.notify }

// Drain returns what has accumulated, in first-arrival order per path. dropped
// is true once the subscriber has fallen behind; after that nothing more is
// delivered and the caller should end the stream with a resync marker.
func (s *Subscription) Drain() (ts []Transition, resyncs []Transition, dropped bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dropped {
		return nil, nil, true
	}
	ts = make([]Transition, 0, len(s.order))
	for _, k := range s.order {
		ts = append(ts, s.pending[k])
	}
	s.pending, s.order = map[string]Transition{}, nil
	resyncs, s.resyncs = s.resyncs, nil
	return ts, resyncs, false
}
