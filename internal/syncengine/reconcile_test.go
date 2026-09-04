package syncengine

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zishmusic/drivel/internal/fsevent"
	"github.com/zishmusic/drivel/internal/provider"
	"github.com/zishmusic/drivel/internal/state"
)

// --- fakes ------------------------------------------------------------------

// enumStore is a fakeStore that can also enumerate. Pages are handed out in
// order; the cursor is "page-N", so a test can resume mid-sweep the way a
// restarted process does.
type enumStore struct {
	*fakeStore
	pages   [][]provider.RemoteFile
	trace   *[]string
	cursors []string     // every cursor Enumerate was called with
	swept   atomic.Int32 // sweeps started; atomic so a test can watch Run make progress
}

func newEnumStore(trace *[]string, pages ...[]provider.RemoteFile) *enumStore {
	return &enumStore{fakeStore: newFakeStore(), pages: pages, trace: trace}
}

func (e *enumStore) Enumerate(_ context.Context, cursor string) ([]provider.RemoteFile, string, error) {
	if cursor == "" {
		e.swept.Add(1) // every sweep starts from the empty cursor
	}
	e.cursors = append(e.cursors, cursor)
	if e.trace != nil {
		*e.trace = append(*e.trace, "enumerate("+cursor+")")
	}
	i := 0
	if cursor != "" {
		fmt.Sscanf(cursor, "page-%d", &i)
	}
	if i >= len(e.pages) {
		return nil, "", nil
	}
	next := ""
	if i+1 < len(e.pages) {
		next = fmt.Sprintf("page-%d", i+1)
	}
	return e.pages[i], next, nil
}

// recSource is a change feed that records when its start token was taken — the
// ordering M7b hangs on — and can be scripted to fail the first poll.
type recSource struct {
	trace *[]string
	start string
	errs  []error
	polls atomic.Int32 // read by the test while Run polls
}

func (s *recSource) StartCursor(context.Context) (string, error) {
	if s.trace != nil {
		*s.trace = append(*s.trace, "startCursor")
	}
	return s.start, nil
}

func (s *recSource) Changes(_ context.Context, cursor string) ([]provider.RemoteChange, string, error) {
	s.polls.Add(1)
	if len(s.errs) > 0 {
		err := s.errs[0]
		s.errs = s.errs[1:]
		return nil, "", err
	}
	return nil, cursor, nil
}

// recPusher records what the reconciler asked the outbound path to do.
type recPusher struct {
	mu  sync.Mutex
	evs []fsevent.Event
}

func (p *recPusher) Push(_ context.Context, ev fsevent.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.evs = append(p.evs, ev)
}

func (p *recPusher) ops() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.evs))
	for _, ev := range p.evs {
		out = append(out, string(ev.Op)+" "+ev.Path)
	}
	sort.Strings(out)
	return out
}

// remote builds a listing entry with content-derived metadata.
func remote(path, content string) provider.RemoteFile {
	return provider.RemoteFile{Path: path, Size: int64(len(content)), Hash: md5hex(content), Version: "1"}
}

// sweepFixture wires a downloader with a reconciler over a temp backing dir.
type sweepFixture struct {
	dir   string
	st    *state.Store
	store *enumStore
	push  *recPusher
	dl    *Downloader
	trace []string
}

func newSweepFixture(t *testing.T, opts ReconcileOptions, pages ...[]provider.RemoteFile) *sweepFixture {
	t.Helper()
	f := &sweepFixture{dir: t.TempDir(), st: newState(t), push: &recPusher{}}
	f.store = newEnumStore(&f.trace, pages...)
	if opts.Push == nil {
		opts.Push = f.push
	}
	src := &recSource{trace: &f.trace, start: "token-before-sweep"}
	f.dl = NewDownloader(src, f.store, f.dir, f.st, DefaultCadence).Reconcile(opts)
	return f
}

// run performs the startup path: the sweep, then whatever cursor the pull loop
// should poll from.
func (f *sweepFixture) run(t *testing.T) string {
	t.Helper()
	cursor, err := f.dl.startFeed(context.Background())
	if err != nil {
		t.Fatalf("startFeed: %v", err)
	}
	return cursor
}

func (f *sweepFixture) exists(rel string) bool {
	_, err := os.Lstat(filepath.Join(f.dir, filepath.FromSlash(rel)))
	return err == nil
}

// --- ordering ---------------------------------------------------------------

// Snapshot, then tail: the start token is taken BEFORE the first listing and is
// what the pull loop resumes from afterwards. The other order silently loses
// every change made while the sweep was running.
func TestSweepTakesTokenBeforeEnumerating(t *testing.T) {
	f := newSweepFixture(t, ReconcileOptions{Fetch: true},
		[]provider.RemoteFile{remote("a.txt", "alpha")},
		[]provider.RemoteFile{remote("b.txt", "beta")},
	)
	f.store.content["a.txt"] = []byte("alpha")
	f.store.content["b.txt"] = []byte("beta")

	cursor := f.run(t)

	if len(f.trace) < 2 || f.trace[0] != "startCursor" || !strings.HasPrefix(f.trace[1], "enumerate") {
		t.Fatalf("call order = %v; want the start token before the first page", f.trace)
	}
	if cursor != "token-before-sweep" {
		t.Fatalf("cursor = %q; want the token taken before the sweep", cursor)
	}
	if persisted, ok, _ := f.st.Cursor(); !ok || persisted != "token-before-sweep" {
		t.Fatalf("persisted cursor = %q ok=%v; want the pre-sweep token", persisted, ok)
	}
	// And the sweep record is gone, so the next start tails instead of re-sweeping.
	if _, ok, _ := f.st.Sweep(); ok {
		t.Fatal("sweep record survived a completed sweep")
	}
}

// An interrupted sweep resumes from its persisted page cursor rather than
// starting over, and still installs the token its predecessor took.
func TestSweepResumesFromPersistedCursor(t *testing.T) {
	f := newSweepFixture(t, ReconcileOptions{Fetch: true},
		[]provider.RemoteFile{remote("a.txt", "alpha")},
		[]provider.RemoteFile{remote("b.txt", "beta")},
	)
	f.store.content["b.txt"] = []byte("beta")
	if err := f.st.SetSweep(state.Sweep{Gen: "g1", Token: "token-from-the-interrupted-run", Cursor: "page-1", Started: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}

	cursor := f.run(t)

	if got := f.store.cursors[0]; got != "page-1" {
		t.Fatalf("first Enumerate cursor = %q; want page-1 (resume, not restart)", got)
	}
	if cursor != "token-from-the-interrupted-run" {
		t.Fatalf("cursor = %q; want the token the interrupted run took", cursor)
	}
	if f.exists("a.txt") {
		t.Fatal("page 0 was re-applied; the sweep restarted instead of resuming")
	}
	if !f.exists("b.txt") {
		t.Fatal("page 1 was not applied")
	}
}

// --- materialisation --------------------------------------------------------

// The first-ever run: everything remote appears locally, everything local-only is
// pushed, and NOTHING is deleted on either side — every path is baseline-absent,
// so no absence means anything yet.
func TestFirstRunMaterialisesPushesAndDeletesNothing(t *testing.T) {
	f := newSweepFixture(t, ReconcileOptions{Fetch: true},
		[]provider.RemoteFile{remote("remote.txt", "from drive")},
	)
	f.store.content["remote.txt"] = []byte("from drive")
	writeFile(t, f.dir, "local.txt", "made here")

	f.run(t)

	if got, ok := readBacking(t, f.dir, "remote.txt"); !ok || got != "from drive" {
		t.Fatalf("remote file not materialised: %q ok=%v", got, ok)
	}
	if !f.exists("local.txt") {
		t.Fatal("local-only file was deleted on a first run")
	}
	if got, want := f.push.ops(), []string{"write local.txt"}; !equal(got, want) {
		t.Fatalf("pushes = %v; want %v", got, want)
	}
}

// In eager mode, materialising means downloading the whole remote tree — which is
// a decision, not a side effect of mounting. Without -materialize the sweep still
// indexes and baselines nothing it did not fetch.
func TestEagerSweepDoesNotDownloadWithoutMaterialize(t *testing.T) {
	f := newSweepFixture(t, ReconcileOptions{}, []provider.RemoteFile{remote("big.bin", "payload")})
	f.store.content["big.bin"] = []byte("payload")

	f.run(t)

	if f.exists("big.bin") {
		t.Fatal("eager sweep downloaded a remote file without -materialize")
	}
	if _, ok, _ := f.st.GetEcho("big.bin"); ok {
		t.Fatal("an echo was recorded for content we never fetched; the next sweep would read it as deleted locally and delete it remotely")
	}
}

// In lazy mode there is no such cost: a placeholder is metadata, so the whole
// remote tree becomes visible for the price of the sweep.
func TestLazySweepMaterialisesPlaceholders(t *testing.T) {
	f := newSweepFixture(t, ReconcileOptions{}, []provider.RemoteFile{remote("doc.txt", "body")})
	hyd := newFakeHydrator()
	f.dl = f.dl.Lazy(hyd)

	f.run(t)

	if got := hyd.stampedPaths(); len(got) != 1 || got[0] != "doc.txt" {
		t.Fatalf("placeholders = %v; want [doc.txt]", got)
	}
}

// A Google-native doc has no byte stream: nothing to place locally. It is still
// marked seen, so it never looks remotely deleted, and gets no echo, because we
// hold none of its content.
func TestSweepSkipsGoogleNativeDocs(t *testing.T) {
	doc := provider.RemoteFile{Path: "notes", ExportOnly: true, Version: "7"}
	f := newSweepFixture(t, ReconcileOptions{Fetch: true}, []provider.RemoteFile{doc})
	// A baseline from an earlier session, older than this sweep.
	if err := f.st.SetEcho("notes", state.Echo{Version: "7", At: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, f.dir, "notes", "stale local rendering")

	f.run(t)

	if !f.exists("notes") {
		t.Fatal("a Google-native doc that is still on Drive was deleted locally")
	}
	if ops := f.push.ops(); len(ops) != 0 {
		t.Fatalf("pushes = %v; want none for an export-only object", ops)
	}
}

// --- deletions --------------------------------------------------------------

// Baseline present, gone remotely, local copy untouched since: the remote lost
// it while we were down, and the local bytes are the ones the baseline describes,
// so deleting them loses nothing.
func TestReconcileDeletesLocalWhenRemoteIsGone(t *testing.T) {
	f := newSweepFixture(t, ReconcileOptions{Force: true, Fetch: true})
	writeFile(t, f.dir, "gone.txt", "synced body")
	seedBaseline(t, f.st, "gone.txt", "synced body")

	f.run(t)

	if f.exists("gone.txt") {
		t.Fatal("a previously-synced file the remote no longer has was kept")
	}
	if _, ok, _ := f.st.GetEcho("gone.txt"); ok {
		t.Fatal("baseline survived the local delete")
	}
	if ops := f.push.ops(); len(ops) != 0 {
		t.Fatalf("pushes = %v; want none — the remote already lost the file", ops)
	}
}

// The same shape, but the local copy has changed since the baseline. Those bytes
// are now the only version of that work anywhere, and no inference is worth
// losing them: keep the file and push it back.
func TestReconcileKeepsLocallyModifiedFileWhoseRemoteIsGone(t *testing.T) {
	f := newSweepFixture(t, ReconcileOptions{Force: true, Fetch: true})
	writeFile(t, f.dir, "edited.txt", "edited while offline")
	seedBaseline(t, f.st, "edited.txt", "the version we synced")

	f.run(t)

	if !f.exists("edited.txt") {
		t.Fatal("a locally-modified file was deleted because the remote lost it")
	}
	if got, want := f.push.ops(), []string{"write edited.txt"}; !equal(got, want) {
		t.Fatalf("pushes = %v; want %v (the kept file goes back up)", got, want)
	}
}

// Baseline present, gone remotely, gone locally: the user deleted it here while
// we were down, so the deletion propagates to the remote.
func TestReconcileDeletesRemoteWhenLocalIsGone(t *testing.T) {
	f := newSweepFixture(t, ReconcileOptions{Force: true, Fetch: true})
	seedBaseline(t, f.st, "removed.txt", "was synced")

	f.run(t)

	if got, want := f.push.ops(), []string{"unlink removed.txt"}; !equal(got, want) {
		t.Fatalf("pushes = %v; want %v", got, want)
	}
}

// A file created locally *while the sweep was running* has a baseline younger
// than the sweep and is missing from the seen-set for that reason alone. Deleting
// it would destroy something the user just made.
func TestReconcileIgnoresBaselineWrittenDuringTheSweep(t *testing.T) {
	f := newSweepFixture(t, ReconcileOptions{Force: true, Fetch: true})
	writeFile(t, f.dir, "just-made.txt", "fresh")
	// Written after the sweep started (the uploader ran during it).
	if err := f.st.SetEcho("just-made.txt", state.Echo{Hash: md5hex("fresh"), At: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	f.run(t)

	if !f.exists("just-made.txt") {
		t.Fatal("a file pushed during the sweep was deleted by the sweep's own delete pass")
	}
	if ops := f.push.ops(); len(ops) != 0 {
		t.Fatalf("pushes = %v; want none", ops)
	}
}

// A huge delete count is the signature of a broken premise — a state DB reused
// against another root, a fresh empty backing dir — not of a huge deletion. The
// whole pass is abandoned rather than trimmed.
func TestReconcileRefusesToDeleteOverTheLimit(t *testing.T) {
	f := newSweepFixture(t, ReconcileOptions{Force: true, Fetch: true, MaxDeletes: 3})
	for i := 0; i < 4; i++ {
		rel := fmt.Sprintf("f%d.txt", i)
		writeFile(t, f.dir, rel, "body")
		seedBaseline(t, f.st, rel, "body")
	}

	f.run(t)

	for i := 0; i < 4; i++ {
		if !f.exists(fmt.Sprintf("f%d.txt", i)) {
			t.Fatalf("f%d.txt was deleted despite exceeding -max-deletes", i)
		}
	}
	if ops := f.push.ops(); len(ops) != 0 {
		t.Fatalf("pushes = %v; want none — the delete pass was refused wholesale", ops)
	}
}

// A directory whose remote counterpart is gone is removed only if it is empty:
// its children are candidates in their own right, and anything unaccounted for
// must survive.
func TestReconcileWillNotRemoveANonEmptyDirectory(t *testing.T) {
	f := newSweepFixture(t, ReconcileOptions{Force: true, Fetch: true})
	writeFile(t, f.dir, "dir/keep.txt", "not accounted for")
	if err := f.st.SetEcho("dir", state.Echo{Hash: "", At: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}

	f.run(t)

	if !f.exists("dir/keep.txt") {
		t.Fatal("a local-only file was destroyed by removing its directory")
	}
	if !f.exists("dir") {
		t.Fatal("non-empty directory removed")
	}
}

// --- the local walk ---------------------------------------------------------

// §6 conflict copies are local-only by policy. The walk must not discover them
// and helpfully upload the losing side of every conflict ever resolved. Drivel's
// own temp files are skipped for the same reason.
func TestLocalWalkSkipsConflictCopiesAndTempFiles(t *testing.T) {
	f := newSweepFixture(t, ReconcileOptions{Force: true, Fetch: true})
	writeFile(t, f.dir, "a (conflict 2026-01-02 03-04-05).txt", "the losing side")
	writeFile(t, f.dir, "sub/b (conflict 2026-01-02 03-04-05)", "extensionless")
	writeFile(t, f.dir, ".drivel-tmp-123", "half a download")
	writeFile(t, f.dir, "real.txt", "genuine local file")

	f.run(t)

	if got, want := f.push.ops(), []string{"write real.txt"}; !equal(got, want) {
		t.Fatalf("pushes = %v; want %v", got, want)
	}
}

// A placeholder is remote-born by definition, so the walk must never treat one as
// a local-only file — pushing it is the M5 catastrophe.
func TestLocalWalkNeverPushesAPlaceholder(t *testing.T) {
	f := newSweepFixture(t, ReconcileOptions{Force: true})
	f.dl = f.dl.Lazy(newFakeHydrator("orphan.txt"))
	writeFile(t, f.dir, "orphan.txt", "")

	f.run(t)

	if ops := f.push.ops(); len(ops) != 0 {
		t.Fatalf("pushes = %v; want none — that file is a placeholder", ops)
	}
}

// --- cursor expiry ----------------------------------------------------------

// A dead cursor is not retryable: the changes it covered are gone. The recovery
// is a resync, which takes a fresh token BEFORE re-enumerating, exactly as a first
// run does.
func TestExpiredCursorTriggersResync(t *testing.T) {
	f := newSweepFixture(t, ReconcileOptions{Fetch: true}, []provider.RemoteFile{remote("a.txt", "alpha")})
	f.store.content["a.txt"] = []byte("alpha")
	if err := f.st.SetCursor("dead-token"); err != nil {
		t.Fatal(err)
	}

	cursor, err := f.dl.recoverCursor(context.Background())
	if err != nil {
		t.Fatalf("recoverCursor: %v", err)
	}
	if cursor != "token-before-sweep" {
		t.Fatalf("cursor = %q; want a fresh token", cursor)
	}
	if !f.exists("a.txt") {
		t.Fatal("expiry recovery did not re-enumerate")
	}
	if len(f.trace) < 2 || f.trace[0] != "startCursor" || !strings.HasPrefix(f.trace[1], "enumerate") {
		t.Fatalf("call order = %v; want the fresh token taken before the sweep", f.trace)
	}
}

// Without an enumerator there is nothing to resync from, but a dead cursor still
// must not stall inbound sync forever: restart the feed from now and say what was
// lost.
func TestExpiredCursorWithoutEnumeratorRestartsTheFeed(t *testing.T) {
	st := newState(t)
	src := &recSource{start: "fresh-token"}
	d := NewDownloader(src, newFakeStore(), t.TempDir(), st, DefaultCadence)

	cursor, err := d.recoverCursor(context.Background())
	if err != nil || cursor != "fresh-token" {
		t.Fatalf("recoverCursor = %q, %v; want fresh-token", cursor, err)
	}
	if got, ok, _ := st.Cursor(); !ok || got != "fresh-token" {
		t.Fatalf("persisted cursor = %q ok=%v; want fresh-token", got, ok)
	}
}

// The pull loop recovers in place: it swaps in the resynced cursor and keeps
// polling, instead of retrying a token that can never answer again.
func TestRunRecoversFromAnExpiredCursor(t *testing.T) {
	f := newSweepFixture(t, ReconcileOptions{Fetch: true}, []provider.RemoteFile{remote("a.txt", "alpha")})
	f.store.content["a.txt"] = []byte("alpha")
	if err := f.st.SetCursor("dead-token"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	src := &recSource{trace: &f.trace, start: "token-before-sweep", errs: []error{provider.ErrCursorExpired}}
	d := NewDownloader(src, f.store, f.dir, f.st, Cadence{Fast: time.Millisecond, Slow: 2 * time.Millisecond}).
		Reconcile(ReconcileOptions{Push: f.push, Fetch: true})
	go func() {
		for src.polls.Load() < 3 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	d.Run(ctx)

	if !f.exists("a.txt") {
		t.Fatal("the loop did not resync after the cursor expired")
	}
	if cur, _, _ := f.st.Cursor(); cur != "token-before-sweep" {
		t.Fatalf("cursor = %q; want the post-resync token", cur)
	}
}

// --- helpers ----------------------------------------------------------------

// seedBaseline records that we previously synced exactly this content at rel,
// with a timestamp older than any sweep the test will start.
func seedBaseline(t *testing.T, st *state.Store, rel, content string) {
	t.Helper()
	if err := st.SetEcho(rel, state.Echo{Hash: md5hex(content), Version: "1", At: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
}

func equal(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// The reconciler pushes through the Engine, not around it, so a file a sweep
// discovers takes the same path as one written through the mount — including the
// M5 placeholder guard and the echo record.
var _ Pusher = (*Engine)(nil)

func TestReconcilePushesThroughTheEngine(t *testing.T) {
	f := newSweepFixture(t, ReconcileOptions{Force: true, Fetch: true})
	engine := New(Config{Store: f.store, DataDir: f.dir, State: f.st})
	f.dl.rec.Push = engine
	writeFile(t, f.dir, "local.txt", "made here")
	seedBaseline(t, f.st, "removed.txt", "was synced")

	f.run(t)

	got := append([]string(nil), f.store.calls...)
	sort.Strings(got)
	want := []string{"Put(create,local.txt,bytes=9)", "Remove(removed.txt)"}
	if !equal(got, want) {
		t.Fatalf("store calls = %v; want %v", got, want)
	}
	// The engine recorded what it pushed, so the change feed will recognise its
	// own echo when Drive reports it back.
	if _, ok, _ := f.st.GetEcho("local.txt"); !ok {
		t.Fatal("no echo recorded for the reconcile-triggered push")
	}
}

// --- state records ----------------------------------------------------------

// A sweep with nothing wired to push still has to drop the baseline for a path
// that is gone locally and unlisted remotely. Keeping it made that path a delete
// candidate on every subsequent sweep, forever — nothing else would ever resolve
// it, so the record leaked for the life of the state DB.
func TestReconcileDropsTheBaselineWhenNothingCanPush(t *testing.T) {
	f := newSweepFixture(t, ReconcileOptions{Force: true, Fetch: true})
	f.dl.rec.Push = nil
	seedBaseline(t, f.st, "removed.txt", "was synced")

	f.run(t)

	if _, ok, err := f.st.GetEcho("removed.txt"); err != nil || ok {
		t.Fatalf("baseline still present after the sweep (ok=%v, err=%v)", ok, err)
	}
	if got := f.push.ops(); len(got) != 0 {
		t.Fatalf("pushes = %v; want none (nothing is wired to push)", got)
	}
}

// The kept-and-pushed row drops its baseline inline, because the push that
// follows writes a fresh one for the same path. Verify the drop actually happens:
// a stale baseline here is what the next sweep reads as evidence of a delete.
func TestReconcileDropsTheStaleBaselineOfAKeptFile(t *testing.T) {
	f := newSweepFixture(t, ReconcileOptions{Force: true, Fetch: true})
	writeFile(t, f.dir, "edited.txt", "changed since")
	seedBaseline(t, f.st, "edited.txt", "was synced")

	f.run(t)

	if !f.exists("edited.txt") {
		t.Fatal("the locally modified file was deleted")
	}
	if got, want := f.push.ops(), []string{"write edited.txt"}; !equal(got, want) {
		t.Fatalf("pushes = %v; want %v", got, want)
	}
	if _, ok, err := f.st.GetEcho("edited.txt"); err != nil || ok {
		t.Fatalf("stale baseline still present (ok=%v, err=%v)", ok, err)
	}
}

// --- the periodic sweep -----------------------------------------------------

// stampSweep backdates the last completed sweep, which is what the schedule reads.
func stampSweep(t *testing.T, st *state.Store, ago time.Duration) {
	t.Helper()
	if err := st.FinishSweep(time.Now().Add(-ago)); err != nil {
		t.Fatal(err)
	}
}

// A live cursor is no longer reason enough to skip the sweep: the feed cannot
// report what happened while drivel was not running, so an interval past the last
// completed sweep means enumerate again.
func TestPeriodicSweepIsDueFromTheLastCompletedSweep(t *testing.T) {
	f := newSweepFixture(t, ReconcileOptions{Fetch: true, Interval: time.Hour},
		[]provider.RemoteFile{remote("a.txt", "alpha")})
	f.store.content["a.txt"] = []byte("alpha")
	if err := f.st.SetCursor("live-token"); err != nil {
		t.Fatal(err)
	}
	stampSweep(t, f.st, 2*time.Hour)

	if cursor := f.run(t); cursor != "token-before-sweep" {
		t.Fatalf("cursor = %q; want the token taken before the periodic sweep", cursor)
	}
	if int(f.store.swept.Load()) != 1 {
		t.Fatalf("sweeps = %d; want 1", int(f.store.swept.Load()))
	}
	if !f.exists("a.txt") {
		t.Fatal("the periodic sweep did not reconcile the remote tree")
	}
}

// Inside the interval the cursor is used as-is. A sweep on every mount would make
// short-lived mounts re-enumerate the whole tree every time.
func TestPeriodicSweepIsNotDueWithinTheInterval(t *testing.T) {
	f := newSweepFixture(t, ReconcileOptions{Fetch: true, Interval: time.Hour},
		[]provider.RemoteFile{remote("a.txt", "alpha")})
	if err := f.st.SetCursor("live-token"); err != nil {
		t.Fatal(err)
	}
	stampSweep(t, f.st, time.Minute)

	if cursor := f.run(t); cursor != "live-token" {
		t.Fatalf("cursor = %q; want the stored one", cursor)
	}
	if int(f.store.swept.Load()) != 0 {
		t.Fatalf("sweeps = %d; want 0 — the interval had not elapsed", int(f.store.swept.Load()))
	}
}

// No completion stamp means no sweep has ever finished against this state DB —
// including every DB written before the stamp existed. That is the case the
// interval is for, so it counts as overdue.
func TestPeriodicSweepIsDueWhenNoneHasEverCompleted(t *testing.T) {
	f := newSweepFixture(t, ReconcileOptions{Fetch: true, Interval: time.Hour},
		[]provider.RemoteFile{remote("a.txt", "alpha")})
	f.store.content["a.txt"] = []byte("alpha")
	if err := f.st.SetCursor("live-token"); err != nil {
		t.Fatal(err)
	}

	f.run(t)

	if int(f.store.swept.Load()) != 1 {
		t.Fatalf("sweeps = %d; want 1", int(f.store.swept.Load()))
	}
}

// ...but only when the interval is on. With it disabled, a state DB with no stamp
// behaves exactly as it did before the feature existed.
func TestZeroIntervalNeverSweepsPeriodically(t *testing.T) {
	f := newSweepFixture(t, ReconcileOptions{Fetch: true},
		[]provider.RemoteFile{remote("a.txt", "alpha")})
	if err := f.st.SetCursor("live-token"); err != nil {
		t.Fatal(err)
	}

	if cursor := f.run(t); cursor != "live-token" {
		t.Fatalf("cursor = %q; want the stored one", cursor)
	}
	if int(f.store.swept.Load()) != 0 {
		t.Fatalf("sweeps = %d; want 0 — no interval is configured", int(f.store.swept.Load()))
	}
}

// The completion stamp is written by the sweep itself, in the same commit that
// clears the in-progress record.
func TestSweepRecordsItsCompletion(t *testing.T) {
	f := newSweepFixture(t, ReconcileOptions{Fetch: true},
		[]provider.RemoteFile{remote("a.txt", "alpha")})
	f.store.content["a.txt"] = []byte("alpha")
	before := time.Now()

	f.run(t)

	at, ok, err := f.st.SweepDone()
	if err != nil || !ok {
		t.Fatalf("SweepDone = ok %v, err %v; want a stamp", ok, err)
	}
	if at.Before(before.Truncate(time.Second)) {
		t.Fatalf("SweepDone = %s; want a time at or after %s", at, before)
	}
}

// A mount that never restarts is exactly the process startFeed's checks cannot
// help, so the poll loop re-sweeps on the interval too.
func TestRunSweepsOnTheInterval(t *testing.T) {
	dir, st := t.TempDir(), newState(t)
	store := newEnumStore(nil, []provider.RemoteFile{remote("a.txt", "alpha")})
	store.content["a.txt"] = []byte("alpha")
	src := &recSource{start: "token-before-sweep"}
	d := NewDownloader(src, store, dir, st, Cadence{Fast: time.Millisecond, Slow: 2 * time.Millisecond}).
		Reconcile(ReconcileOptions{Push: &recPusher{}, Fetch: true, Interval: time.Millisecond})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() {
		for store.swept.Load() < 3 && ctx.Err() == nil {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	d.Run(ctx)

	if n := int(store.swept.Load()); n < 3 {
		t.Fatalf("sweeps = %d; want the poll loop to have re-swept at least twice after the first", n)
	}
}

// --- the delete cap ---------------------------------------------------------

// captureLog redirects the standard logger for the duration of one test.
func captureLog(t *testing.T, w io.Writer) {
	t.Helper()
	prev := log.Writer()
	log.SetOutput(w)
	t.Cleanup(func() { log.SetOutput(prev) })
}

// Past the cap the walk stops retaining candidates but keeps counting, so the
// refusal names the real total. That number is the whole point of the message: it
// is what tells an operator whether they are looking at 10 genuine deletions or a
// state DB pointed at the wrong tree, and "more than 3" answers neither.
func TestRefusalReportsTheTrueCandidateCount(t *testing.T) {
	f := newSweepFixture(t, ReconcileOptions{Force: true, Fetch: true, MaxDeletes: 3})
	for i := 0; i < 10; i++ {
		rel := fmt.Sprintf("f%d.txt", i)
		writeFile(t, f.dir, rel, "body")
		seedBaseline(t, f.st, rel, "body")
	}
	var logs bytes.Buffer
	captureLog(t, &logs)

	f.run(t)

	if !strings.Contains(logs.String(), "10 previously-synced path(s)") {
		t.Fatalf("refusal did not count past the cap:\n%s", logs.String())
	}
	for i := 0; i < 10; i++ {
		if !f.exists(fmt.Sprintf("f%d.txt", i)) {
			t.Fatalf("f%d.txt was deleted despite the refusal", i)
		}
	}
}

// A candidate count at the cap is still applied; only exceeding it refuses.
func TestDeletesExactlyAtTheCapStillRun(t *testing.T) {
	f := newSweepFixture(t, ReconcileOptions{Force: true, Fetch: true, MaxDeletes: 3})
	for i := 0; i < 3; i++ {
		rel := fmt.Sprintf("f%d.txt", i)
		writeFile(t, f.dir, rel, "body")
		seedBaseline(t, f.st, rel, "body")
	}

	f.run(t)

	for i := 0; i < 3; i++ {
		if f.exists(fmt.Sprintf("f%d.txt", i)) {
			t.Fatalf("f%d.txt survived a delete pass within the cap", i)
		}
	}
}
