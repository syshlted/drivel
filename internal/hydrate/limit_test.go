package hydrate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zishmusic/drivel/internal/testenv"
	"github.com/zishmusic/drivel/provider"
)

// gateStore holds every Get open until it is released, and records the highest
// number it ever held at once. Blocking rather than sleeping is what makes the
// measurement exact: with a delay the peak depends on scheduling, so a broken
// limiter would sometimes still measure under its cap.
type gateStore struct {
	release chan struct{} // closed to let every held Get return

	mu     sync.Mutex
	inside int
	peak   int

	arrived chan struct{} // one token per Get that has reached the gate
}

func newGateStore() *gateStore {
	return &gateStore{release: make(chan struct{}), arrived: make(chan struct{}, 1024)}
}

func (g *gateStore) Get(ctx context.Context, p string) (io.ReadCloser, error) {
	g.mu.Lock()
	g.inside++
	if g.inside > g.peak {
		g.peak = g.inside
	}
	g.mu.Unlock()
	g.arrived <- struct{}{}

	select {
	case <-g.release:
	case <-ctx.Done():
	}

	g.mu.Lock()
	g.inside--
	g.mu.Unlock()
	return io.NopCloser(strings.NewReader(p)), nil
}

func (g *gateStore) peakSeen() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.peak
}

func (g *gateStore) Put(context.Context, string, io.Reader) (provider.RemoteFile, error) {
	return provider.RemoteFile{}, errors.New("unexpected Put")
}
func (g *gateStore) Mkdir(context.Context, string) (provider.RemoteFile, error) {
	return provider.RemoteFile{}, errors.New("unexpected Mkdir")
}
func (g *gateStore) Move(context.Context, string, string) (provider.RemoteFile, error) {
	return provider.RemoteFile{}, errors.New("unexpected Move")
}
func (g *gateStore) Remove(context.Context, string) error { return errors.New("unexpected Remove") }
func (g *gateStore) Stat(context.Context, string) (provider.RemoteFile, bool, error) {
	return provider.RemoteFile{}, false, nil
}

// placeholders stamps n placeholders and returns their paths.
func placeholders(t *testing.T, h *Hydrator, n int) []string {
	t.Helper()
	paths := make([]string, n)
	for i := range paths {
		paths[i] = fmt.Sprintf("f%02d.bin", i)
		body := strings.Repeat("x", 16)
		if err := h.CreatePlaceholder(paths[i], provider.RemoteFile{
			Path: paths[i], Size: int64(len(body)), Hash: "h", Version: "1",
			Modified: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
		}); err != nil {
			t.Fatalf("CreatePlaceholder %s: %v", paths[i], err)
		}
	}
	return paths
}

// A recursive read over a lazy tree must not fault every file at once: no more
// than `workers` fetches reach the store simultaneously. This is the property the
// pool exists for — before it, 40 concurrent opens were 40 concurrent downloads.
func TestHydrateBoundsConcurrentFetches(t *testing.T) {
	const (
		workers = 3
		files   = 20
	)
	dir := t.TempDir()
	gate := newGateStore()
	h := New(dir, gate, nil, workers)
	if !h.XattrsUsable() {
		testenv.Unavailable(t, testenv.Xattr, "backing filesystem does not store user.* attributes")
	}
	paths := placeholders(t, h, files)

	var wg sync.WaitGroup
	errs := make(chan error, files)
	for _, p := range paths {
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			if err := h.Hydrate(t.Context(), p); err != nil {
				errs <- err
			}
		}(p)
	}

	// Wait until the pool is saturated and then some: every slot filled, with the
	// rest of the files necessarily queued behind them. If the limiter were absent
	// all `files` would arrive, which is what the peak assertion below catches.
	for range workers {
		select {
		case <-gate.arrived:
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for the fetch pool to fill")
		}
	}
	// Nothing else can arrive while the held fetches occupy every slot; give any
	// unbounded extras a window to show up before measuring.
	time.Sleep(50 * time.Millisecond)
	if got := gate.peakSeen(); got > workers {
		t.Fatalf("peak concurrent fetches = %d, want <= %d", got, workers)
	}

	close(gate.release)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("Hydrate: %v", err)
	}
	if got := gate.peakSeen(); got > workers {
		t.Errorf("peak concurrent fetches over the whole run = %d, want <= %d", got, workers)
	}
	// The cap must bound the work, not drop it.
	for _, p := range paths {
		if _, still, err := h.Marker(p); err != nil {
			t.Fatalf("Marker %s: %v", p, err)
		} else if still {
			t.Errorf("%s is still a placeholder after Hydrate", p)
		}
	}
}

// A pool of one still hydrates every file: the limiter serialises, it does not
// deadlock. Guards the shape of the acquire — a slot released on the wrong path
// (or a zero-sized channel) shows up here as a hang rather than as a wrong count.
func TestHydrateSerialisedStillCompletes(t *testing.T) {
	dir := t.TempDir()
	gate := newGateStore()
	close(gate.release) // never hold; just count
	h := New(dir, gate, nil, 1)
	if !h.XattrsUsable() {
		testenv.Unavailable(t, testenv.Xattr, "backing filesystem does not store user.* attributes")
	}
	paths := placeholders(t, h, 8)

	var wg sync.WaitGroup
	for _, p := range paths {
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			if err := h.Hydrate(t.Context(), p); err != nil {
				t.Errorf("Hydrate %s: %v", p, err)
			}
		}(p)
	}
	wg.Wait()
	if got := gate.peakSeen(); got != 1 {
		t.Errorf("peak concurrent fetches = %d, want 1", got)
	}
}

// Cancelling while queued for a slot returns the context error and never reaches
// the store. A caller who gave up must not spend one of the slots the callers
// still waiting are queued for.
func TestHydrateCancelledWhileQueuedSkipsStore(t *testing.T) {
	dir := t.TempDir()
	gate := newGateStore()
	h := New(dir, gate, nil, 1)
	if !h.XattrsUsable() {
		testenv.Unavailable(t, testenv.Xattr, "backing filesystem does not store user.* attributes")
	}
	paths := placeholders(t, h, 2)

	// Occupy the only slot.
	held := make(chan error, 1)
	go func() { held <- h.Hydrate(t.Context(), paths[0]) }()
	select {
	case <-gate.arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the first fetch to reach the store")
	}

	var reached atomic.Int32
	reached.Store(int32(len(gate.arrived)))

	ctx, cancel := context.WithCancel(t.Context())
	queued := make(chan error, 1)
	go func() { queued <- h.Hydrate(ctx, paths[1]) }()
	time.Sleep(50 * time.Millisecond) // let it reach the semaphore
	cancel()

	select {
	case err := <-queued:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("queued Hydrate returned %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled Hydrate did not return")
	}
	if len(gate.arrived) != 0 {
		t.Errorf("the cancelled fetch reached the store; want it declined before the request")
	}
	if _, still, err := h.Marker(paths[1]); err != nil {
		t.Fatalf("Marker: %v", err)
	} else if !still {
		t.Errorf("%s lost its placeholder marker on a cancelled hydration", paths[1])
	}

	close(gate.release)
	if err := <-held; err != nil {
		t.Errorf("held Hydrate: %v", err)
	}
}
