package sftp

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/zishmusic/drivel/provider"
	"github.com/zishmusic/drivel/ranges"
)

func TestPutGetStat(t *testing.T) {
	ts := startServer(t)
	s := ts.open(t)

	rf, err := s.Put(t.Context(), "dir/sub/hello.txt", strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if rf.Path != "dir/sub/hello.txt" || rf.Size != 5 || rf.IsDir {
		t.Fatalf("Put returned %+v", rf)
	}
	if rf.Version == "" {
		t.Error("Put returned no Version; echo suppression has nothing to compare on")
	}

	// Put creates missing ancestors — the engine never pre-creates parents.
	if b, err := os.ReadFile(ts.path("dir/sub/hello.txt")); err != nil || string(b) != "hello" {
		t.Fatalf("on disk: %q, %v", b, err)
	}

	got, ok, err := s.Stat(t.Context(), "dir/sub/hello.txt")
	if err != nil || !ok {
		t.Fatalf("Stat: ok=%v err=%v", ok, err)
	}
	if got.Size != 5 || got.Version != rf.Version {
		t.Errorf("Stat = %+v, want size 5 and version %q", got, rf.Version)
	}

	r, err := s.Get(t.Context(), "dir/sub/hello.txt")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer r.Close() //nolint:errcheck // test
	b, _ := io.ReadAll(r)
	if string(b) != "hello" {
		t.Errorf("Get = %q, want %q", b, "hello")
	}
}

// A missing path is ok=false and NOT an error. The engine's reconcile and both M6
// gates branch on ok; an error there would abandon work that should proceed.
func TestStatMissingIsNotAnError(t *testing.T) {
	ts := startServer(t)
	s := ts.open(t)

	rf, ok, err := s.Stat(t.Context(), "nope.txt")
	if err != nil {
		t.Fatalf("Stat of a missing path: %v", err)
	}
	if ok {
		t.Fatalf("Stat reported ok for a missing path: %+v", rf)
	}
}

// Put replaces content wholesale, and must not leave the temporary name behind.
func TestPutReplacesAndLeavesNoTemp(t *testing.T) {
	ts := startServer(t)
	s := ts.open(t)

	if _, err := s.Put(t.Context(), "f.txt", strings.NewReader("first")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := s.Put(t.Context(), "f.txt", strings.NewReader("second-and-longer")); err != nil {
		t.Fatalf("Put again: %v", err)
	}
	if b, _ := os.ReadFile(ts.path("f.txt")); string(b) != "second-and-longer" {
		t.Errorf("content = %q", b)
	}

	entries, _ := os.ReadDir(ts.root)
	for _, e := range entries {
		if isTemp(e.Name()) {
			t.Errorf("upload temporary %s left behind", e.Name())
		}
	}
}

func TestMkdirAndRemoveRecursive(t *testing.T) {
	ts := startServer(t)
	s := ts.open(t)

	rf, err := s.Mkdir(t.Context(), "a/b/c")
	if err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if !rf.IsDir {
		t.Errorf("Mkdir returned IsDir=false")
	}
	if _, err := s.Put(t.Context(), "a/b/c/deep.txt", strings.NewReader("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Remove is recursive: the seam says so, and an rm -rf above the mount arrives
	// as one removal of the top directory.
	if err := s.Remove(t.Context(), "a"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(ts.path("a")); !os.IsNotExist(err) {
		t.Errorf("a survived Remove: %v", err)
	}
}

// Removing something already gone succeeds. The engine can reach Remove twice for
// one deletion (observed by the mount, then inferred by a sweep), and an error the
// second time is a retry loop over an outcome already achieved.
func TestRemoveMissingSucceeds(t *testing.T) {
	ts := startServer(t)
	s := ts.open(t)
	if err := s.Remove(t.Context(), "never/existed.txt"); err != nil {
		t.Fatalf("Remove of a missing path: %v", err)
	}
}

func TestRemoveRefusesTheRoot(t *testing.T) {
	ts := startServer(t)
	s := ts.open(t)
	if err := s.Remove(t.Context(), ""); err == nil {
		t.Fatal("Remove(\"\") deleted the mount root instead of refusing")
	}
	if _, err := os.Stat(ts.root); err != nil {
		t.Fatalf("the root did not survive: %v", err)
	}
}

func TestMove(t *testing.T) {
	ts := startServer(t)
	s := ts.open(t)

	if _, err := s.Put(t.Context(), "old.txt", strings.NewReader("body")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rf, err := s.Move(t.Context(), "old.txt", "sub/new.txt")
	if err != nil {
		t.Fatalf("Move: %v", err)
	}
	if rf.Path != "sub/new.txt" {
		t.Errorf("Move returned path %q", rf.Path)
	}
	if b, _ := os.ReadFile(ts.path("sub/new.txt")); string(b) != "body" {
		t.Errorf("moved content = %q", b)
	}
	if _, err := os.Stat(ts.path("old.txt")); !os.IsNotExist(err) {
		t.Error("the source survived the move")
	}
}

// Move over an existing destination must replace it. Plain SSH_FXP_RENAME is
// specified to fail here, so this is what conn.rename's two branches exist for —
// and the fallback is not hypothetical, since a server may not offer posix-rename.
func TestMoveOverwritesDestination(t *testing.T) {
	ts := startServer(t)
	s := ts.open(t)

	for name, body := range map[string]string{"src.txt": "new", "dst.txt": "old"} {
		if _, err := s.Put(t.Context(), name, strings.NewReader(body)); err != nil {
			t.Fatalf("Put %s: %v", name, err)
		}
	}
	if _, err := s.Move(t.Context(), "src.txt", "dst.txt"); err != nil {
		t.Fatalf("Move onto an existing name: %v", err)
	}
	if b, _ := os.ReadFile(ts.path("dst.txt")); string(b) != "new" {
		t.Errorf("destination = %q, want the source's content", b)
	}
}

// A source the server does not have is ErrNotExist and not a generic failure: the
// engine turns exactly that sentinel into "upload the destination as fresh
// content", which is right for renaming a file that was never pushed.
func TestMoveMissingSourceIsErrNotExist(t *testing.T) {
	ts := startServer(t)
	s := ts.open(t)

	_, err := s.Move(t.Context(), "ghost.txt", "wherever.txt")
	if !errors.Is(err, provider.ErrNotExist) {
		t.Fatalf("Move of a missing source = %v, want provider.ErrNotExist", err)
	}
}

func TestGetRange(t *testing.T) {
	ts := startServer(t)
	s := ts.open(t)

	body := "0123456789abcdef"
	if _, err := s.Put(t.Context(), "f.bin", strings.NewReader(body)); err != nil {
		t.Fatalf("Put: %v", err)
	}

	for _, tc := range []struct {
		name     string
		off, len int64
		want     string
	}{
		{"middle", 4, 6, "456789"},
		{"from an offset to the end", 10, 0, "abcdef"},
		{"whole file", 0, int64(len(body)), body},
		{"length past the end clamps", 12, 999, "cdef"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := s.GetRange(t.Context(), "f.bin", tc.off, tc.len)
			if err != nil {
				t.Fatalf("GetRange: %v", err)
			}
			defer r.Close() //nolint:errcheck // test
			b, err := io.ReadAll(r)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if string(b) != tc.want {
				t.Errorf("GetRange(%d,%d) = %q, want %q", tc.off, tc.len, b, tc.want)
			}
		})
	}
}

// The milestone's headline: extents are spliced in place and every other byte is
// left alone. Drive cannot do this at all, so before this backend M6's gate 2 had
// never run against a server.
func TestPutRangePatchesInPlace(t *testing.T) {
	ts := startServer(t)
	s := ts.open(t)

	original := []byte("AAAAAAAAAABBBBBBBBBBCCCCCCCCCC") // 30 bytes, three runs of ten
	if _, err := s.Put(t.Context(), "f.bin", bytes.NewReader(original)); err != nil {
		t.Fatalf("Put: %v", err)
	}

	local := []byte("AAAAAAAAAAxxxxxxxxxxCCCCCCCCyy") // middle run and the last two bytes differ
	rf, err := s.PutRange(t.Context(), "f.bin", bytes.NewReader(local), int64(len(local)),
		[]ranges.Range{{Off: 10, Len: 10}, {Off: 28, Len: 2}})
	if err != nil {
		t.Fatalf("PutRange: %v", err)
	}
	if rf.Size != int64(len(local)) {
		t.Errorf("PutRange left the file %d bytes, want %d", rf.Size, len(local))
	}

	got, err := os.ReadFile(ts.path("f.bin"))
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if !bytes.Equal(got, local) {
		t.Errorf("after PutRange = %q, want %q", got, local)
	}
}

// The seam says the implementation must re-check the size and fail rather than do
// something clever. A stale size means every offset computed from it lands in the
// wrong place, and failing costs only a whole-file Put.
func TestPutRangeRefusesASizeMismatch(t *testing.T) {
	ts := startServer(t)
	s := ts.open(t)

	if _, err := s.Put(t.Context(), "f.bin", strings.NewReader("0123456789")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	_, err := s.PutRange(t.Context(), "f.bin", strings.NewReader("0123456789abcdef"), 16,
		[]ranges.Range{{Off: 0, Len: 4}})
	if err == nil {
		t.Fatal("PutRange accepted a size that disagreed with the remote")
	}
	if b, _ := os.ReadFile(ts.path("f.bin")); string(b) != "0123456789" {
		t.Errorf("the remote was modified anyway: %q", b)
	}
}

// A range write may neither create nor resize. Both are the whole-file Put's job.
func TestPutRangeRefusesBadInput(t *testing.T) {
	ts := startServer(t)
	s := ts.open(t)

	if _, err := s.Put(t.Context(), "f.bin", strings.NewReader("0123456789")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	src := strings.NewReader("0123456789")

	for name, extents := range map[string][]ranges.Range{
		"past the end":    {{Off: 8, Len: 8}},
		"zero length":     {{Off: 0, Len: 0}},
		"overlapping":     {{Off: 0, Len: 5}, {Off: 3, Len: 5}},
		"out of order":    {{Off: 5, Len: 2}, {Off: 0, Len: 2}},
		"none at all":     {},
		"negative offset": {{Off: -1, Len: 2}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := s.PutRange(t.Context(), "f.bin", src, 10, extents); err == nil {
				t.Errorf("PutRange accepted extents that were %s", name)
			}
		})
	}

	// A file that is not there is not created.
	if _, err := s.PutRange(t.Context(), "absent.bin", src, 10, []ranges.Range{{Off: 0, Len: 2}}); err == nil {
		t.Error("PutRange created a file that did not exist")
	}
	if _, err := os.Stat(ts.path("absent.bin")); !os.IsNotExist(err) {
		t.Error("PutRange created absent.bin")
	}
}

func TestEnumerate(t *testing.T) {
	ts := startServer(t)
	s := ts.open(t)

	for _, p := range []string{"a.txt", "dir/b.txt", "dir/sub/c.txt"} {
		if _, err := s.Put(t.Context(), p, strings.NewReader(p)); err != nil {
			t.Fatalf("Put %s: %v", p, err)
		}
	}

	got := enumerateAll(t, s)
	want := []string{"a.txt", "dir", "dir/b.txt", "dir/sub", "dir/sub/c.txt"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Enumerate = %v, want %v", got, want)
	}
}

// An interrupted sweep resumes from its cursor. syncengine persists the cursor
// after every page, so this is the property that decides what a resume costs.
func TestEnumerateResumesFromItsCursor(t *testing.T) {
	ts := startServer(t)
	s := ts.open(t)

	for _, d := range []string{"d1", "d2", "d3"} {
		if _, err := s.Put(t.Context(), d+"/f.txt", strings.NewReader("x")); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	first, cursor, err := s.Enumerate(t.Context(), "")
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	if cursor == "" {
		t.Skip("the whole tree fit in one page; nothing to resume")
	}
	// Resume with a *fresh* store, as a restarted process would.
	seen := map[string]bool{}
	for _, f := range first {
		seen[f.Path] = true
	}
	s2 := ts.open(t)
	for cursor != "" {
		var files []provider.RemoteFile
		files, cursor, err = s2.Enumerate(t.Context(), cursor)
		if err != nil {
			t.Fatalf("resumed Enumerate: %v", err)
		}
		for _, f := range files {
			seen[f.Path] = true
		}
	}
	for _, want := range []string{"d1", "d1/f.txt", "d2", "d2/f.txt", "d3", "d3/f.txt"} {
		if !seen[want] {
			t.Errorf("a resumed sweep never reported %s", want)
		}
	}
}

// An unreadable resume point restarts the walk rather than failing it: a cursor
// from another version or another provider is not a reason to stop syncing.
func TestEnumerateRecoversFromAGarbageCursor(t *testing.T) {
	ts := startServer(t)
	s := ts.open(t)
	if _, err := s.Put(t.Context(), "a.txt", strings.NewReader("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	for _, cursor := range []string{
		"not json at all",
		`{}`,               // parses, but names no directories
		`{"pending":[]}`,   // ditto, explicitly
		`{"pending":null}`, // ditto, from a different encoder
	} {
		files, next, err := s.Enumerate(t.Context(), cursor)
		if err != nil {
			t.Fatalf("Enumerate with cursor %q: %v", cursor, err)
		}
		// The dangerous answer is "no files, sweep complete": M7b would read every
		// synced path as remotely deleted. Restarting the walk is the safe one.
		if len(files) != 1 || files[0].Path != "a.txt" {
			t.Errorf("Enumerate(%q) = %+v, want the tree from the root", cursor, files)
		}
		if next != "" {
			t.Errorf("Enumerate(%q) did not finish the (tiny) tree: next = %q", cursor, next)
		}
	}
}

// Uploads in flight are invisible to the sweep. Reporting one would materialise a
// .drivel-upload.* beside every interrupted push and then infer its deletion.
func TestEnumerateSkipsUploadTemporaries(t *testing.T) {
	ts := startServer(t)
	s := ts.open(t)

	if err := os.WriteFile(ts.path(tempPrefix+"stranded"), []byte("partial"), 0o600); err != nil {
		t.Fatalf("writing a stranded temporary: %v", err)
	}
	if _, err := s.Put(t.Context(), "real.txt", strings.NewReader("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got := enumerateAll(t, s)
	if strings.Join(got, ",") != "real.txt" {
		t.Errorf("Enumerate = %v, want only real.txt", got)
	}
}

// The seam carries a byte stream and a size, and a symlink has neither honestly.
// READDIR uses LSTAT, so a link arrives as a link — which is also what stops a
// link loop turning the walk into a non-terminating one.
func TestEnumerateSkipsSpecialFiles(t *testing.T) {
	ts := startServer(t)
	s := ts.open(t)

	if _, err := s.Put(t.Context(), "real.txt", strings.NewReader("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := os.Symlink(ts.path("real.txt"), ts.path("link.txt")); err != nil {
		t.Skipf("this filesystem has no symlinks: %v", err)
	}
	// A loop, to prove the walk terminates.
	if err := os.Symlink(ts.root, ts.path("loop")); err != nil {
		t.Skipf("this filesystem has no symlinks: %v", err)
	}

	got := enumerateAll(t, s)
	if strings.Join(got, ",") != "real.txt" {
		t.Errorf("Enumerate = %v, want only real.txt", got)
	}
}

// An empty tree is the shape M7b row 4 calls dangerous — every synced path then
// looks deleted — so it must be reported as an empty listing only when the root
// really is empty, and never as a substitute for an error.
func TestEnumerateOfAnEmptyRootIsEmptyAndComplete(t *testing.T) {
	ts := startServer(t)
	s := ts.open(t)

	files, next, err := s.Enumerate(t.Context(), "")
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	if len(files) != 0 || next != "" {
		t.Errorf("Enumerate of an empty root = %d file(s), next %q", len(files), next)
	}
}

// A root that cannot be listed is an error, never an empty sweep.
func TestEnumerateErrorsWhenTheRootIsGone(t *testing.T) {
	ts := startServer(t)
	s := ts.open(t)
	if _, _, err := s.Enumerate(t.Context(), ""); err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	if err := os.RemoveAll(ts.root); err != nil {
		t.Fatalf("removing the root: %v", err)
	}
	if _, _, err := s.Enumerate(t.Context(), ""); err == nil {
		t.Fatal("Enumerate reported an empty tree for a root it could not list")
	}
}

func enumerateAll(t *testing.T, s *Store) []string {
	t.Helper()
	var out []string
	cursor := ""
	for {
		files, next, err := s.Enumerate(t.Context(), cursor)
		if err != nil {
			t.Fatalf("Enumerate: %v", err)
		}
		for _, f := range files {
			out = append(out, f.Path)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	sort.Strings(out)
	return out
}

// The store is called from the uploader and the downloader at once.
func TestConcurrentUse(t *testing.T) {
	ts := startServer(t)
	s := ts.open(t)

	const n = 12
	errs := make(chan error, n)
	for i := range n {
		go func() {
			name := "f" + string(rune('a'+i)) + ".txt"
			if _, err := s.Put(t.Context(), name, strings.NewReader(name)); err != nil {
				errs <- err
				return
			}
			_, _, err := s.Stat(t.Context(), name)
			errs <- err
		}()
	}
	for range n {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent operation: %v", err)
		}
	}
	entries, _ := os.ReadDir(ts.root)
	if len(entries) != n {
		t.Errorf("wrote %d files, found %d", n, len(entries))
	}
}

// Paths are root-relative and stay inside the root.
func TestPathsStayInsideTheRoot(t *testing.T) {
	ts := startServer(t)
	s := ts.open(t)

	if _, err := s.Put(t.Context(), "../escaped.txt", strings.NewReader("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// It must land inside the root, not beside it. Joining the two directly would
	// have resolved the "..'" by climbing out — which is what this asserts against.
	if _, err := os.Stat(filepath.Join(filepath.Dir(ts.root), "escaped.txt")); !os.IsNotExist(err) {
		t.Error("a path escaped the configured root")
	}
	if _, err := os.Stat(ts.path("escaped.txt")); err != nil {
		t.Errorf("the path did not land inside the root either: %v", err)
	}
}
