package syncengine

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/zishmusic/drivel/internal/fsevent"
	"github.com/zishmusic/drivel/provider"
	"github.com/zishmusic/drivel/ranges"
)

// --- coalescer (pure debounce logic, no timers) -----------------------------

func opPaths(evs []fsevent.Event) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = string(e.Op) + ":" + e.Path
		if e.Op == fsevent.OpRename {
			out[i] += "->" + e.NewPath
		}
	}
	return out
}

func TestCoalescerCoalescesContentWrites(t *testing.T) {
	c := coalescer{pending: map[string]*ranges.Set{}}
	c.markContent("a.txt", nil)
	c.markContent("a.txt", nil)
	c.markContent("a.txt", nil)

	// A burst collapses to a single OpWrite task on flush.
	got := opPaths(c.flush("a.txt"))
	if len(got) != 1 || got[0] != "write:a.txt" {
		t.Fatalf("flush = %v; want one write:a.txt", got)
	}
	// Nothing left pending afterwards.
	if got := c.flush("a.txt"); got != nil {
		t.Fatalf("second flush = %v; want nil", opPaths(got))
	}
}

func TestCoalescerStructuralFlushesPendingFirst(t *testing.T) {
	c := coalescer{pending: map[string]*ranges.Set{}}

	// write then rename on the same path: the pending write must be dispatched
	// before the rename so per-path order is preserved.
	c.markContent("a.txt", nil)
	got := opPaths(c.structural(fsevent.Event{Op: fsevent.OpRename, Path: "a.txt", NewPath: "b.txt"}))
	want := []string{"write:a.txt", "rename:a.txt->b.txt"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("structural(rename) = %v; want %v", got, want)
	}

	// unlink with no pending content is dispatched alone.
	got = opPaths(c.structural(fsevent.Event{Op: fsevent.OpUnlink, Path: "c.txt"}))
	if len(got) != 1 || got[0] != "unlink:c.txt" {
		t.Fatalf("structural(unlink) = %v; want [unlink:c.txt]", got)
	}
}

func TestCoalescerFlushAll(t *testing.T) {
	c := coalescer{pending: map[string]*ranges.Set{}}
	c.markContent("a.txt", nil)
	c.markContent("b.txt", nil)
	if got := c.flushAll(); len(got) != 2 {
		t.Fatalf("flushAll returned %d tasks; want 2", len(got))
	}
	if len(c.pending) != 0 {
		t.Fatalf("pending not cleared after flushAll: %v", c.pending)
	}
}

// --- retry / backoff --------------------------------------------------------

// testErr carries a Retryable() verdict so provider.IsRetryable can classify it
// without any provider package involved.
type testErr struct {
	msg   string
	retry bool
}

func (e testErr) Error() string   { return e.msg }
func (e testErr) Retryable() bool { return e.retry }

// retryStore fails the first failN Put calls with failErr, then succeeds. Only
// Put is exercised by these tests; the rest satisfy provider.Store trivially.
type retryStore struct {
	mu       sync.Mutex
	putCalls int
	failN    int
	failErr  error
}

func (s *retryStore) Put(_ context.Context, p string, r io.Reader) (provider.RemoteFile, error) {
	_, _ = io.Copy(io.Discard, r)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.putCalls++
	if s.putCalls <= s.failN {
		return provider.RemoteFile{}, s.failErr
	}
	return provider.RemoteFile{Path: p}, nil
}

func (s *retryStore) calls() int { s.mu.Lock(); defer s.mu.Unlock(); return s.putCalls }

func (s *retryStore) Mkdir(context.Context, string) (provider.RemoteFile, error) {
	return provider.RemoteFile{}, nil
}
func (s *retryStore) Move(context.Context, string, string) (provider.RemoteFile, error) {
	return provider.RemoteFile{}, nil
}
func (s *retryStore) Remove(context.Context, string) error { return nil }
func (s *retryStore) Get(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(nil), nil
}
func (s *retryStore) Stat(context.Context, string) (provider.RemoteFile, bool, error) {
	return provider.RemoteFile{}, false, nil
}

func fastRetry(e *Engine) {
	e.retry = retryPolicy{max: 5, base: time.Microsecond, cap: 100 * time.Microsecond}
}

func TestRetryTransientThenSucceeds(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "hello")
	store := &retryStore{failN: 2, failErr: testErr{msg: "flaky", retry: true}}
	e := New(Config{Store: store, DataDir: dir})
	fastRetry(e)

	e.executeWithRetry(context.Background(), fsevent.Event{Op: fsevent.OpWrite, Path: "a.txt"})

	if got := store.calls(); got != 3 {
		t.Fatalf("Put called %d times; want 3 (2 transient failures + success)", got)
	}
}

func TestRetryPermanentGivesUpImmediately(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "hello")
	store := &retryStore{failN: 99, failErr: testErr{msg: "forbidden", retry: false}}
	e := New(Config{Store: store, DataDir: dir})
	fastRetry(e)

	e.executeWithRetry(context.Background(), fsevent.Event{Op: fsevent.OpWrite, Path: "a.txt"})

	if got := store.calls(); got != 1 {
		t.Fatalf("Put called %d times; want 1 (permanent error, no retry)", got)
	}
}

func TestRetryStopsOnCancelledContext(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "hello")
	store := &retryStore{failN: 99, failErr: testErr{msg: "flaky", retry: true}}
	e := New(Config{Store: store, DataDir: dir})
	e.retry = retryPolicy{max: 100, base: 50 * time.Millisecond, cap: time.Second}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: the first backoff sleep must abort the loop
	e.executeWithRetry(ctx, fsevent.Event{Op: fsevent.OpWrite, Path: "a.txt"})

	if got := store.calls(); got != 1 {
		t.Fatalf("Put called %d times; want 1 (cancelled before retrying)", got)
	}
}

// --- Run: end-to-end debounce + drain on shutdown ---------------------------

// A burst of writes to one path, then a distinct file, all coalesce and are
// drained when events closes — exactly one upload per path.
func TestRunCoalescesAndDrainsOnClose(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "hello")
	writeFile(t, dir, "b.txt", "world")
	fs := newFakeStore()
	e := New(Config{Store: fs, DataDir: dir, Debounce: 5 * time.Millisecond})

	events := make(chan fsevent.Event, 16)
	events <- fsevent.Event{Op: fsevent.OpCreate, Path: "a.txt"}
	events <- fsevent.Event{Op: fsevent.OpWrite, Path: "a.txt"}
	events <- fsevent.Event{Op: fsevent.OpWrite, Path: "a.txt"}
	events <- fsevent.Event{Op: fsevent.OpCreate, Path: "b.txt"}
	close(events)

	e.Run(context.Background(), events) // returns only after the drain completes

	fs.mu.Lock()
	defer fs.mu.Unlock()
	puts := map[string]int{}
	for _, c := range fs.calls {
		puts[c]++
	}
	if puts["Put(create,a.txt,bytes=5)"] != 1 {
		t.Fatalf("want exactly one create Put for a.txt, calls=%v", fs.calls)
	}
	if puts["Put(create,b.txt,bytes=5)"] != 1 {
		t.Fatalf("want exactly one create Put for b.txt, calls=%v", fs.calls)
	}
	if len(fs.calls) != 2 {
		t.Fatalf("coalescing failed: got %d store calls, want 2: %v", len(fs.calls), fs.calls)
	}
}

// Pending debounced content that never reached its timer is still flushed and
// uploaded on shutdown (drain-flushes-pending).
func TestRunDrainFlushesPendingWrite(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "late.txt", "content")
	fs := newFakeStore()
	// Long debounce: the timer would never fire within the test; only the drain
	// can flush this write.
	e := New(Config{Store: fs, DataDir: dir, Debounce: time.Hour})

	events := make(chan fsevent.Event, 4)
	events <- fsevent.Event{Op: fsevent.OpWrite, Path: "late.txt"}
	close(events)

	e.Run(context.Background(), events)

	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.calls) != 1 || fs.calls[0] != "Put(create,late.txt,bytes=7)" {
		t.Fatalf("pending write not drained: calls=%v", fs.calls)
	}
}
