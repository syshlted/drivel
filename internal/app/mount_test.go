package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zishmusic/drivel/internal/provider"
	"github.com/zishmusic/drivel/internal/testenv"
)

// fakeStore is a provider.Store that keeps everything in memory and records what
// it was asked to do. It implements io.Closer so the lifecycle tests can observe
// that a mount releases its provider.
type fakeStore struct {
	mu     sync.Mutex
	puts   map[string][]byte
	closed int
}

func newFakeStore() *fakeStore { return &fakeStore{puts: map[string][]byte{}} }

func (f *fakeStore) Put(_ context.Context, path string, r io.Reader) (provider.RemoteFile, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return provider.RemoteFile{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts[path] = b
	return provider.RemoteFile{Path: path, Size: int64(len(b)), Version: "1"}, nil
}

func (f *fakeStore) Mkdir(_ context.Context, path string) (provider.RemoteFile, error) {
	return provider.RemoteFile{Path: path, IsDir: true}, nil
}

func (f *fakeStore) Move(_ context.Context, _, newPath string) (provider.RemoteFile, error) {
	return provider.RemoteFile{Path: newPath}, nil
}

func (f *fakeStore) Remove(_ context.Context, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.puts, path)
	return nil
}

func (f *fakeStore) Get(_ context.Context, path string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.puts[path]
	if !ok {
		return nil, provider.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (f *fakeStore) Stat(_ context.Context, path string) (provider.RemoteFile, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.puts[path]
	if !ok {
		return provider.RemoteFile{}, false, nil
	}
	return provider.RemoteFile{Path: path, Size: int64(len(b)), Version: "1"}, true, nil
}

func (f *fakeStore) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	return nil
}

func (f *fakeStore) put(path string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.puts[path]
	return b, ok
}

func (f *fakeStore) closeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// registryWith registers store under kind, ignoring whatever config arrives.
func registryWith(t *testing.T, kind string, store provider.Store) *provider.Registry {
	t.Helper()
	reg := provider.NewRegistry()
	err := reg.Register(kind, func(context.Context, provider.Params) (provider.Store, error) {
		return store, nil
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	return reg
}

// baseSpec is a valid separate-backing-dir spec with no provider.
func baseSpec(t *testing.T) MountSpec {
	t.Helper()
	dir := t.TempDir()
	return MountSpec{
		Name:       "test",
		Mountpoint: filepath.Join(dir, "mnt"),
		DataDir:    filepath.Join(dir, "data"),
		StateDB:    filepath.Join(dir, "state.db"),
	}
}

func TestOpenRejectsInvalidSpec(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*MountSpec)
		want string
	}{
		{"no mountpoint", func(s *MountSpec) { s.Mountpoint = "" }, "mountpoint"},
		{
			// M5: a placeholder is a promise to fetch bytes later.
			"lazy without a provider",
			func(s *MountSpec) { s.Lazy = true; s.Provider = "" },
			"hydrate",
		},
		{
			// §4 echo suppression and M7b's delete baseline both live in the state
			// store; running a provider without one must not be silent.
			"provider without a state db",
			func(s *MountSpec) { s.Provider = "fake"; s.StateDB = "" },
			"state DB",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := baseSpec(t)
			tc.mut(&spec)
			reg := registryWith(t, "fake", newFakeStore())
			m, err := Open(t.Context(), spec, reg)
			if err == nil {
				_ = m.Close()
				t.Fatalf("Open accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q should mention %q", err, tc.want)
			}
		})
	}
}

func TestOpenLogOnly(t *testing.T) {
	spec := baseSpec(t)
	m, err := Open(t.Context(), spec, provider.NewRegistry())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer m.Close() //nolint:errcheck // asserted below

	if m.store != nil {
		t.Error("log-only mount opened a store")
	}
	if m.state != nil {
		t.Error("log-only mount opened a state DB")
	}
	if m.down != nil {
		t.Error("log-only mount started a pull loop")
	}
	if err := m.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestOpenWiresProviderAndState(t *testing.T) {
	spec := baseSpec(t)
	spec.Provider = "fake"
	store := newFakeStore()

	m, err := Open(t.Context(), spec, registryWith(t, "fake", store))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if m.store == nil {
		t.Error("store not wired")
	}
	if m.state == nil {
		t.Error("state DB not opened")
	}
	// fakeStore is not a ChangeSource, so there is nothing to pull from.
	if m.down != nil {
		t.Error("pull loop started for a store with no change feed")
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := store.closeCount(); got != 1 {
		t.Errorf("store closed %d times; want 1", got)
	}
	// The state DB's bbolt lock must actually be gone, or the next mount of the
	// same account blocks for five seconds and then fails opaquely.
	if _, err := os.Stat(spec.StateDB); err != nil {
		t.Fatalf("state db missing: %v", err)
	}
	m2, err := Open(t.Context(), spec, registryWith(t, "fake", newFakeStore()))
	if err != nil {
		t.Fatalf("reopen after Close: %v", err)
	}
	_ = m2.Close()
}

// A failure partway through Open has to hand back what it already took. Without
// it a failed startup leaks a dirfd, a bbolt lock and a transport per mount.
func TestOpenFailureReleasesAcquiredResources(t *testing.T) {
	spec := baseSpec(t)
	spec.Provider = "fake"
	// A directory is not a bbolt file, so state.Open fails *after* the store opened.
	spec.StateDB = t.TempDir()
	store := newFakeStore()

	if _, err := Open(t.Context(), spec, registryWith(t, "fake", store)); err == nil {
		t.Fatal("Open succeeded with an unusable state DB")
	}
	if got := store.closeCount(); got != 1 {
		t.Errorf("store closed %d times after a failed Open; want 1", got)
	}
}

func TestOpenReportsUnknownProvider(t *testing.T) {
	spec := baseSpec(t)
	spec.Provider = "nosuch"
	_, err := Open(t.Context(), spec, registryWith(t, "fake", newFakeStore()))
	if !errors.Is(err, provider.ErrUnknownKind) {
		t.Fatalf("err = %v; want ErrUnknownKind", err)
	}
}

func TestCloseIdempotent(t *testing.T) {
	spec := baseSpec(t)
	spec.Provider = "fake"
	store := newFakeStore()
	m, err := Open(t.Context(), spec, registryWith(t, "fake", store))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for i := range 3 {
		if err := m.Close(); err != nil {
			t.Fatalf("Close #%d: %v", i, err)
		}
	}
	if got := store.closeCount(); got != 1 {
		t.Errorf("store closed %d times across 3 Closes; want 1", got)
	}
}

// The whole lifecycle over a real mount: Run serves, a write through the
// mountpoint reaches the backing dir, and the drain on unmount pushes it to the
// provider before Run returns. This is the path no test could reach before the
// wiring left main.
func TestRunServesAndDrainsToProvider(t *testing.T) {
	if _, err := os.Stat("/dev/fuse"); err != nil {
		testenv.Unavailable(t, testenv.FUSE, "no /dev/fuse: "+err.Error())
	}
	spec := baseSpec(t)
	spec.Provider = "fake"
	store := newFakeStore()

	m, err := Open(t.Context(), spec, registryWith(t, "fake", store))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer m.Close() //nolint:errcheck // best effort in cleanup

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()

	// Readiness has to be observed, not assumed: until the mount is live the
	// mountpoint is an ordinary directory, and a write to it would succeed
	// without ever touching FUSE. Probe from the backing side instead.
	probe := ".mount-probe"
	if err := os.WriteFile(filepath.Join(spec.DataDir, probe), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !waitFor(10*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(spec.Mountpoint, probe))
		return err == nil
	}) {
		cancel()
		<-done
		testenv.Unavailable(t, testenv.FUSE, "mount did not come up within 10s")
	}

	if err := os.WriteFile(filepath.Join(spec.Mountpoint, "hello.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write through the mount: %v", err)
	}

	// Unmount. Run must not return until the engine has drained, so the push is
	// guaranteed to have happened by the time we look (DESIGN.md §7).
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after unmount; try: fusermount3 -u " + spec.Mountpoint)
	}

	if _, err := os.Stat(filepath.Join(spec.DataDir, "hello.txt")); err != nil {
		t.Errorf("write did not reach the backing dir: %v", err)
	}
	got, ok := store.put("hello.txt")
	if !ok {
		t.Fatal("engine drained without pushing the file to the provider")
	}
	if string(got) != "hi" {
		t.Errorf("pushed %q; want %q", got, "hi")
	}
}

func waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}
