package syncengine

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/zishmusic/drivel/internal/fsevent"
	"github.com/zishmusic/drivel/internal/provider"
)

// fakeStore records path-addressed calls. It tracks which paths it has "seen" so
// Put can report create-vs-replace, mirroring a real provider's internal index.
type fakeStore struct {
	mu      sync.Mutex
	calls   []string
	seen    map[string]bool
	content map[string][]byte // path -> bytes served by Get (downloader tests)
}

func newFakeStore() *fakeStore {
	return &fakeStore{seen: map[string]bool{}, content: map[string][]byte{}}
}

func (f *fakeStore) record(format string, a ...any) {
	f.calls = append(f.calls, fmt.Sprintf(format, a...))
}

func (f *fakeStore) Put(_ context.Context, p string, r io.Reader) (provider.RemoteFile, error) {
	b, _ := io.ReadAll(r)
	f.mu.Lock()
	defer f.mu.Unlock()
	verb := "create"
	if f.seen[p] {
		verb = "replace"
	}
	f.seen[p] = true
	f.record("Put(%s,%s,bytes=%d)", verb, p, len(b))
	return provider.RemoteFile{Path: p, Size: int64(len(b))}, nil
}

func (f *fakeStore) Mkdir(_ context.Context, p string) (provider.RemoteFile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen[p] = true
	f.record("Mkdir(%s)", p)
	return provider.RemoteFile{Path: p, IsDir: true}, nil
}

func (f *fakeStore) Move(_ context.Context, oldPath, newPath string) (provider.RemoteFile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.seen[oldPath] {
		return provider.RemoteFile{}, provider.ErrNotExist
	}
	delete(f.seen, oldPath)
	f.seen[newPath] = true
	f.record("Move(%s->%s)", oldPath, newPath)
	return provider.RemoteFile{Path: newPath}, nil
}

func (f *fakeStore) Remove(_ context.Context, p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.seen, p)
	f.record("Remove(%s)", p)
	return nil
}

func (f *fakeStore) Get(_ context.Context, p string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.content[p]
	if !ok {
		return nil, fmt.Errorf("get: unknown path %q", p)
	}
	f.record("Get(%s)", p)
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (f *fakeStore) Stat(_ context.Context, p string) (provider.RemoteFile, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return provider.RemoteFile{Path: p}, f.seen[p], nil
}

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPushMapsEventsToStoreCalls(t *testing.T) {
	dataDir := t.TempDir()
	fs := newFakeStore()
	e := New(Config{Store: fs, DataDir: dataDir})
	ctx := context.Background()

	// New top-level file: content exists on disk, so Create should Put(create).
	writeFile(t, dataDir, "a.txt", "hello")
	e.handle(ctx, fsevent.Event{Op: fsevent.OpCreate, Path: "a.txt"})

	// A subsequent write to the now-known file should Put(replace).
	e.handle(ctx, fsevent.Event{Op: fsevent.OpWrite, Path: "a.txt"})

	// Directory then a nested file: both are path-addressed, no IDs threaded.
	e.handle(ctx, fsevent.Event{Op: fsevent.OpMkdir, Path: "sub"})
	writeFile(t, dataDir, "sub/b.txt", "nested")
	e.handle(ctx, fsevent.Event{Op: fsevent.OpCreate, Path: "sub/b.txt"})

	// Rename the known top-level file.
	e.handle(ctx, fsevent.Event{Op: fsevent.OpRename, Path: "a.txt", NewPath: "renamed.txt"})

	// Delete the nested file.
	e.handle(ctx, fsevent.Event{Op: fsevent.OpUnlink, Path: "sub/b.txt"})

	want := []string{
		"Put(create,a.txt,bytes=5)",
		"Put(replace,a.txt,bytes=5)",
		"Mkdir(sub)",
		"Put(create,sub/b.txt,bytes=6)",
		"Move(a.txt->renamed.txt)",
		"Remove(sub/b.txt)",
	}
	assertCalls(t, fs.calls, want)
}

// Renaming a file the store never received falls back to uploading the
// destination as fresh content (ErrNotExist path).
func TestRenameUnknownSourceFallsBackToPut(t *testing.T) {
	dataDir := t.TempDir()
	fs := newFakeStore()
	e := New(Config{Store: fs, DataDir: dataDir})
	ctx := context.Background()

	writeFile(t, dataDir, "moved.txt", "content")
	e.handle(ctx, fsevent.Event{Op: fsevent.OpRename, Path: "orphan.txt", NewPath: "moved.txt"})

	assertCalls(t, fs.calls, []string{"Put(create,moved.txt,bytes=7)"})
}

func assertCalls(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("call count = %d, want %d\ngot:  %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// Removing an object clears BOTH state records for its path. Dropping only the
// echo leaked one hydration bitmap per deleted path for the life of the state DB
// — and in lazy mode that is every file, because a completed hydration writes a
// full bitmap and nothing else ever removes it.
func TestRemoveClearsEveryStateRecordForThePath(t *testing.T) {
	dataDir := t.TempDir()
	st := newState(t)
	e := New(Config{Store: newFakeStore(), DataDir: dataDir, State: st})
	ctx := context.Background()

	writeFile(t, dataDir, "a.txt", "hello")
	e.handle(ctx, fsevent.Event{Op: fsevent.OpCreate, Path: "a.txt"})
	if err := st.SetHydration("a.txt", []byte("bitmap")); err != nil {
		t.Fatal(err)
	}

	e.handle(ctx, fsevent.Event{Op: fsevent.OpUnlink, Path: "a.txt"})

	if _, ok, err := st.GetEcho("a.txt"); err != nil || ok {
		t.Fatalf("echo still present after Remove (ok=%v, err=%v)", ok, err)
	}
	if _, ok, err := st.Hydration("a.txt"); err != nil || ok {
		t.Fatalf("hydration record still present after Remove (ok=%v, err=%v)", ok, err)
	}
}

// A rename leaves nothing behind at the old path either: its hydration bitmap
// describes bytes that are no longer addressed by that name.
func TestRenameClearsEveryStateRecordForTheOldPath(t *testing.T) {
	dataDir := t.TempDir()
	st := newState(t)
	e := New(Config{Store: newFakeStore(), DataDir: dataDir, State: st})
	ctx := context.Background()

	writeFile(t, dataDir, "a.txt", "hello")
	e.handle(ctx, fsevent.Event{Op: fsevent.OpCreate, Path: "a.txt"})
	if err := st.SetHydration("a.txt", []byte("bitmap")); err != nil {
		t.Fatal(err)
	}

	e.handle(ctx, fsevent.Event{Op: fsevent.OpRename, Path: "a.txt", NewPath: "b.txt"})

	if _, ok, err := st.Hydration("a.txt"); err != nil || ok {
		t.Fatalf("hydration record left at the old path (ok=%v, err=%v)", ok, err)
	}
	if _, ok, err := st.GetEcho("b.txt"); err != nil || !ok {
		t.Fatalf("no baseline recorded at the new path (ok=%v, err=%v)", ok, err)
	}
}
