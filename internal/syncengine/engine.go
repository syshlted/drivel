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
	"hash/fnv"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/zishmusic/drivel/internal/fsevent"
	"github.com/zishmusic/drivel/internal/provider"
	"github.com/zishmusic/drivel/internal/state"
)

// Defaults for the outbound uploader. All are overridable via Config.
const (
	defaultDebounce     = 300 * time.Millisecond
	defaultWorkers      = 4
	defaultDrainTimeout = 30 * time.Second
)

// retryPolicy bounds the per-op backoff loop.
type retryPolicy struct {
	max  int           // total attempts before giving up
	base time.Duration // first backoff
	cap  time.Duration // ceiling for the geometric growth
}

var defaultRetry = retryPolicy{max: 5, base: 500 * time.Millisecond, cap: 30 * time.Second}

// Engine applies local change events to a store.
type Engine struct {
	store   provider.Store // nil => log-only mode
	dataDir string         // underlying dir; content is read from here
	state   *state.Store   // echo-suppression store; nil => don't record

	debounce     time.Duration
	workers      int
	drainTimeout time.Duration
	retry        retryPolicy
}

// Config parameterises an Engine. The tuning fields are optional; zero values fall
// back to the package defaults.
type Config struct {
	Store   provider.Store // nil for log-only mode
	DataDir string         // underlying directory (source of truth)
	State   *state.Store   // engine-level sync state; nil to skip echo recording

	Debounce     time.Duration // per-path coalescing window
	Workers      int           // size of the path-hashed worker pool
	DrainTimeout time.Duration // bound on the shutdown drain
}

// New constructs an Engine, applying defaults for any unset tuning fields.
func New(cfg Config) *Engine {
	e := &Engine{
		store:        cfg.Store,
		dataDir:      cfg.DataDir,
		state:        cfg.State,
		debounce:     cfg.Debounce,
		workers:      cfg.Workers,
		drainTimeout: cfg.DrainTimeout,
		retry:        defaultRetry,
	}
	if e.debounce <= 0 {
		e.debounce = defaultDebounce
	}
	if e.workers <= 0 {
		e.workers = defaultWorkers
	}
	if e.drainTimeout <= 0 {
		e.drainTimeout = defaultDrainTimeout
	}
	return e
}

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
		in[i] = make(chan fsevent.Event, 64)
		wg.Add(1)
		go func(ch <-chan fsevent.Event) {
			defer wg.Done()
			for ev := range ch {
				e.executeWithRetry(opCtx, ev)
			}
		}(in[i])
	}
	dispatch := func(ev fsevent.Event) { in[workerFor(ev.Path, len(in))] <- ev }

	// Debounce state: coalesce content writes per path behind a trailing timer.
	// Structural ops flush the affected path(s) first so per-path order is kept.
	c := coalescer{pending: map[string]struct{}{}}
	timers := map[string]*time.Timer{}
	flushCh := make(chan string, 64)
	stopTimer := func(p string) {
		if t, ok := timers[p]; ok {
			t.Stop()
			delete(timers, p)
		}
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
				c.markContent(ev.Path)
				stopTimer(ev.Path)
				p := ev.Path
				timers[p] = time.AfterFunc(e.debounce, func() {
					select {
					case flushCh <- p:
					case <-opCtx.Done():
					}
				})
			case fsevent.OpSetattr:
				// Metadata-only; no content upload (M2 parity).
			default:
				for _, t := range c.structural(ev) {
					stopTimer(t.Path)
					dispatch(t)
				}
			}
		case p := <-flushCh:
			for _, t := range c.flush(p) {
				stopTimer(p)
				dispatch(t)
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
		log.Printf("[sync] drain deadline (%s) exceeded; aborting in-flight uploads", e.drainTimeout)
		opCancel()
		<-done
	}
	opCancel()
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
			logEvent(ev)
		}
	}
}

// coalescer tracks which paths have a pending (debounced) content push and turns
// incoming events into the ordered list of tasks to dispatch. A "task" is just an
// fsevent.Event; a coalesced content push is normalised to OpWrite (the executor
// re-reads the file from disk, so create-vs-write is irrelevant). It holds no
// timers — the Run loop owns those and calls flush when a timer fires.
type coalescer struct {
	pending map[string]struct{}
}

// markContent records that path has buffered content to push once its debounce
// window elapses.
func (c *coalescer) markContent(path string) { c.pending[path] = struct{}{} }

// flush returns the coalesced content task for path (if any), clearing it.
func (c *coalescer) flush(path string) []fsevent.Event {
	if _, ok := c.pending[path]; !ok {
		return nil
	}
	delete(c.pending, path)
	return []fsevent.Event{{Op: fsevent.OpWrite, Path: path}}
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
	for p := range c.pending {
		out = append(out, fsevent.Event{Op: fsevent.OpWrite, Path: p})
	}
	c.pending = map[string]struct{}{}
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
			log.Printf("[sync] push %s %s failed after %d attempt(s): %v", ev.Op, ev.Path, attempt, err)
			return
		}
		log.Printf("[sync] push %s %s attempt %d failed, retrying: %v", ev.Op, ev.Path, attempt, err)
		if !sleepCtx(ctx, jitter(delay)) {
			return
		}
		if delay *= 2; delay > e.retry.cap {
			delay = e.retry.cap
		}
	}
}

// handle applies one event synchronously (log-only mode logs; otherwise push with
// retry). Retained for direct/unit use; the Run loop drives events through the
// debouncer + worker pool instead.
func (e *Engine) handle(ctx context.Context, ev fsevent.Event) {
	if e.store == nil {
		logEvent(ev)
		return
	}
	e.executeWithRetry(ctx, ev)
}

// push maps one event to store calls. The store creates ancestor directories and
// resolves paths to its native addressing itself, so this stays path-only.
func (e *Engine) push(ctx context.Context, ev fsevent.Event) error {
	switch ev.Op {
	case fsevent.OpMkdir:
		rf, err := e.store.Mkdir(ctx, ev.Path)
		if err != nil {
			return err
		}
		e.recordEcho(rf)
		return nil

	case fsevent.OpCreate, fsevent.OpWrite:
		return e.pushContent(ctx, ev.Path)

	case fsevent.OpUnlink, fsevent.OpRmdir:
		if err := e.store.Remove(ctx, ev.Path); err != nil {
			return err
		}
		e.forgetEcho(ev.Path)
		return nil

	case fsevent.OpRename:
		rf, err := e.store.Move(ctx, ev.Path, ev.NewPath)
		if errors.Is(err, provider.ErrNotExist) {
			// Source was never pushed; treat the destination as new content.
			return e.pushContent(ctx, ev.NewPath)
		}
		if err != nil {
			return err
		}
		e.forgetEcho(ev.Path)
		e.recordEcho(rf)
		return nil

	case fsevent.OpSetattr:
		// Metadata-only change; no content push.
	}
	return nil
}

// pushContent uploads (or replaces) the file at virtual path p from the
// underlying dir. Put decides create-vs-replace, so the engine doesn't track IDs.
func (e *Engine) pushContent(ctx context.Context, p string) error {
	f, err := os.Open(filepath.Join(e.dataDir, filepath.FromSlash(p)))
	if err != nil {
		if os.IsNotExist(err) {
			return nil // raced with a delete; nothing to push
		}
		return err
	}
	defer f.Close()

	rf, err := e.store.Put(ctx, p, f)
	if err != nil {
		return err
	}
	e.recordEcho(rf)
	return nil
}

// recordEcho notes the content we just pushed at rf.Path so the change-feed
// report of this same write is recognised as our echo and dropped (DESIGN.md §4).
func (e *Engine) recordEcho(rf provider.RemoteFile) {
	if e.state == nil {
		return
	}
	if err := e.state.SetEcho(rf.Path, state.Echo{Hash: rf.Hash, Version: rf.Version, At: time.Now()}); err != nil {
		log.Printf("[sync] record echo %s: %v", rf.Path, err)
	}
}

func (e *Engine) forgetEcho(p string) {
	if e.state == nil {
		return
	}
	if err := e.state.DeleteEcho(p); err != nil {
		log.Printf("[sync] forget echo %s: %v", p, err)
	}
}

func logEvent(ev fsevent.Event) {
	if ev.Op == fsevent.OpRename {
		log.Printf("[sync] %-7s %s -> %s", ev.Op, ev.Path, ev.NewPath)
	} else {
		log.Printf("[sync] %-7s %s", ev.Op, ev.Path)
	}
}

// jitter returns a duration in [d/2, d) — full jitter around the backoff, so many
// workers retrying a shared outage don't resynchronise into thundering herds.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return d/2 + time.Duration(rand.Int63n(int64(d)/2+1))
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
