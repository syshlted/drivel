package syncengine

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zishmusic/drivel/internal/fsevent"
	"github.com/zishmusic/drivel/internal/state"
	"github.com/zishmusic/drivel/provider"
	"github.com/zishmusic/drivel/ranges"
)

// patchStore is a provider that CAN write byte ranges — the capability Drive
// lacks (see the gdrive RangePutter note). It exists so the M6 range-write path
// is exercised end to end rather than only reasoned about, and so the fallbacks
// can be observed choosing Put over PutRange.
type patchStore struct {
	mu       sync.Mutex
	calls    []string
	stats    int
	content  map[string][]byte
	failNext error // one-shot PutRange failure
}

func newPatchStore() *patchStore { return &patchStore{content: map[string][]byte{}} }

var (
	_ provider.Store         = (*patchStore)(nil)
	_ provider.RangePutter   = (*patchStore)(nil)
	_ provider.ContentHasher = (*patchStore)(nil)
)

func (s *patchStore) record(format string, a ...any) {
	s.calls = append(s.calls, fmt.Sprintf(format, a...))
}

func (s *patchStore) ops() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

func (s *patchStore) statCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

func (s *patchStore) remoteBytes(p string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.content[p]...)
}

// seed installs remote content without recording a call, so a test's assertions
// see only the ops the engine made.
func (s *patchStore) seed(p string, b []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.content[p] = append([]byte(nil), b...)
}

func (s *patchStore) file(p string) provider.RemoteFile {
	sum := md5.Sum(s.content[p])
	return provider.RemoteFile{
		Path: p,
		Size: int64(len(s.content[p])),
		Hash: hex.EncodeToString(sum[:]),
	}
}

func (s *patchStore) Put(_ context.Context, p string, r io.Reader) (provider.RemoteFile, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return provider.RemoteFile{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.content[p] = b
	s.record("Put(%s,bytes=%d)", p, len(b))
	return s.file(p), nil
}

// PutRange overwrites exactly the named extents, enforcing the seam's contract:
// the object must exist at the stated size, and a range write may never resize.
func (s *patchStore) PutRange(_ context.Context, p string, src io.ReaderAt, size int64, extents []ranges.Range) (provider.RemoteFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failNext != nil {
		err := s.failNext
		s.failNext = nil
		s.record("PutRange(%s,FAILED)", p)
		return provider.RemoteFile{}, err
	}
	cur, ok := s.content[p]
	if !ok {
		return provider.RemoteFile{}, fmt.Errorf("range write: %q does not exist", p)
	}
	if int64(len(cur)) != size {
		return provider.RemoteFile{}, fmt.Errorf("range write: %q is %d bytes, caller said %d", p, len(cur), size)
	}
	var wrote int64
	for _, r := range extents {
		if r.Off < 0 || r.Len <= 0 || r.Off+r.Len > size {
			return provider.RemoteFile{}, fmt.Errorf("range write: extent %+v outside [0,%d)", r, size)
		}
		buf := make([]byte, r.Len)
		if _, err := src.ReadAt(buf, r.Off); err != nil {
			return provider.RemoteFile{}, err
		}
		copy(cur[r.Off:], buf)
		wrote += r.Len
	}
	s.record("PutRange(%s,extents=%d,bytes=%d)", p, len(extents), wrote)
	return s.file(p), nil
}

func (s *patchStore) HashContent(r io.Reader) (string, error) {
	h := md5.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (s *patchStore) Stat(_ context.Context, p string) (provider.RemoteFile, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats++
	if _, ok := s.content[p]; !ok {
		return provider.RemoteFile{}, false, nil
	}
	return s.file(p), true, nil
}

func (s *patchStore) Mkdir(_ context.Context, p string) (provider.RemoteFile, error) {
	return provider.RemoteFile{Path: p, IsDir: true}, nil
}
func (s *patchStore) Move(_ context.Context, _, newPath string) (provider.RemoteFile, error) {
	return provider.RemoteFile{Path: newPath}, nil
}
func (s *patchStore) Remove(_ context.Context, p string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.content, p)
	return nil
}
func (s *patchStore) Get(_ context.Context, p string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.content[p]
	if !ok {
		return nil, fmt.Errorf("get: unknown path %q", p)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// --- fixtures ---------------------------------------------------------------

const testBlock = ranges.DefaultBlockSize

// bigFile builds a deterministic, non-uniform body so a patch that writes the
// wrong extent produces visibly wrong content rather than coincidentally right.
func bigFile(blocks int) []byte {
	b := make([]byte, int64(blocks)*testBlock)
	for i := range b {
		b[i] = byte(i%251 + 1)
	}
	return b
}

// syncedAt puts body on the remote and records the echo a successful push would
// have written. This is the state a path is in after any normal sync, and it is
// what a range write requires: without it the engine cannot prove the remote is
// still the version the extents were measured against.
func syncedAt(t *testing.T, st *state.Store, store *patchStore, p string, body []byte) {
	t.Helper()
	store.seed(p, body)
	sum := md5.Sum(body)
	if err := st.SetEcho(p, state.Echo{Hash: hex.EncodeToString(sum[:])}); err != nil {
		t.Fatal(err)
	}
}

// localEdit writes body to dir/name with [off,off+len(patch)) overwritten, and
// returns the dirty set a mount would have reported for that write.
func localEdit(t *testing.T, dir, name string, body []byte, off int64, patch []byte) *ranges.Set {
	t.Helper()
	edited := append([]byte(nil), body...)
	copy(edited[off:], patch)
	if err := os.WriteFile(filepath.Join(dir, name), edited, 0o644); err != nil {
		t.Fatal(err)
	}
	d := ranges.New(int64(len(edited)), ranges.DefaultBlockSize)
	d.MarkCovering(off, int64(len(patch)))
	return &d
}

// --- the range-write path ---------------------------------------------------

// The headline M6 behaviour: editing one block of a large file sends one block,
// and the remote ends up byte-identical to the local file.
func TestRangeWriteShipsOnlyDirtyExtents(t *testing.T) {
	dir := t.TempDir()
	st := newState(t)
	body := bigFile(4)
	store := newPatchStore()
	syncedAt(t, st, store, "big.bin", body)

	dirty := localEdit(t, dir, "big.bin", body, testBlock+1024, []byte("patched"))
	e := New(Config{Store: store, DataDir: dir, State: st})

	if err := e.pushContent(context.Background(), "big.bin", dirty); err != nil {
		t.Fatal(err)
	}

	want := fmt.Sprintf("PutRange(big.bin,extents=1,bytes=%d)", testBlock)
	if got := store.ops(); len(got) != 1 || got[0] != want {
		t.Fatalf("ops = %v; want [%s]", got, want)
	}
	local, err := os.ReadFile(filepath.Join(dir, "big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(store.remoteBytes("big.bin"), local) {
		t.Fatal("remote content diverged from local after the range write")
	}
}

// A patched remote is only correct if the untouched blocks are genuinely
// untouched — assert the bytes outside the extent are the ORIGINAL ones, which a
// whole-file upload would also satisfy but a mis-offset patch would not.
func TestRangeWriteLeavesOtherBlocksIntact(t *testing.T) {
	dir := t.TempDir()
	st := newState(t)
	body := bigFile(4)
	store := newPatchStore()
	syncedAt(t, st, store, "big.bin", body)

	dirty := localEdit(t, dir, "big.bin", body, 2*testBlock, []byte("zzzz"))
	e := New(Config{Store: store, DataDir: dir, State: st})
	if err := e.pushContent(context.Background(), "big.bin", dirty); err != nil {
		t.Fatal(err)
	}

	got := store.remoteBytes("big.bin")
	if !bytes.Equal(got[:2*testBlock], body[:2*testBlock]) {
		t.Error("blocks before the patch were modified")
	}
	if !bytes.Equal(got[3*testBlock:], body[3*testBlock:]) {
		t.Error("blocks after the patch were modified")
	}
	if !bytes.HasPrefix(got[2*testBlock:], []byte("zzzz")) {
		t.Error("the patched extent does not hold the new bytes")
	}
}

// Several separate edits in one push become several extents in one call.
func TestRangeWriteSendsMultipleExtents(t *testing.T) {
	dir := t.TempDir()
	st := newState(t)
	body := bigFile(6)
	store := newPatchStore()
	syncedAt(t, st, store, "multi.bin", body)

	edited := append([]byte(nil), body...)
	copy(edited[0:], []byte("aaa"))
	copy(edited[4*testBlock:], []byte("bbb"))
	if err := os.WriteFile(filepath.Join(dir, "multi.bin"), edited, 0o644); err != nil {
		t.Fatal(err)
	}
	d := ranges.New(int64(len(edited)), ranges.DefaultBlockSize)
	d.MarkCovering(0, 3)
	d.MarkCovering(4*testBlock, 3)

	e := New(Config{Store: store, DataDir: dir, State: st})
	if err := e.pushContent(context.Background(), "multi.bin", &d); err != nil {
		t.Fatal(err)
	}

	want := fmt.Sprintf("PutRange(multi.bin,extents=2,bytes=%d)", 2*testBlock)
	if got := store.ops(); len(got) != 1 || got[0] != want {
		t.Fatalf("ops = %v; want [%s]", got, want)
	}
	if !bytes.Equal(store.remoteBytes("multi.bin"), edited) {
		t.Fatal("remote content diverged from local")
	}
}

// --- every fallback lands on a whole-file Put -------------------------------

// Each of these must produce a correct upload by the slower route. The failure
// they guard against is the opposite of a crash: a patch applied against offsets
// that no longer mean what they meant.
func TestRangeWriteFallsBackToWholeFile(t *testing.T) {
	body := bigFile(4)

	cases := []struct {
		name  string
		setup func(t *testing.T, dir string, st *state.Store, store *patchStore) *ranges.Set
	}{
		{
			// No mount reported extents (a rename fallback, a synthesised event).
			name: "unknown extents",
			setup: func(t *testing.T, dir string, st *state.Store, store *patchStore) *ranges.Set {
				syncedAt(t, st, store, "big.bin", body)
				localEdit(t, dir, "big.bin", body, testBlock, []byte("x"))
				return nil
			},
		},
		{
			// The set describes a different-length file than the one on disk: a
			// truncate or a racing writer moved every offset after the cut.
			name: "size mismatch",
			setup: func(t *testing.T, dir string, st *state.Store, store *patchStore) *ranges.Set {
				syncedAt(t, st, store, "big.bin", body)
				dirty := localEdit(t, dir, "big.bin", body, testBlock, []byte("x"))
				stale := ranges.New(dirty.Size+testBlock, ranges.DefaultBlockSize)
				stale.MarkCovering(testBlock, 1)
				return &stale
			},
		},
		{
			// PutRange cannot create; a first upload must be a Put.
			name: "remote does not exist",
			setup: func(t *testing.T, dir string, st *state.Store, store *patchStore) *ranges.Set {
				return localEdit(t, dir, "big.bin", body, testBlock, []byte("x"))
			},
		},
		{
			// PutRange may not resize, and the remote is a different length.
			name: "remote size differs",
			setup: func(t *testing.T, dir string, st *state.Store, store *patchStore) *ranges.Set {
				syncedAt(t, st, store, "big.bin", bigFile(3))
				return localEdit(t, dir, "big.bin", body, testBlock, []byte("x"))
			},
		},
		{
			// Every block is dirty, so a patch would send the whole file anyway.
			name: "everything dirty",
			setup: func(t *testing.T, dir string, st *state.Store, store *patchStore) *ranges.Set {
				syncedAt(t, st, store, "big.bin", body)
				dirty := localEdit(t, dir, "big.bin", body, 0, []byte("x"))
				dirty.MarkAll()
				return dirty
			},
		},
		{
			// The provider tried and failed: slower is always available.
			name: "PutRange fails",
			setup: func(t *testing.T, dir string, st *state.Store, store *patchStore) *ranges.Set {
				syncedAt(t, st, store, "big.bin", body)
				store.failNext = fmt.Errorf("range write unsupported for this object")
				return localEdit(t, dir, "big.bin", body, testBlock, []byte("x"))
			},
		},
		{
			// We have never synced this path, so there is no way to prove the
			// remote is the version these extents were measured against.
			name: "no echo record",
			setup: func(t *testing.T, dir string, st *state.Store, store *patchStore) *ranges.Set {
				store.seed("big.bin", body) // remote exists, but nothing recorded it
				return localEdit(t, dir, "big.bin", body, testBlock, []byte("x"))
			},
		},
		{
			// Someone else edited the remote to a different body of the same
			// length. Patching into it would splice our blocks into their file.
			name: "remote diverged at the same size",
			setup: func(t *testing.T, dir string, st *state.Store, store *patchStore) *ranges.Set {
				syncedAt(t, st, store, "big.bin", body)
				theirs := bigFile(4)
				copy(theirs[3*testBlock:], []byte("THEIR EDIT"))
				store.seed("big.bin", theirs) // echo still names the old version
				return localEdit(t, dir, "big.bin", body, testBlock, []byte("x"))
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			st := newState(t)
			store := newPatchStore()
			dirty := tc.setup(t, dir, st, store)

			e := New(Config{Store: store, DataDir: dir, State: st})
			if err := e.pushContent(context.Background(), "big.bin", dirty); err != nil {
				t.Fatal(err)
			}

			ops := store.ops()
			if len(ops) == 0 {
				t.Fatal("nothing was uploaded at all")
			}
			last := ops[len(ops)-1]
			if want := fmt.Sprintf("Put(big.bin,bytes=%d)", len(body)); last != want {
				t.Fatalf("ops = %v; want the push to end in %s", ops, want)
			}
			// However it got there, the remote must hold the local bytes.
			local, err := os.ReadFile(filepath.Join(dir, "big.bin"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(store.remoteBytes("big.bin"), local) {
				t.Fatal("remote content diverged from local")
			}
		})
	}
}

// A provider without RangePutter — Drive's situation — ignores the extents
// entirely and uploads whole files, exactly as it did before M6.
func TestNonPatchingProviderIgnoresExtents(t *testing.T) {
	dir := t.TempDir()
	body := bigFile(4)
	fs := newFakeStore()

	dirty := localEdit(t, dir, "big.bin", body, testBlock, []byte("x"))
	e := New(Config{Store: fs, DataDir: dir})
	if err := e.pushContent(context.Background(), "big.bin", dirty); err != nil {
		t.Fatal(err)
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()
	want := fmt.Sprintf("Put(create,big.bin,bytes=%d)", len(body))
	if len(fs.calls) != 1 || fs.calls[0] != want {
		t.Fatalf("calls = %v; want [%s]", fs.calls, want)
	}
}

// --- the unchanged-content gate ---------------------------------------------

// bigBody is a large, deterministic body — large enough to clear
// hashSkipMinSize, so the gate actually runs.
func bigBody(tag byte) []byte {
	b := bigFile(2)
	b[0] = tag
	return b
}

// A write that did not change the bytes makes no upload at all: the remote
// already holds exactly this content.
func TestUnchangedContentSkipsUpload(t *testing.T) {
	dir := t.TempDir()
	st := newState(t)
	store := newPatchStore()

	body := bigBody('a')
	if err := os.WriteFile(filepath.Join(dir, "a.bin"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	syncedAt(t, st, store, "a.bin", body)

	e := New(Config{Store: store, DataDir: dir, State: st})
	if err := e.pushContent(context.Background(), "a.bin", nil); err != nil {
		t.Fatal(err)
	}
	if got := store.ops(); len(got) != 0 {
		t.Fatalf("unchanged content should upload nothing; got %v", got)
	}
}

// The gate must open the moment the content really differs, or a real edit would
// be silently dropped.
func TestChangedContentStillUploads(t *testing.T) {
	dir := t.TempDir()
	st := newState(t)
	store := newPatchStore()

	old := bigBody('o')
	syncedAt(t, st, store, "a.bin", old)
	fresh := bigBody('n')
	if err := os.WriteFile(filepath.Join(dir, "a.bin"), fresh, 0o644); err != nil {
		t.Fatal(err)
	}

	e := New(Config{Store: store, DataDir: dir, State: st})
	if err := e.pushContent(context.Background(), "a.bin", nil); err != nil {
		t.Fatal(err)
	}
	if got := store.ops(); len(got) != 1 {
		t.Fatalf("ops = %v; want a single Put", got)
	}
	if !bytes.Equal(store.remoteBytes("a.bin"), fresh) {
		t.Fatal("remote does not hold the new content")
	}
}

// The gate compares against what the remote CURRENTLY holds, not against the echo
// record. A remote that vanished outside the change feed — an expired cursor
// across a downtime, a state DB reused against a different root, a delete we
// never learned about — leaves a stale echo that still matches our local file. If
// that were the comparison, the file would be skipped on every push and never
// restored.
func TestStaleEchoDoesNotBlockReupload(t *testing.T) {
	dir := t.TempDir()
	st := newState(t)
	store := newPatchStore()

	body := bigBody('a')
	if err := os.WriteFile(filepath.Join(dir, "a.bin"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	// Echo says we synced this content; the remote no longer has it.
	sum := md5.Sum(body)
	if err := st.SetEcho("a.bin", state.Echo{Hash: hex.EncodeToString(sum[:])}); err != nil {
		t.Fatal(err)
	}

	e := New(Config{Store: store, DataDir: dir, State: st})
	if err := e.pushContent(context.Background(), "a.bin", nil); err != nil {
		t.Fatal(err)
	}
	if got := store.ops(); len(got) != 1 {
		t.Fatalf("ops = %v; want the file re-uploaded despite the matching echo", got)
	}
	if !bytes.Equal(store.remoteBytes("a.bin"), body) {
		t.Fatal("the file was not restored on the remote")
	}
}

// A first upload has nothing to compare against and must proceed.
func TestNoRemoteObjectStillUploads(t *testing.T) {
	dir := t.TempDir()
	st := newState(t)
	store := newPatchStore()
	if err := os.WriteFile(filepath.Join(dir, "a.bin"), bigBody('a'), 0o644); err != nil {
		t.Fatal(err)
	}

	e := New(Config{Store: store, DataDir: dir, State: st})
	if err := e.pushContent(context.Background(), "a.bin", nil); err != nil {
		t.Fatal(err)
	}
	if got := store.ops(); len(got) != 1 {
		t.Fatalf("ops = %v; want one Put", got)
	}
}

// A small file is not worth a round-trip to interrogate: the gates would spend a
// request to save a request, so they are skipped entirely and the push is exactly
// what it was before M6.
func TestSmallFileSkipsTheRemoteCheck(t *testing.T) {
	dir := t.TempDir()
	st := newState(t)
	store := newPatchStore()

	body := []byte("small enough not to bother")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	syncedAt(t, st, store, "a.txt", body) // identical content, but too small to check

	e := New(Config{Store: store, DataDir: dir, State: st})
	if err := e.pushContent(context.Background(), "a.txt", nil); err != nil {
		t.Fatal(err)
	}
	if got := store.statCount(); got != 0 {
		t.Errorf("a small file cost %d Stat call(s); want 0", got)
	}
	if got := store.ops(); len(got) != 1 {
		t.Fatalf("ops = %v; want the plain single Put", got)
	}
}

// A push that ends in a whole-file Put must have rewound the file first — the
// hash gate reads it to EOF, and a missed Seek would upload zero bytes over good
// remote content.
func TestWholeFilePutRewindsAfterHashing(t *testing.T) {
	dir := t.TempDir()
	st := newState(t)
	store := newPatchStore()

	syncedAt(t, st, store, "a.bin", bigBody('o'))
	body := bigBody('n')
	if err := os.WriteFile(filepath.Join(dir, "a.bin"), body, 0o644); err != nil {
		t.Fatal(err)
	}

	e := New(Config{Store: store, DataDir: dir, State: st})
	if err := e.pushContent(context.Background(), "a.bin", nil); err != nil {
		t.Fatal(err)
	}
	if got := store.remoteBytes("a.bin"); !bytes.Equal(got, body) {
		t.Fatalf("uploaded %d bytes; want the full %d", len(got), len(body))
	}
}

// A remote someone else edited must never receive a splice of our blocks: the
// result would be a hybrid file that existed nowhere, with no conflict copy and
// no intact version of anyone's work. The push degrades to a whole-file Put,
// whose last-writer-wins loss is at least the documented §6 policy.
func TestDivergedRemoteIsNeverSpliced(t *testing.T) {
	dir := t.TempDir()
	st := newState(t)
	store := newPatchStore()

	base := bigFile(4)
	syncedAt(t, st, store, "big.bin", base)

	// Their edit: same length, different bytes, in a block we did NOT touch.
	theirs := append([]byte(nil), base...)
	copy(theirs[3*testBlock:], []byte("THEIR EDIT"))
	store.seed("big.bin", theirs)

	dirty := localEdit(t, dir, "big.bin", base, testBlock, []byte("OUR EDIT"))
	e := New(Config{Store: store, DataDir: dir, State: st})
	if err := e.pushContent(context.Background(), "big.bin", dirty); err != nil {
		t.Fatal(err)
	}

	for _, op := range store.ops() {
		if strings.HasPrefix(op, "PutRange(") {
			t.Fatalf("patched a remote we had not verified: %v", store.ops())
		}
	}
	local, err := os.ReadFile(filepath.Join(dir, "big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	got := store.remoteBytes("big.bin")
	if !bytes.Equal(got, local) {
		t.Fatal("remote is neither our version nor theirs — a hybrid was written")
	}
}

// After a range write the echo must name the patched content, or the pull loop
// will read the provider's report of our own write as a remote edit (§4) — and
// the next range write will refuse, having lost its proof of identity.
func TestRangeWriteRecordsEcho(t *testing.T) {
	dir := t.TempDir()
	st := newState(t)
	body := bigFile(4)
	store := newPatchStore()
	syncedAt(t, st, store, "big.bin", body)

	dirty := localEdit(t, dir, "big.bin", body, testBlock, []byte("patched"))
	e := New(Config{Store: store, DataDir: dir, State: st})
	if err := e.pushContent(context.Background(), "big.bin", dirty); err != nil {
		t.Fatal(err)
	}

	echo, ok, err := st.GetEcho("big.bin")
	if err != nil || !ok {
		t.Fatalf("no echo recorded after a range write (ok=%v err=%v)", ok, err)
	}
	local, err := os.ReadFile(filepath.Join(dir, "big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	sum := md5.Sum(local)
	if want := hex.EncodeToString(sum[:]); echo.Hash != want {
		t.Fatalf("echo hash = %q; want the patched content's %q", echo.Hash, want)
	}

	// And the recorded echo is exactly what lets the NEXT partial write patch.
	next := localEdit(t, dir, "big.bin", local, 2*testBlock, []byte("again"))
	if err := e.pushContent(context.Background(), "big.bin", next); err != nil {
		t.Fatal(err)
	}
	ops := store.ops()
	if last := ops[len(ops)-1]; !strings.HasPrefix(last, "PutRange(") {
		t.Fatalf("the second partial write did not patch: %v", ops)
	}
}

// A placeholder must still be skipped before any of the M6 gates run: the M5
// guard is about data loss, not cost, and nothing may get in front of it.
func TestPlaceholderStillSkippedWithExtents(t *testing.T) {
	dir := t.TempDir()
	st := newState(t)
	body := bigFile(4)
	store := newPatchStore()
	syncedAt(t, st, store, "big.bin", body)

	dirty := localEdit(t, dir, "big.bin", body, testBlock, []byte("x"))
	e := New(Config{Store: store, DataDir: dir, State: st, Holes: allPlaceholders{}})
	if err := e.pushContent(context.Background(), "big.bin", dirty); err != nil {
		t.Fatal(err)
	}
	if got := store.ops(); len(got) != 0 {
		t.Fatalf("a placeholder must not be pushed by any route; got %v", got)
	}
}

type allPlaceholders struct{}

func (allPlaceholders) IsPlaceholder(string) bool { return true }

// --- coalescing extents across events ---------------------------------------

// Two debounced writes to one path union their extents into one push.
func TestCoalescerUnionsExtents(t *testing.T) {
	c := coalescer{pending: map[string]*ranges.Set{}}

	a := ranges.New(8*testBlock, ranges.DefaultBlockSize)
	a.MarkCovering(0, 1)
	b := ranges.New(8*testBlock, ranges.DefaultBlockSize)
	b.MarkCovering(5*testBlock, 1)

	c.markContent("f.bin", &a)
	c.markContent("f.bin", &b)

	got := c.flush("f.bin")
	if len(got) != 1 || got[0].Dirty == nil {
		t.Fatalf("flush = %+v; want one task carrying extents", got)
	}
	want := []ranges.Range{{Off: 0, Len: testBlock}, {Off: 5 * testBlock, Len: testBlock}}
	if ext := got[0].Dirty.Extents(); len(ext) != 2 || ext[0] != want[0] || ext[1] != want[1] {
		t.Fatalf("Extents() = %v; want %v", ext, want)
	}
}

// One unknown event poisons the whole coalesced push, in either order. This is
// the fail-safe direction: a push that forgets an extent corrupts the file, a
// push that sends too much only costs bandwidth.
func TestCoalescerUnknownAbsorbsExtents(t *testing.T) {
	known := ranges.New(8*testBlock, ranges.DefaultBlockSize)
	known.MarkCovering(0, 1)

	for _, order := range []string{"known first", "unknown first"} {
		t.Run(order, func(t *testing.T) {
			c := coalescer{pending: map[string]*ranges.Set{}}
			k := known.Clone()
			if order == "known first" {
				c.markContent("f.bin", &k)
				c.markContent("f.bin", nil)
			} else {
				c.markContent("f.bin", nil)
				c.markContent("f.bin", &k)
			}
			got := c.flush("f.bin")
			if len(got) != 1 {
				t.Fatalf("flush = %+v; want one task", got)
			}
			if got[0].Dirty != nil {
				t.Fatalf("Dirty = %v; want unknown (whole file)", got[0].Dirty.Extents())
			}
		})
	}
}

// Sets built on different block grids cannot be merged bit-for-bit, so the merge
// must decline and fall back to whole-file rather than mismark.
func TestCoalescerRefusesMismatchedGrids(t *testing.T) {
	c := coalescer{pending: map[string]*ranges.Set{}}

	a := ranges.New(8*testBlock, ranges.DefaultBlockSize)
	a.MarkCovering(0, 1)
	b := ranges.New(8*testBlock, 1<<20)
	b.MarkCovering(0, 1)

	c.markContent("f.bin", &a)
	c.markContent("f.bin", &b)

	got := c.flush("f.bin")
	if len(got) != 1 || got[0].Dirty != nil {
		t.Fatalf("flush = %+v; want one task with unknown extents", got)
	}
}

// The shutdown drain must carry extents through too, not silently downgrade
// every pending push to a whole-file upload.
func TestFlushAllPreservesExtents(t *testing.T) {
	c := coalescer{pending: map[string]*ranges.Set{}}
	a := ranges.New(8*testBlock, ranges.DefaultBlockSize)
	a.MarkCovering(0, 1)
	c.markContent("known.bin", &a)
	c.markContent("unknown.bin", nil)

	byPath := map[string]*ranges.Set{}
	for _, ev := range c.flushAll() {
		byPath[ev.Path] = ev.Dirty
	}
	if byPath["known.bin"] == nil {
		t.Error("extents were dropped by the drain")
	}
	if byPath["unknown.bin"] != nil {
		t.Error("an unknown push gained extents in the drain")
	}
}

// End to end through Run: a debounced write carries its extents all the way to
// the provider as a range write.
func TestRunDeliversExtentsToProvider(t *testing.T) {
	dir := t.TempDir()
	st := newState(t)
	body := bigFile(4)
	store := newPatchStore()
	syncedAt(t, st, store, "big.bin", body)
	dirty := localEdit(t, dir, "big.bin", body, testBlock, []byte("patched"))

	e := New(Config{Store: store, DataDir: dir, State: st, Debounce: 5 * time.Millisecond})
	events := make(chan fsevent.Event, 4)
	events <- fsevent.Event{Op: fsevent.OpWrite, Path: "big.bin", Dirty: dirty}
	close(events)

	e.Run(context.Background(), events)

	want := fmt.Sprintf("PutRange(big.bin,extents=1,bytes=%d)", testBlock)
	if got := store.ops(); len(got) != 1 || got[0] != want {
		t.Fatalf("ops = %v; want [%s]", got, want)
	}
}
