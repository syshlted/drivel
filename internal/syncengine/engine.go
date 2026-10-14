// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

// Package syncengine consumes filesystem change events and applies them to a
// cloud store (outbound/push via Engine), and polls the store's change feed to
// apply remote deltas back to the underlying directory (inbound/pull via
// Downloader — see downloader.go).
//
// The store is path-addressed (provider.Store), so the engine speaks only
// root-relative paths — path↔native-ID translation lives inside the provider.
// Both directions coordinate through the engine-level state store
// (internal/state): the uploader records what it pushed and the downloader drops
// change-feed reports of that same content, the §4 echo/loop-suppression model.
//
// Outbound path (DESIGN.md §5, §7): events are debounced per path (a burst of
// writes coalesces into one upload), dispatched to a bounded pool of path-hashed
// workers (same path → same worker → ordered, serialised), and each op is retried
// with exponential backoff on transient provider errors. On shutdown the pending
// and in-flight work is drained under a bounded deadline before Run returns. With
// no store configured, the Engine runs in log-only mode (M1 behaviour).
package syncengine

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/syshlted/drivel/internal/fsevent"
	"github.com/syshlted/drivel/internal/state"
	"github.com/syshlted/drivel/provider"
	"github.com/syshlted/drivel/ranges"
)

// Defaults for the outbound uploader. All are overridable via Config.
const defaultDrainTimeout = 30 * time.Second

// DefaultPushDelay is how long a path must go without a further content change
// before its upload is dispatched — Config.Debounce's default, and what
// `drivel mount -push-delay` overrides.
//
// It is short because the events it coalesces are already coarse: OpWrite is
// emitted once per close (push-on-close, internal/vfs/file.go), not once per
// write(2), so a 50 GB copy arrives as one OpCreate and one OpWrite rather than
// a stream. The window exists to merge those two and to absorb a program that
// closes a file several times in quick succession, not to batch a transfer.
//
// Raising it trades promptness for quota: a file rewritten repeatedly inside the
// window is uploaded once instead of once per close, and the outbound queue
// drains on a longer cycle, which is what keeps a bulk import from filling the
// event channel and stalling the FUSE handler (see eventBuffer in internal/app).
const DefaultPushDelay = 300 * time.Millisecond

// maxWaitFactor bounds how long a path may be held by repeated re-arming of its
// debounce timer, as a multiple of the delay itself: a pending change is always
// dispatched within DefaultPushDelay*maxWaitFactor of the *first* change that
// made it pending, however much activity follows.
//
// Without it, a path touched again inside every window is never dispatched at
// all, and the longer -push-delay is set the easier that is to hit — a script
// appending to a file once a second starves a 5m delay forever. It cannot
// disturb a large copy, because a copy emits one OpWrite at close rather than
// one per write, so there is no stream of events to keep re-arming the timer.
//
// Ten rather than two so that the coalescing a long delay was set to buy is not
// undone at the first sign of activity; at the 300ms default it puts the bound
// at 3s, which nothing observable reaches.
const maxWaitFactor = 10

// DefaultWorkers is the out-of-the-box size of the path-hashed upload pool.
//
// It is deliberately small. Concurrency here buys latency-hiding, not bandwidth:
// every push shares one HTTP/3 connection and therefore one congestion window,
// so raising it overlaps the per-file round trips (auth, the M6 Stat gate,
// metadata) rather than moving more bytes. Against that, each in-flight upload
// can hold a provider-sized chunk buffer, and past the point the provider starts
// refusing requests, more workers cost quota and gain nothing — the M7c descent
// measured 97x the ideal request count when it pushed a throttled provider
// harder. Four is a floor that is never the bottleneck on a consumer uplink;
// tuning up is the user's call, per mount.
const DefaultWorkers = 4

// workerQueue is how many events may wait in front of ONE upload worker.
//
// It and eventBuffer (internal/app) are the two bounds on the outbound path, and
// together they are what the mount can absorb before a write(2) through the
// mountpoint has to wait: 1024 + 4*64 = 1280 events at the defaults. MC-11
// measured a 1286-deep backlog against that, which is the bound doing its job.
// It is named here rather than written inline because the saturation report has
// to quote it.
//
// **That sum is the best case, not the bound a given workload meets.** Paths are
// hashed to workers, so a burst concentrated on one worker blocks once *its* 64
// slots are full while the other three sit idle — the practical bound is then
// eventBuffer + 64. Which is why the saturation line reports the depth it
// actually reached rather than asserting the queue is full: with four workers it
// first reported 646 of 1280.
const workerQueue = 64

// defaultSatSample is how often a saturated outbound path is re-examined to see
// whether it still is.
//
// It is a sampling period, not a reporting period: nothing is logged on a tick,
// only on the transition it detects. The sampler exists at all because the event
// that ends an episode is the one that never arrives — a bulk copy finishes, the
// queues drain, and no further dispatch happens to notice. It runs only while
// saturated, so an idle mount arms no timer.
const defaultSatSample = time.Second

// An episode ends when the coalescer has nothing left to hand to a worker and
// no dispatch waited over the last sampling interval. Hysteresis is not a
// refinement here, it is the feature: in steady saturation every dispatch finds
// room the instant a worker frees a slot and then refills it, so "did this send
// block?" flips thousands of times a minute and a report keyed on it would be
// the ten-thousand-line log this exists to avoid.
//
// It is NOT a fraction of the bound, which is what shipped and what MC-12 found
// wrong on 2026-10-14: a quarter of 1280 is 320, a burst concentrated on one
// worker plateaus at ~256 queued, and the coalescer holds the rest of the
// backlog in a map the depth could not see — so the clear condition was true
// for the entire 35-minute drain. Every freed slot opened an episode and the
// next tick closed it: 53 lines in three minutes, each "cleared" line reporting
// 253 events still queued. Both halves of the rule matter. "Nothing left to
// dispatch" is the stable half — no further dispatch can block until new events
// arrive, so it cannot flap — and "no wait since the last tick" is what keeps a
// provider slower than the writer inside one episode while the backlog drains.

// retryPolicy bounds the per-op backoff loop.
type retryPolicy struct {
	max  int           // total attempts before giving up
	base time.Duration // first backoff
	cap  time.Duration // ceiling for the geometric growth
}

var defaultRetry = retryPolicy{max: 5, base: 500 * time.Millisecond, cap: 30 * time.Second}

// Placeholders is the OPTIONAL lazy-hydration guard (M5), satisfied by
// *hydrate.Hydrator. The uploader consults it before pushing content: an
// unhydrated placeholder holds no bytes, so uploading it would replace real
// remote content with zeros. Nil means eager mode, where every local file is
// assumed to hold its content (M1–M4 behaviour).
type Placeholders interface {
	// IsPlaceholder reports whether the backing file at rel lacks its content.
	// Implementations must fail safe by reporting true when unsure.
	IsPlaceholder(rel string) bool
}

// Engine applies local change events to a store.
type Engine struct {
	store   provider.Store // nil => log-only mode
	dataDir string         // underlying dir; content is read from here
	state   *state.Store   // echo-suppression store; nil => don't record
	holes   Placeholders   // nil => eager mode; see Placeholders

	lg           *log.Logger
	debounce     time.Duration
	maxWait      time.Duration
	workers      int
	drainTimeout time.Duration
	retry        retryPolicy

	// satSample is the saturation sampler's period. Not a Config field and not a
	// flag: M25's first rule is that drivel has no verbosity control of its own,
	// and a knob for how often an internal state machine looks at itself would be
	// one in all but name. Tests shorten it.
	satSample time.Duration
}

// logf writes one line for this engine.
func (e *Engine) logf(format string, args ...any) {
	if e.lg == nil {
		log.Printf(format, args...)
		return
	}
	e.lg.Printf(format, args...)
}

// Config parameterises an Engine. The tuning fields are optional; zero values fall
// back to the package defaults.
type Config struct {
	Store   provider.Store // nil for log-only mode
	DataDir string         // underlying directory (source of truth)
	State   *state.Store   // engine-level sync state; nil to skip echo recording
	Holes   Placeholders   // lazy-hydration guard (M5); nil for eager mode

	// Logger is where this engine writes; nil uses the log package's default.
	// Per engine because one process may run several mounts, and a sync line that
	// does not say which mount it belongs to is close to useless (M8).
	Logger *log.Logger

	Debounce     time.Duration // per-path coalescing window; see DefaultPushDelay
	Workers      int           // size of the path-hashed worker pool
	DrainTimeout time.Duration // bound on the shutdown drain

	// MaxWait bounds how long repeated activity may hold a path pending, counted
	// from the change that made it pending rather than from the last one. Unset
	// derives it from Debounce (maxWaitFactor); it is separate only so a test can
	// pin both ends of the window independently. A value below Debounce would
	// defeat the coalescing entirely and is raised to it.
	MaxWait time.Duration
}

// New constructs an Engine, applying defaults for any unset tuning fields.
func New(cfg Config) *Engine {
	e := &Engine{
		store:        cfg.Store,
		dataDir:      cfg.DataDir,
		state:        cfg.State,
		holes:        cfg.Holes,
		lg:           cfg.Logger,
		debounce:     cfg.Debounce,
		maxWait:      cfg.MaxWait,
		workers:      cfg.Workers,
		drainTimeout: cfg.DrainTimeout,
		retry:        defaultRetry,
		satSample:    defaultSatSample,
	}
	if e.debounce <= 0 {
		e.debounce = DefaultPushDelay
	}
	// After the debounce default, since it is derived from it. Clamped upward so
	// that a caller who sets MaxWait below Debounce gets a window that still
	// coalesces rather than one that dispatches on the first event.
	if e.maxWait <= 0 {
		e.maxWait = e.debounce * maxWaitFactor
	}
	if e.maxWait < e.debounce {
		e.maxWait = e.debounce
	}
	if e.workers <= 0 {
		e.workers = DefaultWorkers
	}
	if e.drainTimeout <= 0 {
		e.drainTimeout = defaultDrainTimeout
	}
	return e
}

// Workers reports the size of the upload pool this Engine is running, after
// Config's defaulting. A read-only view of a value fixed at construction: it
// exists so the wiring above can be tested end to end, and so a future status
// endpoint can report the number without reaching into Run's goroutines.
func (e *Engine) Workers() int { return e.workers }

// Run consumes events until events is closed or ctx is cancelled. On either it
// drains pending (debounced) and in-flight uploads under drainTimeout before
// returning, so a clean unmount does not lose buffered writes (DESIGN.md §7).
func (e *Engine) Run(ctx context.Context, events <-chan fsevent.Event) {
	if e.store == nil {
		e.runLogOnly(ctx, events)
		return
	}

	// Worker pool. Store ops run under opCtx, NOT the caller's ctx, so a Ctrl-C
	// (which cancels ctx) still lets the drain finish in-flight uploads; opCtx is
	// cancelled only once draining completes or the drain deadline elapses.
	opCtx, opCancel := context.WithCancel(context.Background())
	in := make([]chan fsevent.Event, e.workers)
	var wg sync.WaitGroup
	for i := range in {
		in[i] = make(chan fsevent.Event, workerQueue)
		wg.Add(1)
		go func(ch <-chan fsevent.Event) {
			defer wg.Done()
			for ev := range ch {
				e.executeWithRetry(opCtx, ev)
			}
		}(in[i])
	}

	// Debounce state: coalesce content writes per path behind a trailing timer.
	// Structural ops flush the affected path(s) first so per-path order is kept.
	c := coalescer{pending: map[string]*ranges.Set{}}
	timers := map[string]*time.Timer{}
	flushCh := make(chan string, 64)
	// since[p] is when p's oldest un-dispatched content change arrived. The
	// trailing timer is re-armed by every later change, so this is the only thing
	// that bounds how long one path can be held: see maxWaitFactor.
	since := map[string]time.Time{}
	stopTimer := func(p string) {
		if t, ok := timers[p]; ok {
			t.Stop()
			delete(timers, p)
		}
	}
	// settle forgets everything that was holding p pending. It must run whenever p
	// is dispatched — including when the coalescer turns out to have nothing for
	// it — because a stale `since` entry would put the next change to that path
	// past its deadline the moment it arrived.
	settle := func(p string) {
		stopTimer(p)
		delete(since, p)
	}

	// Back-pressure reporting (M27). bp is touched only by this goroutine — from
	// dispatch, which this goroutine calls, and from the sampler case below — so
	// it needs no lock.
	bp := &backpressure{
		e:     e,
		bound: cap(events) + e.workers*workerQueue,
		depth: func() (queued, undispatched int) {
			for _, ch := range in {
				queued += len(ch)
			}
			// What the coalescer has yet to hand over. A burst does not sit in
			// the worker queues — it sits HERE, in the debounce map, which no
			// channel length can see: 1100 files reported 254 of 1280 queued
			// while 846 of them were still waiting for a worker (MC-12, live).
			return queued, len(events) + len(c.pending)
		},
	}
	// The sampler ticks only while an episode is open; a nil channel blocks
	// forever, which is what an idle mount should cost.
	var satTick *time.Ticker
	var satC <-chan time.Time
	satStop := func() {
		if satTick != nil {
			satTick.Stop()
			satTick, satC = nil, nil
		}
	}
	defer satStop()

	// dispatch hands an event to its worker, and is the one place that can
	// observe the outbound path running out of room: it is where the send blocks,
	// which is what then stops the coalescer reading events and ultimately blocks
	// vfs.node.emit inside a FUSE handler. A depth sampled anywhere upstream
	// cannot tell a full worker queue from a slow provider, and those two want
	// different advice.
	dispatch := func(ev fsevent.Event) {
		ch := in[workerFor(ev.Path, len(in))]
		select {
		case ch <- ev:
			return
		default:
		}
		if bp.blocked() && satTick == nil {
			satTick = time.NewTicker(e.satSample)
			satC = satTick.C
		}
		ch <- ev
	}

loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case ev, ok := <-events:
			if !ok {
				break loop
			}
			switch ev.Op {
			case fsevent.OpCreate, fsevent.OpWrite:
				c.markContent(ev.Path, ev.Dirty)
				stopTimer(ev.Path)
				p := ev.Path
				now := time.Now()
				if _, pending := since[p]; !pending {
					since[p] = now
				}
				// Trailing delay, except that it may not push the dispatch past
				// the max-wait deadline measured from the first pending change.
				// Negative means the deadline has already passed: fire at once.
				delay := e.debounce
				if left := since[p].Add(e.maxWait).Sub(now); left < delay {
					delay = max(left, 0)
				}
				timers[p] = time.AfterFunc(delay, func() {
					select {
					case flushCh <- p:
					case <-opCtx.Done():
					}
				})
			case fsevent.OpSetattr:
				// Metadata-only; no content upload (M2 parity).
			default:
				for _, t := range c.structural(ev) {
					settle(t.Path)
					dispatch(t)
				}
			}
		case p := <-flushCh:
			settle(p)
			for _, t := range c.flush(p) {
				dispatch(t)
			}
		case <-satC:
			if !bp.sample() {
				satStop()
			}
		}
	}

	// Drain: flush every pending content push, then close worker inputs so the
	// pool finishes its queue, bounded by drainTimeout.
	for _, t := range c.flushAll() {
		dispatch(t)
	}
	for _, t := range timers {
		t.Stop()
	}
	for _, ch := range in {
		close(ch)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(e.drainTimeout):
		e.logf("[sync] drain deadline (%s) exceeded; aborting in-flight uploads", e.drainTimeout)
		opCancel()
		<-done
	}
	opCancel()
	// An episode open at shutdown would otherwise never be closed in the log, and
	// an unterminated "queue full" is exactly the kind of thing that gets read as
	// a hang. The drain above has ended it one way or the other by now.
	bp.end()
}

// backpressure reports, once each way, that the outbound path has begun making
// the filesystem wait — and that it has stopped.
//
// The bound it watches is correct and is not what M27 changes: dropping an event
// loses a file until a sweep, and a larger buffer only defers the stall and
// lengthens the drain at unmount. What was wrong is that a mount could spend two
// hours uploading at the provider's rate with nothing in the log to say the
// filesystem was being held back on purpose, so the only conclusion available to
// the person watching it was that drivel had broken.
//
// Both lines are keyed on the state TRANSITION rather than on a timer. A
// periodic depth report is a status API's job (M14) and would be a worse version
// of one; what a log can say that an API cannot is "this started here, and
// stopped here".
type backpressure struct {
	e     *Engine
	bound int // what the queued half of depth() can reach before everything upstream blocks
	// depth reports the outbound path's two halves: how many events are queued
	// in front of the uploader, and how many the coalescer has not handed over
	// yet (its event channel plus its debounce map).
	depth func() (queued, undispatched int)

	on    bool
	since time.Time
	at    int // queued depth when the episode opened
	waits int // dispatches that had to wait during it
	seen  int // waits as of the previous sample
}

// blocked records that a dispatch is about to wait for room, and reports whether
// that opened a new episode.
func (b *backpressure) blocked() bool {
	b.waits++
	if b.on {
		return false
	}
	queued, undispatched := b.depth()
	b.on, b.since, b.at, b.seen = true, time.Now(), queued, b.waits
	// Not "the queue is full": what filled is ONE worker's inbox, and because
	// paths are hashed to workers the aggregate can be well short of the bound
	// when that happens — measured at 646 of 1280 with four workers. Leading
	// with the consequence is both shorter and true. The second number is the
	// backlog behind it, which is the one a bulk copy makes large.
	b.e.logf("[sync] back-pressure: writes through the mount are now waiting on uploads "+
		"(%d of %d event(s) queued, %d awaiting dispatch; -upload-workers %d, -push-delay %s)",
		b.at, b.bound, undispatched, b.e.workers, b.e.debounce)
	return true
}

// sample reports whether the episode is still open, ending it if the backlog has
// receded far enough to say so.
func (b *backpressure) sample() bool {
	if !b.on {
		return false
	}
	queued, undispatched := b.depth()
	waited := b.waits > b.seen
	b.seen = b.waits
	if undispatched > 0 || waited {
		return true
	}
	// "Cleared" is about the filesystem, not about the uploads: what ended is
	// writes waiting on room, and the queue it still reports drains without
	// holding anything back.
	b.e.logf("[sync] back-pressure: cleared after %s — %d write(s) waited, %d event(s) still queued",
		time.Since(b.since).Round(time.Millisecond), b.waits, queued)
	b.on, b.waits, b.seen = false, 0, 0
	return false
}

// end closes an episode that was still open when the engine stopped.
func (b *backpressure) end() {
	if !b.on {
		return
	}
	b.e.logf("[sync] back-pressure: cleared at shutdown after %s — %d write(s) waited",
		time.Since(b.since).Round(time.Millisecond), b.waits)
	b.on, b.waits = false, 0
}

// runLogOnly is the M1 no-store path: just log each observed mutation.
func (e *Engine) runLogOnly(ctx context.Context, events <-chan fsevent.Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			e.logEvent(ev)
		}
	}
}

// coalescer tracks which paths have a pending (debounced) content push and turns
// incoming events into the ordered list of tasks to dispatch. A "task" is just an
// fsevent.Event; a coalesced content push is normalised to OpWrite (the executor
// re-reads the file from disk, so create-vs-write is irrelevant). It holds no
// timers — the Run loop owns those and calls flush when a timer fires.
//
// A pending entry carries the union of the dirty extents its events reported
// (M6). A nil value means "extents unknown" — push the whole file — and it is
// absorbing: once any contributing event is unknown, the coalesced push is too.
// The map key's presence, not its value, is what marks a path pending.
type coalescer struct {
	pending map[string]*ranges.Set
}

// markContent records that path has buffered content to push once its debounce
// window elapses, folding dirty into whatever extents are already pending.
//
// The merge is deliberately pessimistic in two places. An unknown set poisons the
// accumulator, and so does a union across mismatched block grids: both mean we
// can no longer name every changed byte, and the only safe answer to that is the
// whole file.
func (c *coalescer) markContent(path string, dirty *ranges.Set) {
	cur, pending := c.pending[path]
	switch {
	case !pending:
		if dirty == nil {
			c.pending[path] = nil
			return
		}
		merged := dirty.Clone()
		c.pending[path] = &merged
	case cur == nil || dirty == nil:
		c.pending[path] = nil // unknown absorbs known
	case !cur.Union(*dirty):
		c.pending[path] = nil // incompatible grids; fall back to whole-file
	}
}

// flush returns the coalesced content task for path (if any), clearing it.
func (c *coalescer) flush(path string) []fsevent.Event {
	dirty, ok := c.pending[path]
	if !ok {
		return nil
	}
	delete(c.pending, path)
	return []fsevent.Event{{Op: fsevent.OpWrite, Path: path, Dirty: dirty}}
}

// structural flushes any content pending for the path(s) the op names, then
// appends the op itself — so a "write then rename/delete" on a path stays ordered.
func (c *coalescer) structural(ev fsevent.Event) []fsevent.Event {
	out := c.flush(ev.Path)
	if ev.Op == fsevent.OpRename {
		out = append(out, c.flush(ev.NewPath)...)
	}
	return append(out, ev)
}

// flushAll drains every pending content push (used on shutdown).
func (c *coalescer) flushAll() []fsevent.Event {
	out := make([]fsevent.Event, 0, len(c.pending))
	for p, dirty := range c.pending {
		out = append(out, fsevent.Event{Op: fsevent.OpWrite, Path: p, Dirty: dirty})
	}
	c.pending = map[string]*ranges.Set{}
	return out
}

// workerFor hashes a path to a worker index. Same path → same worker → its ops run
// serially and in order. Renames are dispatched by their source path (ev.Path), so
// a "write then rename" sequence on that path lands on one worker; cross-path
// ordering is not globally guaranteed but the system converges regardless (the
// executor re-reads disk and Move-on-unknown falls back to a fresh upload).
func workerFor(path string, n int) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(path))
	return int(h.Sum32()) % n
}

// executeWithRetry pushes one event, retrying transient failures with exponential
// backoff + full jitter. It stops early if ctx is cancelled (shutdown/drain).
func (e *Engine) executeWithRetry(ctx context.Context, ev fsevent.Event) {
	delay := e.retry.base
	for attempt := 1; ; attempt++ {
		err := e.push(ctx, ev)
		if err == nil {
			return
		}
		if ctx.Err() != nil {
			return // cancelled: give up quietly (drain deadline or shutdown)
		}
		if !provider.IsRetryable(err) || attempt >= e.retry.max {
			// One line, not two: the shape that abandons the most pushes is a
			// non-retryable error, which gives up after a single attempt, so a
			// permission failure over a large tree writes one of these per file.
			e.logf("[sync] push %s %s failed after %d attempt(s): %v%s",
				ev.Op, ev.Path, attempt, err, e.oweRepair(ev))
			return
		}
		e.logf("[sync] push %s %s attempt %d failed, retrying: %v", ev.Op, ev.Path, attempt, err)
		if !sleepCtx(ctx, jitter(delay)) {
			return
		}
		if delay *= 2; delay > e.retry.cap {
			delay = e.retry.cap
		}
	}
}

// oweRepair records that this event was abandoned, so the next sweep re-pushes
// the path instead of the repair waiting for -sweep-interval — 24 hours away on
// Drive, and never at all when it is 0. It returns the clause to append to the
// failure line, so that giving up still costs exactly one line (M27).
//
// Giving up is right: retrying a permission error forever is a spin, and an
// unbounded persistent outbound queue would be a second durability surface over
// the one DESIGN.md §12 already prices. What was wrong is that the knowledge
// went with it — nothing marked the path, so nothing could put it right.
func (e *Engine) oweRepair(ev fsevent.Event) string {
	if e.state == nil {
		return ""
	}
	rel, ok := repairPathOf(ev)
	if !ok {
		return ""
	}
	if err := e.state.OweRepair(rel, time.Now()); err != nil {
		// Worth its own clause: this is the path losing its last chance of being
		// repaired before the next scheduled sweep, which may be a day away.
		return fmt.Sprintf(" (and recording %s for repair failed: %v)", rel, err)
	}
	return " (recorded for repair by the next sweep)"
}

// repairPathOf names the local path whose current contents a later sweep should
// send for an abandoned event, and reports whether there is one at all.
//
// It is the path and not the operation on purpose. The record is not a queued
// request to replay — the sweep re-derives what to send from the backing file as
// it stands then, which is the only honest answer once an arbitrary amount of
// time has passed.
func repairPathOf(ev fsevent.Event) (string, bool) {
	switch ev.Op {
	case fsevent.OpRename:
		// The bytes are at the destination; the source no longer exists locally,
		// so a re-push of it would find nothing. Re-sending the file under its new
		// name is what a Move was going to achieve anyway.
		return ev.NewPath, ev.NewPath != ""
	case fsevent.OpUnlink, fsevent.OpRmdir:
		// Nothing local to send. An abandoned deletion is therefore NOT repaired
		// by this record, and M27 does not close that case: the remote object is
		// still there and still marked seen by the sweep, so the delete pass never
		// considers it. Undoing it needs the deletion to go back through
		// -max-deletes, which is the delete pass's own machinery rather than a
		// re-push — see DESIGN.md §9, M27.
		return "", false
	case fsevent.OpSetattr:
		return "", false // metadata-only; push does nothing for it, so it cannot fail
	}
	return ev.Path, ev.Path != ""
}

// handle applies one event synchronously (log-only mode logs; otherwise push with
// retry). Retained for direct/unit use; the Run loop drives events through the
// debouncer + worker pool instead.
func (e *Engine) handle(ctx context.Context, ev fsevent.Event) {
	if e.store == nil {
		e.logEvent(ev)
		return
	}
	e.executeWithRetry(ctx, ev)
}

// Push applies one event immediately, with the same retry policy Run uses. It is
// the seam the M7b reconciler pushes through (syncengine.Pusher) so that a file
// discovered by a sweep takes exactly the path a file written through the mount
// takes — placeholder guard, M6 gates, echo recording, backoff — instead of a
// second, subtly different implementation of "upload this".
//
// It bypasses only the debounce window, which exists to coalesce bursts; a
// reconcile produces one event per path by construction.
func (e *Engine) Push(ctx context.Context, ev fsevent.Event) { e.handle(ctx, ev) }

// push maps one event to store calls. The store creates ancestor directories and
// resolves paths to its native addressing itself, so this stays path-only.
//
// Every branch that reaches the remote says so, in the same shape the pull side
// uses ("[pull] download", "[pull] delete"). Until M0's multi-client campaign
// went looking for the numbers, a successful push logged *nothing* — the only
// "[sync] push" lines in a log were failures — so from the outside a working
// upload and an upload that never happened were the same silence. That is a bad
// property for a filesystem whose whole job is to move bytes somewhere else, and
// it made the transfer-volume half of docs/dev/multiclient-test-plan.md §2.7
// unmeasurable.
func (e *Engine) push(ctx context.Context, ev fsevent.Event) error {
	switch ev.Op {
	case fsevent.OpMkdir:
		rf, err := e.store.Mkdir(ctx, ev.Path)
		if err != nil {
			return err
		}
		e.recordEcho(rf)
		e.logf("[sync] mkdir    %s", ev.Path)
		return nil

	case fsevent.OpCreate, fsevent.OpWrite:
		return e.pushContent(ctx, ev.Path, ev.Dirty)

	case fsevent.OpUnlink, fsevent.OpRmdir:
		if err := e.store.Remove(ctx, ev.Path); err != nil {
			return err
		}
		e.forgetPath(ev.Path)
		e.logf("[sync] delete   %s", ev.Path)
		return nil

	case fsevent.OpRename:
		rf, err := e.store.Move(ctx, ev.Path, ev.NewPath)
		if errors.Is(err, provider.ErrNotExist) {
			// Source was never pushed; treat the destination as new content — whole
			// file, since no handle tracked which of its bytes are new.
			return e.pushContent(ctx, ev.NewPath, nil)
		}
		if err != nil {
			return err
		}
		e.forgetPath(ev.Path)
		// The bytes did not move; the name did. Fingerprint the file at its new
		// local path so the baseline still describes something that exists.
		fi, _ := os.Stat(filepath.Join(e.dataDir, filepath.FromSlash(ev.NewPath)))
		e.recordEchoOf(rf, fi)
		e.logf("[sync] rename   %s -> %s", ev.Path, ev.NewPath)
		return nil

	case fsevent.OpSetattr:
		// Metadata-only change; no content push.
	}
	return nil
}

// hashSkipMinSize is the size below which the unchanged-content gate is not worth
// running. Both gates below cost a Stat round-trip, and for a small file that is
// the same order as just uploading it — the gate would spend a request to save a
// request. The threshold buys back the M1–M5 behaviour for small files (always
// upload) and reserves the checks for payloads where a wasted transfer actually
// hurts. One block keeps it coherent with the range granularity.
const hashSkipMinSize = ranges.DefaultBlockSize

// pushContent uploads the file at virtual path p from the underlying dir, taking
// the cheapest route that is certainly correct. dirty bounds the byte extents the
// mount observed changing; nil means "unknown", which is always safe.
//
// The placeholder check comes first and nothing may get in front of it. It is
// load-bearing, not an optimization (DESIGN.md §9, M5): a lazily-hydrated file
// that has never been read holds zero resident bytes at its full apparent size,
// so pushing it would overwrite the remote content it stands for with zeros.
// Placeholders have no local edits by definition — the FUSE layer hydrates before
// any write — so skipping is the correct answer, not a deferral.
//
// After that, pushShortcut may finish the push by a cheaper route. Anything it
// declines falls through to a whole-file Put, which is the M1–M5 behaviour and is
// never wrong — only slower.
func (e *Engine) pushContent(ctx context.Context, p string, dirty *ranges.Set) error {
	if e.holes != nil && e.holes.IsPlaceholder(p) {
		e.logf("[sync] skip %s: unhydrated placeholder (nothing local to push)", p)
		return nil
	}
	f, err := os.Open(filepath.Join(e.dataDir, filepath.FromSlash(p)))
	if err != nil {
		if os.IsNotExist(err) {
			return nil // raced with a delete; nothing to push
		}
		return err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return err
	}
	size := fi.Size()

	done, err := e.pushShortcut(ctx, p, f, size, dirty)
	if err != nil {
		return err
	}
	if done {
		return nil
	}

	// Whole file. Rewind: the gates above may have read from f.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	// Counted rather than taken from the Stat above, because the two disagree
	// exactly when the number matters. A file still being written grows during its
	// own push — Put streams to EOF, so the whole of it goes across — and the
	// pre-transfer size then under-reports the transfer by however much arrived in
	// the meantime: a 512 MiB copy was logged as 372 MiB (MC-40). This line is the
	// instrument the campaign measures transfer volume with
	// (docs/dev/multiclient-test-plan.md §2.7), so it reports what was sent.
	counted := &countingReader{r: f}
	rf, err := e.store.Put(ctx, p, counted)
	if err != nil {
		return err
	}
	e.recordEchoOf(rf, statOf(f))
	// Only here, never on the routes above: a range write and both skips log their
	// own lines, and counting this one as well would report an upload that did not
	// happen — which is exactly what MC-21 (a touch must cost nothing) asserts.
	e.logf("[sync] upload   %s (%d B)", p, counted.n)
	return nil
}

// countingReader reports how many bytes a store actually read out of a file. It
// is not a progress meter: nothing reads n until the Put has returned.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// pushShortcut tries the two M6 routes that avoid re-uploading a whole file,
// reporting done=true if one of them completed the push. Declining is always an
// option: every path here has the whole-file Put behind it.
//
// Both routes need to know the remote's current state, so they share one Stat —
// and both are skipped outright for a file too small for that round-trip to pay
// for itself.
//
// The range write is tried BEFORE the unchanged-content hash, which looks
// backwards for a "cheapest first" ordering and is deliberate: hashing means
// reading the entire file, so on the case M6 exists for — one block changed in a
// multi-gigabyte file — checking "did anything change?" first would cost a 4 GB
// read to avoid a 4 MiB upload. If the content turns out not to have really
// changed, the range write rewrites identical bytes, which is wasteful but not
// wrong.
func (e *Engine) pushShortcut(ctx context.Context, p string, f *os.File, size int64, dirty *ranges.Set) (bool, error) {
	rp, canPatch := provider.AsRangePutter(e.store)
	patchable := canPatch && rangeWorthIt(dirty, size)
	hashable := size >= hashSkipMinSize
	if !patchable && !hashable {
		return false, nil
	}

	remote, exists, err := e.store.Stat(ctx, p)
	if err != nil {
		e.logf("[sync] stat %s: %v (uploading whole file)", p, err)
		return false, nil
	}
	if !exists {
		return false, nil // nothing to patch or compare against; this is a create
	}

	if patchable && remote.Size == size && e.remoteIsOurs(p, remote) {
		extents := dirty.Extents()
		rf, err := rp.PutRange(ctx, p, f, size, extents)
		if err != nil {
			e.logf("[sync] range write %s failed (%d extent(s), %d of %d B): %v (uploading whole file)",
				p, len(extents), dirty.Bytes(), size, err)
		} else {
			e.logf("[sync] range write %s: %d extent(s), %d of %d B", p, len(extents), dirty.Bytes(), size)
			e.recordEchoOf(rf, statOf(f))
			return true, nil
		}
	}

	if hashable {
		unchanged, err := e.contentMatches(f, remote)
		if err != nil {
			return false, err
		}
		if unchanged {
			e.logf("[sync] skip %s: remote already holds these bytes (%d B not uploaded)", p, size)
			return true, nil
		}
	}
	return false, nil
}

// rangeWorthIt reports whether dirty is a usable, profitable description of the
// changes to a file of this size.
//
// The size equality is the load-bearing half. A set built against a different
// length describes a file nobody is holding — a truncate or a racing writer moved
// every offset after the cut — and patching from it would land bytes in the wrong
// place. Everything else here is economics: nothing marked, or everything marked,
// means a patch would send as much as a Put.
func rangeWorthIt(dirty *ranges.Set, size int64) bool {
	if dirty == nil || dirty.Size != size {
		return false
	}
	return len(dirty.Extents()) > 0 && dirty.Bytes() < size
}

// remoteIsOurs reports whether the remote object is still exactly the content we
// last synced at p, per the §4 echo record.
//
// This is the check that makes a partial write safe. A whole-file Put over a
// remote someone else has edited loses their edit, which is the documented §6
// last-writer-wins policy and is at least recoverable — the loser's bytes existed
// as one coherent version. Splicing our extents into their file produces a hybrid
// that never existed anywhere, silently, with no conflict copy, and no version of
// the file left intact. So a range write may only ever be applied to the exact
// version we based it on; anything else — a divergent remote, no echo record at
// all, no state store — falls back to the whole-file path and its normal conflict
// semantics.
func (e *Engine) remoteIsOurs(p string, remote provider.RemoteFile) bool {
	if e.state == nil {
		return false
	}
	echo, ok, err := e.state.GetEcho(p)
	if err != nil || !ok {
		return false
	}
	return echo.Matches(remote.Hash, remote.Version)
}

// contentMatches reports whether the remote already holds exactly the bytes in f,
// by hashing the local content with the provider's own digest and comparing
// against what Stat just reported.
//
// It compares against the REMOTE's hash, not the echo record's. The echo says
// what we last synced, which is a claim about the past: if the remote has since
// diverged in a way the change feed never delivered (a cursor expired across a
// long downtime, a state DB reused against a different -drive-root, a delete we
// never learned about), a stale echo would match our unchanged local file forever
// and the push would be skipped every time — the file would silently never be
// restored. Asking the remote what it currently holds cannot go stale.
//
// It is best-effort in one direction only: any doubt (no ContentHasher, no remote
// checksum, a read error) reports false and the push proceeds. The cost of a
// wrong "true" is a lost local change, so nothing but a positive hash match may
// produce one.
func (e *Engine) contentMatches(f *os.File, remote provider.RemoteFile) (bool, error) {
	hasher, ok := provider.AsContentHasher(e.store)
	if !ok || remote.Hash == "" {
		return false, nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	local, err := hasher.HashContent(f)
	if err != nil {
		e.logf("[sync] hash %s: %v (uploading anyway)", remote.Path, err)
		return false, nil
	}
	return local != "" && local == remote.Hash, nil
}

// statOf reports f's current metadata, or nil if it cannot be read. A failed stat
// costs a fingerprint, never correctness: no fingerprint reads as "not recorded".
func statOf(f *os.File) os.FileInfo {
	fi, err := f.Stat()
	if err != nil {
		return nil
	}
	return fi
}

// recordEcho notes the content we just pushed at rf.Path so the change-feed
// report of this same write is recognised as our echo and dropped (DESIGN.md §4).
func (e *Engine) recordEcho(rf provider.RemoteFile) {
	e.recordEchoOf(rf, nil)
}

// recordEchoOf is recordEcho with the local file we just pushed, whose size and
// mtime become the baseline's local fingerprint (see state.Echo).
//
// fi may be nil — for a Mkdir, which has no content, and for any caller that does
// not have the file open. That records no fingerprint, which is the safe reading:
// M7b then declines to conclude that the local copy is unmodified, and keeps it.
//
// It is deliberately taken *after* the push rather than before. A write that
// landed while the upload was in flight has already queued another event, so the
// next push re-records this baseline against the newer bytes; recording the
// pre-push state instead would leave a fingerprint that matches nothing on disk.
func (e *Engine) recordEchoOf(rf provider.RemoteFile, fi os.FileInfo) {
	if e.state == nil {
		return
	}
	size, mtime := state.FingerprintOf(fi)
	echo := state.Echo{Hash: rf.Hash, Version: rf.Version, At: time.Now(), LocalSize: size, LocalMTime: mtime}
	if err := e.state.SetEcho(rf.Path, echo); err != nil {
		e.logf("[sync] record echo %s: %v", rf.Path, err)
	}
}

// forgetPath drops every state record for p, in one commit.
//
// It runs when the remote object at p is gone (removed) or has moved away
// (renamed), which makes both records meaningless at once: the §4 echo describes
// content that is no longer there, and the M5 hydration bitmap describes a file
// that no longer exists. Dropping only the echo — which is what this did before —
// leaked one hydration record per deleted path for the life of the state DB, and
// in lazy mode that is every file, since a completed hydration writes a full
// bitmap and nothing ever removes it.
func (e *Engine) forgetPath(p string) {
	if e.state == nil {
		return
	}
	if err := e.state.Forget(p); err != nil {
		e.logf("[sync] forget state %s: %v", p, err)
	}
}

func (e *Engine) logEvent(ev fsevent.Event) {
	if ev.Op == fsevent.OpRename {
		e.logf("[sync] %-7s %s -> %s", ev.Op, ev.Path, ev.NewPath)
		return
	}
	e.logf("[sync] %-7s %s%s", ev.Op, ev.Path, describeDirty(ev.Dirty))
}

// describeDirty renders an event's dirty extents for the log. Log-only mode is
// where a developer watches what the mount actually reports, so it is worth
// seeing whether a write came through as extents or as "whole file" (M6).
func describeDirty(d *ranges.Set) string {
	if d == nil {
		return ""
	}
	extents := d.Extents()
	if len(extents) == 0 {
		return ""
	}
	return fmt.Sprintf(" (%d extent(s), %d of %d B)", len(extents), d.Bytes(), d.Size)
}

// jitter returns a duration in [d/2, d) — full jitter around the backoff, so many
// workers retrying a shared outage don't resynchronise into thundering herds.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	// math/rand is correct here: this decorrelates retry timing, it does not
	// protect anything. crypto/rand would add a syscall per retry for nothing.
	return d/2 + time.Duration(rand.Int63n(int64(d)/2+1)) //nolint:gosec // G404: see above
}

// sleepCtx sleeps for d or until ctx is cancelled; it reports false if cancelled.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
