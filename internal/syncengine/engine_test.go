package syncengine

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/zishmusic/dedupfs/internal/provider"
	"github.com/zishmusic/dedupfs/internal/vfs"
)

// fakeProvider records calls and hands out deterministic IDs.
type fakeProvider struct {
	mu    sync.Mutex
	calls []string
	seq   int
}

func (f *fakeProvider) record(format string, a ...any) { f.calls = append(f.calls, fmt.Sprintf(format, a...)) }
func (f *fakeProvider) nextID() string                 { f.seq++; return fmt.Sprintf("id%d", f.seq) }

func (f *fakeProvider) StartCursor(context.Context) (string, error) { return "c0", nil }
func (f *fakeProvider) Changes(context.Context, string) ([]provider.RemoteChange, string, error) {
	return nil, "c0", nil
}
func (f *fakeProvider) Mkdir(_ context.Context, parentID, name string) (provider.RemoteFile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Mkdir(parent=%s,name=%s)", parentID, name)
	return provider.RemoteFile{ID: f.nextID(), Name: name, ParentID: parentID, IsDir: true}, nil
}
func (f *fakeProvider) Upload(_ context.Context, parentID, name string, r io.Reader) (provider.RemoteFile, error) {
	b, _ := io.ReadAll(r)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Upload(parent=%s,name=%s,bytes=%d)", parentID, name, len(b))
	return provider.RemoteFile{ID: f.nextID(), Name: name, ParentID: parentID}, nil
}
func (f *fakeProvider) Update(_ context.Context, fileID string, r io.Reader) (provider.RemoteFile, error) {
	b, _ := io.ReadAll(r)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Update(id=%s,bytes=%d)", fileID, len(b))
	return provider.RemoteFile{ID: fileID}, nil
}
func (f *fakeProvider) Move(_ context.Context, fileID, newParentID, newName string) (provider.RemoteFile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Move(id=%s,parent=%s,name=%s)", fileID, newParentID, newName)
	return provider.RemoteFile{ID: fileID, Name: newName, ParentID: newParentID}, nil
}
func (f *fakeProvider) Delete(_ context.Context, fileID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Delete(id=%s)", fileID)
	return nil
}
func (f *fakeProvider) Download(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(nil), nil
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

func TestPushMapsEventsToProviderCalls(t *testing.T) {
	dataDir := t.TempDir()
	fp := &fakeProvider{}
	e := New(Config{Provider: fp, DataDir: dataDir, RootID: "root"})
	ctx := context.Background()

	// New top-level file: content exists on disk, so Create should Upload it.
	writeFile(t, dataDir, "a.txt", "hello")
	e.handle(ctx, vfs.Event{Op: vfs.OpCreate, Path: "a.txt"})

	// A subsequent write to the now-known file should Update, not Upload again.
	e.handle(ctx, vfs.Event{Op: vfs.OpWrite, Path: "a.txt"})

	// Directory then a nested file: nested Upload must use the dir's new ID as parent.
	e.handle(ctx, vfs.Event{Op: vfs.OpMkdir, Path: "sub"})
	writeFile(t, dataDir, "sub/b.txt", "nested")
	e.handle(ctx, vfs.Event{Op: vfs.OpCreate, Path: "sub/b.txt"})

	// Rename the known top-level file.
	e.handle(ctx, vfs.Event{Op: vfs.OpRename, Path: "a.txt", NewPath: "renamed.txt"})

	// Delete the nested file.
	e.handle(ctx, vfs.Event{Op: vfs.OpUnlink, Path: "sub/b.txt"})

	want := []string{
		"Upload(parent=root,name=a.txt,bytes=5)",
		"Update(id=id1,bytes=5)",
		"Mkdir(parent=root,name=sub)",
		"Upload(parent=id2,name=b.txt,bytes=6)", // parent is the mkdir'd folder (id2)
		"Move(id=id1,parent=root,name=renamed.txt)",
		"Delete(id=id3)",
	}
	if len(fp.calls) != len(want) {
		t.Fatalf("call count = %d, want %d\ngot:  %v\nwant: %v", len(fp.calls), len(want), fp.calls, want)
	}
	for i := range want {
		if fp.calls[i] != want[i] {
			t.Errorf("call[%d] = %q, want %q", i, fp.calls[i], want[i])
		}
	}
}

func TestEnsureParentCreatesAncestorsOnce(t *testing.T) {
	dataDir := t.TempDir()
	fp := &fakeProvider{}
	e := New(Config{Provider: fp, DataDir: dataDir, RootID: "root"})
	ctx := context.Background()

	// Deep path whose ancestors were never announced via mkdir events: ensureParent
	// must create x then x/y, and reuse them for the second file.
	writeFile(t, dataDir, "x/y/one.txt", "1")
	writeFile(t, dataDir, "x/y/two.txt", "22")
	e.handle(ctx, vfs.Event{Op: vfs.OpCreate, Path: "x/y/one.txt"})
	e.handle(ctx, vfs.Event{Op: vfs.OpCreate, Path: "x/y/two.txt"})

	want := []string{
		"Mkdir(parent=root,name=x)",
		"Mkdir(parent=id1,name=y)",
		"Upload(parent=id2,name=one.txt,bytes=1)",
		"Upload(parent=id2,name=two.txt,bytes=2)", // reuses id2, no new Mkdir
	}
	if fmt.Sprint(fp.calls) != fmt.Sprint(want) {
		t.Fatalf("calls =\n  %v\nwant\n  %v", fp.calls, want)
	}
}
