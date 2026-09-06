package app

import (
	"context"
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zishmusic/drivel/internal/provider"
	"github.com/zishmusic/drivel/internal/state"
	"github.com/zishmusic/drivel/internal/testenv"
)

// Several drivel instances against one remote — the shape docs/multiclient-test-plan.md
// calls Tier A.
//
// What only a fleet can show: §4 echo suppression is per client, so a push by one
// peer is an echo to itself and a genuine remote change to every other. Every
// single-client test in the tree either drives the push side or the pull side with
// the other one idle; none of them can observe a change making the whole round
// trip through a *second* state machine and stopping there.
//
// The fake provider is memStore (memstore_test.go), which the end-to-end test
// already uses. Everything here is deterministic and needs no network; the real
// three-process rig (Tier B) is the plan's other half and lives in
// scripts/multiclient.

const (
	// mountProbe is written into a peer's backing dir and then looked for through
	// its mountpoint. Readiness has to be observed rather than assumed: until the
	// mount is live the mountpoint is an ordinary directory and every assertion
	// would pass against nothing. The ".drivel-" prefix is the one reconcile.skip
	// already ignores, so the probe can never become test data.
	mountProbe = ".drivel-mount-probe"

	// fanoutWait bounds one change reaching every other peer: a 300ms debounce,
	// then the 2s fast poll, then the apply. Generous, because a -race build on a
	// loaded machine is not a quiet one.
	fanoutWait = 30 * time.Second

	// quietWait is how long "nothing further happened" is observed for. It must
	// exceed the slow end of the poll cadence (30s) to be meaningful about the
	// feed, but these fleets are never idle long enough to back off that far, so
	// several fast polls is the honest bound.
	quietWait = 8 * time.Second
)

// testLog routes a mount's output through the test log, so a passing run is quiet
// and a failing one carries every peer's side of the story. Writes are dropped
// once the test has finished: an engine draining on a detached context can log
// after its peer has been stopped, and t.Logf after the test ends panics.
type testLog struct {
	mu    sync.Mutex
	t     *testing.T
	done  bool
	lines []string
}

func (w *testLog) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lines = append(w.lines, string(b))
	if !w.done {
		w.t.Logf("%s", b)
	}
	return len(b), nil
}

// saw reports whether this mount has ever logged a line containing sub.
//
// Asserting on log text is usually a way to pin an implementation detail, and it
// is used here for the one case where the log IS the behaviour: a refusal that
// deliberately does nothing is indistinguishable from a sweep that found nothing
// to do, except by what it told the operator.
func (w *testLog) saw(sub string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, l := range w.lines {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

func (w *testLog) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.done = true
}

// fleet is N mounts sharing one provider, each with its own backing dir, state DB
// and engine — the in-process model of N machines syncing one Drive folder.
type fleet struct {
	t     *testing.T
	store *memStore
	peers []*peer
	logs  []*testLog
}

// peer is one client in the fleet, startable and stoppable on its own. Restarting
// exactly one peer is what the offline cases need, which is why this is a set of
// independent Mounts rather than one App: App deliberately runs its mounts under a
// single cancellation, which is right for a process and wrong for a test that has
// to take one machine away.
type peer struct {
	t     *testing.T
	name  string
	spec  MountSpec
	reg   *provider.Registry
	store *memStore
	logw  *testLog

	mount   *Mount
	cancel  func()
	done    chan error
	running bool
}

// newFleet opens and starts n peers against one memStore. Each opt is applied to
// every peer's spec after the defaults below, for the cases that need a knob the
// fleet does not otherwise vary; a peer's spec can also be changed between a stop
// and a start, which is what "re-run with a different flag" looks like here.
func newFleet(t *testing.T, n int, opts ...func(*MountSpec)) *fleet {
	t.Helper()
	if _, err := os.Stat("/dev/fuse"); err != nil {
		testenv.Unavailable(t, testenv.FUSE, "no /dev/fuse: "+err.Error())
	}

	store := newMemStore()
	root := t.TempDir()
	reg := registryWith(t, "mem", store)

	f := &fleet{t: t, store: store}
	specs := make([]MountSpec, 0, n)
	for i := range n {
		name := "c" + string(rune('1'+i))
		dir := filepath.Join(root, name)
		spec := MountSpec{
			Name:       name,
			Mountpoint: filepath.Join(dir, "mnt"),
			DataDir:    filepath.Join(dir, "data"),
			StateDB:    filepath.Join(dir, "state.db"),
			Provider:   "mem",
			// Eager mode, and a sweep that finds remote-only content: a peer that
			// joins an already-populated remote is the normal case in a fleet.
			Materialize: true,
			// Off unless a case asks for it. The periodic sweep is the most
			// disruptive scheduled event in the system, and a test that did not ask
			// for one must not have one fire in the middle of its assertions.
			SweepInterval: 0,
			// Per-mount logging is M8's rule and a fleet is where it earns its keep:
			// with three peers interleaving, an unprefixed line does not say whose
			// engine produced it. Routed through t so a passing run stays quiet.
			Logger: log.New(&testLog{t: t}, name+": ", 0),
		}
		for _, opt := range opts {
			opt(&spec)
		}
		specs = append(specs, spec)
		w := spec.Logger.Writer().(*testLog)
		f.logs = append(f.logs, w)
		f.peers = append(f.peers, &peer{t: t, name: name, spec: spec, reg: reg, store: store, logw: w})
	}

	// The rig checks itself with the same guards a real process uses. A fleet whose
	// peers overlap would produce failures that look like sync bugs.
	if err := Validate(specs); err != nil {
		t.Fatalf("fleet specs are not a valid mount set: %v", err)
	}

	for _, p := range f.peers {
		p.start()
	}
	t.Cleanup(f.stop)
	return f
}

func (f *fleet) stop() {
	for _, p := range f.peers {
		p.stop()
	}
	for _, w := range f.logs {
		w.close()
	}
}

// running is the peers currently serving; a stopped peer is not expected to agree
// with anyone.
func (f *fleet) running() []*peer {
	var out []*peer
	for _, p := range f.peers {
		if p.running {
			out = append(out, p)
		}
	}
	return out
}

func (p *peer) start() {
	p.t.Helper()
	if p.running {
		return
	}
	m, err := Open(p.t.Context(), p.spec, p.reg)
	if err != nil {
		p.t.Fatalf("%s: Open: %v", p.name, err)
	}
	ctx, cancel := context.WithCancel(p.t.Context())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	p.mount, p.cancel, p.done, p.running = m, cancel, done, true

	if err := os.WriteFile(p.data(mountProbe), []byte("x"), 0o644); err != nil {
		p.t.Fatalf("%s: probe: %v", p.name, err)
	}
	if !waitFor(15*time.Second, func() bool {
		_, err := os.Stat(p.mnt(mountProbe))
		return err == nil
	}) {
		p.stop()
		testenv.Unavailable(p.t, testenv.FUSE, p.name+": mount did not come up within 15s")
	}
}

// stop unmounts and drains this peer, leaving its backing dir and state DB intact
// so it can be started again — which is what "the laptop was closed" looks like
// from the remote's side.
func (p *peer) stop() {
	if !p.running {
		return
	}
	p.cancel()
	select {
	case err := <-p.done:
		if err != nil {
			p.t.Errorf("%s: Run: %v", p.name, err)
		}
	case <-time.After(30 * time.Second):
		p.t.Errorf("%s: Run did not return; try: fusermount3 -u %s", p.name, p.spec.Mountpoint)
	}
	if err := p.mount.Close(); err != nil {
		p.t.Errorf("%s: Close: %v", p.name, err)
	}
	p.mount, p.cancel, p.done, p.running = nil, nil, nil, false
}

// mnt is a path through this peer's mountpoint: what a user touches.
func (p *peer) mnt(rel string) string {
	return filepath.Join(p.spec.Mountpoint, filepath.FromSlash(rel))
}

// data is a path in this peer's backing dir: where sync writes land. Assertions
// read here rather than through the mount, because reading through the mount in
// lazy mode hydrates the placeholder it was about to measure.
func (p *peer) data(rel string) string {
	return filepath.Join(p.spec.DataDir, filepath.FromSlash(rel))
}

// write creates rel through the mountpoint, as a user would.
func (p *peer) write(rel, body string) {
	p.t.Helper()
	if dir := filepath.Dir(p.mnt(rel)); dir != p.spec.Mountpoint {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			p.t.Fatalf("%s: mkdir for %s: %v", p.name, rel, err)
		}
	}
	if err := os.WriteFile(p.mnt(rel), []byte(body), 0o644); err != nil {
		p.t.Fatalf("%s: write %s: %v", p.name, rel, err)
	}
}

// read returns rel's content from the backing dir.
func (p *peer) read(rel string) (string, bool) {
	b, err := os.ReadFile(p.data(rel))
	if err != nil {
		return "", false
	}
	return string(b), true
}

// manifest is what this peer holds: root-relative path -> content digest, using
// the provider's own digest so a local tree and the remote are directly
// comparable.
//
// Directories are excluded — whether an empty one exists on both sides is not what
// any of these cases is about. Conflict copies are excluded too, and that is not a
// convenience: §6 makes them local-only by policy, so a fleet that has resolved a
// conflict is *supposed* to disagree about them. A convergence check that counted
// them would report every conflict case as a failure.
// It samples a tree that is being written to. Both walkers here run while the
// fleet is still converging, so the pull loop can unlink a file between the
// readdir that listed it and the read that would hash it — which is what made
// TestFleetPropagatesABulkDelete fail roughly two runs in three. A vanished entry
// is therefore skipped rather than fatal: from outside, an entry that disappears
// mid-walk is exactly what "not converged yet" looks like, the caller is polling,
// and the path's absence from this manifest is itself the disagreement it is
// waiting to stop seeing. Nothing is weakened by this — every other error still
// fails the test, so it tolerates the race and not an unreadable backing store.
func (p *peer) manifest() map[string]string {
	p.t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(p.spec.DataDir, func(path string, e os.DirEntry, err error) error {
		if err != nil {
			return skipVanished(err)
		}
		if e.IsDir() {
			return nil
		}
		base := e.Name()
		if strings.HasPrefix(base, ".drivel-") || isConflictCopy(base) {
			return nil
		}
		rel, err := filepath.Rel(p.spec.DataDir, path)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return skipVanished(err)
		}
		out[filepath.ToSlash(rel)] = hashOf(b)
		return nil
	})
	if err != nil {
		p.t.Fatalf("%s: walking the backing dir: %v", p.name, err)
	}
	return out
}

// skipVanished turns "that file is gone" into "carry on" and leaves every other
// error alone. Returning nil for a directory's error stops WalkDir descending into
// it and continues with its siblings, which is the right answer for a directory
// the fleet has just deleted.
func skipVanished(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// conflicts lists the §6 conflict copies this peer holds.
func (p *peer) conflicts() []string {
	p.t.Helper()
	var out []string
	err := filepath.WalkDir(p.spec.DataDir, func(path string, e os.DirEntry, err error) error {
		if err != nil {
			return skipVanished(err) // same live-tree race as manifest
		}
		if e.IsDir() {
			return nil
		}
		if isConflictCopy(e.Name()) {
			rel, rErr := filepath.Rel(p.spec.DataDir, path)
			if rErr != nil {
				return rErr
			}
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		p.t.Fatalf("%s: walking the backing dir: %v", p.name, err)
	}
	sort.Strings(out)
	return out
}

// isConflictCopy matches what syncengine.conflictName produces. Kept as a
// substring test rather than a copy of the engine's regexp: this is the test
// asserting what a user would recognise, not a second implementation of it.
func isConflictCopy(base string) bool { return strings.Contains(base, " (conflict ") }

// waitConverged blocks until every running peer's manifest matches every other's
// and the remote's, or fails the test with the first disagreement it found.
//
// Agreement between clients is necessary but not sufficient — three peers can
// agree on something the remote does not hold — so the remote is one of the
// parties to the comparison, not the referee.
func (f *fleet) waitConverged(d time.Duration) {
	f.t.Helper()
	var why string
	ok := waitFor(d, func() bool {
		why = f.disagreement()
		return why == ""
	})
	if !ok {
		f.t.Fatalf("fleet did not converge within %s: %s", d, why)
	}
	// Convergence has to hold, not merely occur: a fleet caught mid-ping-pong
	// passes a single sample every time the two sides happen to agree.
	settle(time.Second)
	if why := f.disagreement(); why != "" {
		f.t.Fatalf("fleet converged and then diverged again: %s", why)
	}
}

// disagreement returns "" when everything agrees, else the first difference found.
func (f *fleet) disagreement() string {
	remote := f.store.manifest()
	for _, p := range f.running() {
		local := p.manifest()
		for path, want := range remote {
			got, ok := local[path]
			if !ok {
				return p.name + " is missing " + path
			}
			if got != want {
				return p.name + " holds different content at " + path
			}
		}
		for path := range local {
			if _, ok := remote[path]; !ok {
				return p.name + " holds " + path + ", which the remote does not"
			}
		}
	}
	return ""
}

// counts is every put and get the provider has served, so a test can assert that
// a number stopped moving.
func (f *fleet) counts() (puts, gets int) {
	return f.store.totalPuts(), f.store.totalGets()
}

// --- MC-03: one write reaches every peer, and comes back to nobody ------------

func TestFleetFansOneWriteOutToEveryPeer(t *testing.T) {
	f := newFleet(t, 3)

	f.peers[0].write("notes/hello.txt", "written on c1")
	f.waitConverged(fanoutWait)

	if got, _ := f.peers[1].read("notes/hello.txt"); got != "written on c1" {
		t.Errorf("c2 holds %q", got)
	}
	if got, _ := f.peers[2].read("notes/hello.txt"); got != "written on c1" {
		t.Errorf("c3 holds %q", got)
	}

	// The cost of one change in a fleet of N: one upload and N-1 downloads. Both
	// halves matter. More than one put means a peer pushed back what it pulled —
	// §4 failing on the way up. More than N-1 gets means the writer downloaded its
	// own upload — §4 failing on the way down, which is the loop the whole echo
	// model exists to break.
	if n := f.store.putCount("notes/hello.txt"); n != 1 {
		t.Errorf("the file was uploaded %d times; want 1 (a peer pushed back what it pulled)", n)
	}
	if n := f.store.getCount("notes/hello.txt"); n != 2 {
		t.Errorf("the file was downloaded %d times; want 2 (one per peer that did not write it)", n)
	}
}

// --- MC-21: an identical rewrite must not cost the fleet anything -------------

func TestFleetSkipsRewritesThatChangeNothing(t *testing.T) {
	f := newFleet(t, 3)

	// M6's unchanged-content gate does not run below one block: for a small file
	// the round trip costs what the upload would. A file that never reaches that
	// size cannot demonstrate the gate at all.
	body := strings.Repeat("m6-unchanged-content-gate;", 200_000) // ~5 MiB
	f.peers[0].write("big.bin", body)
	f.waitConverged(fanoutWait)

	puts, gets := f.counts()

	// Two shapes of no-op, both of which reach the engine as real events: a
	// metadata-only touch (OpSetattr) and a rewrite of identical bytes (OpWrite
	// with extents). Neither changed a byte the remote does not already hold.
	when := time.Now().Add(time.Minute)
	if err := os.Chtimes(f.peers[0].mnt("big.bin"), when, when); err != nil {
		t.Fatalf("touch: %v", err)
	}
	f.peers[0].write("big.bin", body)

	settle(quietWait)

	if p, g := f.counts(); p != puts || g != gets {
		t.Errorf("a no-op rewrite cost the fleet %d upload(s) and %d download(s); want none. "+
			"M6's unchanged-content gate did not fire, so every touch is now a full file-size event on every peer",
			p-puts, g-gets)
	}
	f.waitConverged(fanoutWait)
}

// --- MC-26: a rename must not leave the old path behind ----------------------

func TestFleetRenameLeavesNoStalePathOnOtherPeers(t *testing.T) {
	f := newFleet(t, 3)

	f.peers[0].write("x.txt", "renamed later")
	f.waitConverged(fanoutWait)

	if err := os.Rename(f.peers[0].mnt("x.txt"), f.peers[0].mnt("y.txt")); err != nil {
		t.Fatalf("rename through the mount: %v", err)
	}
	f.waitConverged(fanoutWait)

	// This is the seam's contract, and it is the whole reason the case exists.
	// provider.RemoteChange has no way to say "this object moved" — it carries a
	// path, a file and a removed flag — so the only faithful way for a change feed
	// to report a rename is a removal of the old path and an addition of the new
	// one. memStore.Move does exactly that.
	//
	// A provider that reports only the new path (because its native feed is keyed
	// by object identity and never mentions a path an object has left) leaves every
	// other peer holding a duplicate at the old path until a sweep infers the
	// delete — up to -sweep-interval later, 24h by default. See
	// TestChangesReportsTheOldPathOfARenamedFile in internal/provider/gdrive,
	// which is where that is checked against the shape of Drive's own feed.
	for _, p := range f.peers {
		if _, ok := p.read("x.txt"); ok {
			t.Errorf("%s still holds the pre-rename path x.txt; the rename duplicated the file", p.name)
		}
		if got, _ := p.read("y.txt"); got != "renamed later" {
			t.Errorf("%s holds %q at the post-rename path", p.name, got)
		}
	}
}

// --- MC-23: a bulk delete reaches every peer ---------------------------------

func TestFleetPropagatesABulkDelete(t *testing.T) {
	f := newFleet(t, 3)

	const n = 40
	for i := range n {
		f.peers[0].write(filepath.ToSlash(filepath.Join("bulk", "f"+strconv.Itoa(i)+".txt")), "content "+strconv.Itoa(i))
	}
	f.waitConverged(fanoutWait)

	if err := os.RemoveAll(f.peers[0].mnt("bulk")); err != nil {
		t.Fatalf("rm -rf through the mount: %v", err)
	}
	f.waitConverged(2 * fanoutWait)

	// -max-deletes bounds what a *reconcile* may infer from a baseline; a delete
	// that arrives on the change feed is an observation, not an inference, and is
	// not capped. A fleet in which one peer removes a tree while the others are
	// online must therefore converge on empty however large the tree is.
	for _, p := range f.peers {
		if entries, err := os.ReadDir(p.data("bulk")); err == nil && len(entries) > 0 {
			t.Errorf("%s still holds %d file(s) under a deleted directory", p.name, len(entries))
		}
	}
}

// --- MC-31a: both peers push, and the loser's bytes are gone ------------------

func TestFleetConcurrentEditsConvergeWithoutAConflictCopy(t *testing.T) {
	f := newFleet(t, 3)

	f.peers[0].write("shared.txt", "the common ancestor")
	f.waitConverged(fanoutWait)

	// As close to simultaneous as two goroutines get: both writes land inside the
	// 300ms debounce window, so both peers push before either polls.
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, body := range map[int]string{0: "edited on c1", 1: "edited on c2"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			f.peers[i].write("shared.txt", body)
		}()
	}
	close(start)
	wg.Wait()

	f.waitConverged(fanoutWait)

	// What actually happens, and it is worth stating plainly because it is not what
	// "conflict policy" suggests: when both peers succeed in pushing, the store
	// serialises them and the second write wins outright. Neither peer ever sees a
	// divergence — each one's local content still matches its own echo — so §6
	// never runs and the losing edit is gone with no copy kept.
	//
	// §6 resolves the case where a local edit could NOT be pushed (the peer was
	// offline, or its push is still failing), which is the case below. If this
	// assertion ever starts failing, the conflict policy has begun firing on
	// ordinary concurrent writes, and every busy fleet will fill with copies.
	for _, p := range f.peers {
		if got := p.conflicts(); len(got) != 0 {
			t.Errorf("%s made conflict copies %v for edits that both reached the remote", p.name, got)
		}
	}
	winner, _ := f.peers[2].read("shared.txt")
	if winner != "edited on c1" && winner != "edited on c2" {
		t.Errorf("the fleet settled on %q, which neither peer wrote", winner)
	}
}

// --- MC-31b: an edit that never got pushed is preserved, not overwritten ------

func TestFleetKeepsAnUnpushedLocalEditAsAConflictCopy(t *testing.T) {
	f := newFleet(t, 3)
	c1, c2 := f.peers[0], f.peers[1]

	c1.write("shared.txt", "the common ancestor")
	f.waitConverged(fanoutWait)

	// c2 goes away, and is edited while it is away — a laptop closed mid-edit, or a
	// crash between the write and the push. The edit is written into the backing
	// dir directly because that is what it looks like from drivel's side: content
	// that changed with no mount event behind it.
	c2.stop()
	if err := os.WriteFile(c2.data("shared.txt"), []byte("edited on c2 while it was down"), 0o644); err != nil {
		t.Fatalf("offline edit: %v", err)
	}
	// Stamp it later than anything the remote will hold, so §6's last-writer-wins
	// resolves to a known side instead of to whichever clock ticked first.
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(c2.data("shared.txt"), later, later); err != nil {
		t.Fatalf("stamping the offline edit: %v", err)
	}

	c1.write("shared.txt", "edited on c1 while c2 was down")
	if !waitFor(fanoutWait, func() bool {
		b, ok := f.store.content("shared.txt")
		return ok && string(b) == "edited on c1 while c2 was down"
	}) {
		t.Fatal("c1's edit never reached the remote")
	}

	c2.start()

	// Both sides moved from the baseline, so neither may be silently dropped.
	if !waitFor(fanoutWait, func() bool { return len(c2.conflicts()) > 0 }) {
		t.Fatal("c2 rejoined with a divergent local edit and no conflict copy was made; " +
			"one of the two versions has been lost")
	}
	copies := c2.conflicts()
	if len(copies) != 1 {
		t.Errorf("c2 made %d conflict copies %v; want 1", len(copies), copies)
	}

	// Local was stamped newer, so it keeps the real path and the remote version is
	// the copy. Whichever way the policy resolves, both byte streams must still
	// exist somewhere on the peer that saw the collision.
	held := map[string]bool{}
	if body, ok := c2.read("shared.txt"); ok {
		held[body] = true
	}
	for _, c := range copies {
		if body, ok := c2.read(c); ok {
			held[body] = true
		}
	}
	for _, want := range []string{"edited on c2 while it was down", "edited on c1 while c2 was down"} {
		if !held[want] {
			t.Errorf("c2 no longer holds %q anywhere; the conflict lost a version", want)
		}
	}

	// And the copy stays local: uploading it would publish the losing side of every
	// conflict the fleet has ever had, to every other peer.
	settle(quietWait)
	for _, c := range copies {
		if _, ok := f.store.content(c); ok {
			t.Errorf("the conflict copy %s was pushed to the remote", c)
		}
	}
}

// --- MC-32: the fleet reaches a fixed point ----------------------------------

func TestFleetReachesAFixedPoint(t *testing.T) {
	f := newFleet(t, 3)

	// A burst with every shape of event in it, from all three peers at once.
	var wg sync.WaitGroup
	for _, p := range f.peers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 5 {
				p.write("mixed/"+p.name+"-"+strconv.Itoa(j)+".txt", "from "+p.name)
			}
			p.write("contended.txt", "written by "+p.name)
		}()
	}
	wg.Wait()
	f.waitConverged(2 * fanoutWait)

	// The assertion is that the numbers stop. Every loop breaker in the system —
	// §4's echo record, the on-disk hash comparison in apply, M6's unchanged-content
	// gate — exists so that identical content generates no further work, and a
	// fleet is where a failure of any of them shows up as growth rather than as a
	// single wrong file: each peer's correction is another peer's remote change.
	beforePuts, beforeGets := f.counts()
	beforeCopies := f.conflictCount()
	settle(quietWait)
	afterPuts, afterGets := f.counts()

	if afterPuts != beforePuts || afterGets != beforeGets {
		t.Errorf("the fleet was still working %s after it converged: %d more upload(s), %d more download(s). "+
			"Traffic that continues after the content agrees is the ping-pong §4 exists to prevent",
			quietWait, afterPuts-beforePuts, afterGets-beforeGets)
	}

	// contended.txt is written by all three peers at once, so conflict copies here
	// are legitimate: a peer that creates a path locally and receives a different
	// remote version before its own push lands has no baseline to compare against,
	// and §6 keeps both sides rather than picking one. What must not happen is that
	// they keep arriving — a fleet that manufactures a copy per poll fills the disk
	// with the same collision — or that any of them is uploaded, which would
	// publish the losing side of every conflict to every other peer.
	if after := f.conflictCount(); after != beforeCopies {
		t.Errorf("conflict copies went from %d to %d during a quiet %s; the fleet is still resolving a collision that is over",
			beforeCopies, after, quietWait)
	}
	for _, p := range f.peers {
		for _, c := range p.conflicts() {
			if _, ok := f.store.content(c); ok {
				t.Errorf("%s pushed its conflict copy %s to the remote", p.name, c)
			}
		}
	}
}

// conflictCount is how many §6 copies the whole fleet holds.
func (f *fleet) conflictCount() int {
	n := 0
	for _, p := range f.peers {
		n += len(p.conflicts())
	}
	return n
}

// --- MC-25: a dead cursor over a bulk delete stalls on -max-deletes ----------

// The sequence is ordinary in a fleet and alarming to a sweep: a peer is away
// while another deletes in bulk, and its cursor dies before it comes back. The
// page that carried those deletes no longer exists, so nothing can *observe*
// them — only a reconcile can infer them, from the §4 echo store, and inferred
// deletes are what -max-deletes caps.
//
// The guard is right to refuse. The identical evidence — "every path I have a
// baseline for is missing from the remote" — is what a state DB pointed at the
// wrong account or an empty -drive-root produces, and that shape must never
// delete anything. What this case pins is the price of being right: the peer
// keeps every stale copy, stays diverged with no further attempt, and does not
// recover by being restarted. It also pins the two things it must NOT do —
// delete a subset, or push the stale copies back at the rest of the fleet.
func TestFleetStallsWhenACursorExpiryUncoversABulkDelete(t *testing.T) {
	// The real defaults are 100 and, in the plan's version of this case, 5000
	// files. The guard does not care about the magnitudes, only about crossing
	// them, and a fleet test pays for every file three times over.
	const (
		maxDeletes = 5
		files      = maxDeletes + 3
	)
	name := func(i int) string { return "bulk/f" + strconv.Itoa(i) + ".txt" }

	f := newFleet(t, 2, func(s *MountSpec) { s.MaxDeletes = maxDeletes })
	c1, c2 := f.peers[0], f.peers[1]

	for i := range files {
		c1.write(name(i), "content "+strconv.Itoa(i))
	}
	f.waitConverged(fanoutWait)

	// c2 leaves. From here until it returns, the feed is the only thing that could
	// tell it anything — and the feed is what is about to be taken away.
	c2.stop()

	if err := os.RemoveAll(c1.mnt("bulk")); err != nil {
		t.Fatalf("rm -rf through the mount: %v", err)
	}
	if !waitFor(fanoutWait, func() bool {
		for p := range f.store.manifest() {
			if strings.HasPrefix(p, "bulk/") {
				return false
			}
		}
		return true
	}) {
		t.Fatal("c1's bulk delete never reached the remote")
	}

	// Kill c2's cursor. Tier B does this with a bbolt editor for the same reason:
	// Drive answers a token it no longer retains with 410 and a malformed one with
	// 400/pageToken, gdrive classifies both as provider.ErrCursorExpired, and
	// memStore answers a cursor it cannot place the same way.
	corruptCursor(t, c2.spec.StateDB)

	c2.start()

	if !waitFor(fanoutWait, func() bool { return c2.logw.saw("[pull] cursor expired") }) {
		t.Fatal("c2 never noticed its cursor was dead; inbound sync is stalled silently, " +
			"which is the pre-M7b failure the expiry branch exists to replace")
	}
	if !waitFor(fanoutWait, func() bool { return c2.logw.saw("[sweep] REFUSING to delete") }) {
		t.Fatalf("the recovery sweep did not hit the -max-deletes guard; it inferred %d deletions "+
			"against a limit of %d and either applied or ignored them", files, maxDeletes)
	}

	// Note what the sweep just enumerated: nothing. An empty remote tree is M7b's
	// most dangerous shape precisely because every synced path then looks deleted,
	// and this is that shape arriving legitimately. The guard cannot tell the two
	// apart, which is why it refuses rather than choosing.
	settle(quietWait)

	for i := range files {
		if _, ok := c2.read(name(i)); !ok {
			t.Errorf("c2 deleted %s after refusing the pass; the guard trimmed the pass to the limit "+
				"instead of abandoning it, which deletes an arbitrary subset", name(i))
		}
		// The sweep's other half is the local walk, and every one of these files is
		// local-only from c2's point of view now. It must not push them: they have a
		// baseline, which makes them the delete pass's business, and a stalled peer
		// that uploaded them would resurrect the whole tree for every other client.
		if _, ok := f.store.content(name(i)); ok {
			t.Fatalf("c2 re-uploaded %s; a stalled peer just undid the delete for the whole fleet", name(i))
		}
	}
	if f.disagreement() == "" {
		t.Fatal("the fleet converged; the -max-deletes stall did not reproduce")
	}

	// Recovery, and it needs two changes rather than one. Raising the cap alone
	// does nothing: the refused sweep still recorded itself as *complete*, so the
	// next start finds a valid cursor, no interrupted sweep and — with no
	// -sweep-interval — no schedule, and never reconciles again.
	//
	// So the refusal has to say so. An operator who follows a message that names
	// only -max-deletes restarts, sees no sweep, no error and no change, and has
	// no way to tell a stalled peer from a converged one.
	if !c2.logw.saw("-resync") {
		t.Error("the refusal does not tell the operator to re-run with -resync. " +
			"Raising -max-deletes on its own does not re-run the sweep, so the message as written " +
			"describes a recovery that silently does nothing")
	}

	c2.stop()
	c2.spec.MaxDeletes = 0
	c2.start()
	settle(quietWait)
	if f.disagreement() == "" {
		t.Error("raising -max-deletes and restarting recovered the peer on its own. " +
			"That is better behaviour than this test was written against — update the case, " +
			"and drop the -resync half of the documented recovery")
	}

	// The documented recovery: force the sweep, with a cap that permits the count.
	c2.stop()
	c2.spec.Resync = true
	c2.start()
	f.waitConverged(fanoutWait)

	for i := range files {
		if _, ok := c2.read(name(i)); ok {
			t.Errorf("c2 still holds %s after -resync with -max-deletes 0", name(i))
		}
	}
}

// corruptCursor writes a cursor no provider can place into a stopped peer's state
// DB. The peer has to be stopped: bbolt is single-writer, so state.Open against a
// live mount blocks for its five-second timeout and then fails.
func corruptCursor(t *testing.T, dbPath string) {
	t.Helper()
	st, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("opening the state DB to kill its cursor: %v", err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			t.Errorf("closing the state DB: %v", err)
		}
	}()
	if err := st.SetCursor("this-token-is-gone"); err != nil {
		t.Fatalf("killing the cursor: %v", err)
	}
}

// --- MC-12: the two name shapes drivel reserves for itself --------------------

// reservedNames are the names a real user file can have that drivel also uses to
// recognise its own artefacts: the ".drivel-" prefix (mount probes, download and
// placeholder temporaries) and the §6 conflict-copy shape.
var reservedNames = []string{
	".drivel-notes",
	"report (conflict 2024-01-01 00-00-00).pdf",
}

// offlineReserved are the same two shapes under different names, for the half of
// the case that creates them with nothing watching. They are spelled out rather
// than derived from reservedNames because a derived name is easy to get wrong in
// a way that quietly passes: "offline-.drivel-notes" does not start with
// ".drivel-" and is pushed like any other file.
var offlineReserved = []string{
	".drivel-offline-notes",
	"memo (conflict 2024-02-02 11-11-11).txt",
}

// A user file whose name drivel has reserved syncs when it is written through a
// mount, and never syncs when it is not. The two halves of the engine disagree,
// and the plan (MC-12) left which way as the open question.
//
// The answer is that `skipLocal` guards only reconcile's local walk. The push
// path — an fsevent from the mount, through Engine.Push — applies no name filter
// at all, so creating either file through the mountpoint uploads it like anything
// else. Create the same file while the engine is not watching, though, and the
// sweep that exists precisely to catch up on missed local content will step over
// it, silently and for as long as the file exists.
//
// Nothing here argues the filter is wrong: without it the local walk would
// re-upload every conflict copy drivel has ever written, which is the §6
// catastrophe. What it argues is that the two paths give different answers about
// the same file, that the difference is invisible, and that a fleet's convergence
// check cannot see it either — `peer.manifest` skips both name shapes, exactly as
// scripts/multiclient/manifest.sh does, so every assertion about these files has
// to go around the oracle rather than through it.
func TestFleetSyncsAReservedNameFromTheMountButNeverFromASweep(t *testing.T) {
	f := newFleet(t, 3)
	c1, c2, c3 := f.peers[0], f.peers[1], f.peers[2]

	// Written through the mount: an ordinary event, an ordinary push.
	for _, name := range reservedNames {
		c1.write(name, "user content in "+name)
	}
	for _, name := range reservedNames {
		if !waitFor(fanoutWait, func() bool {
			b, ok := f.store.content(name)
			return ok && string(b) == "user content in "+name
		}) {
			t.Errorf("%q was written through a mount and never reached the remote", name)
			continue
		}
		for _, p := range []*peer{c2, c3} {
			if !waitFor(fanoutWait, func() bool {
				body, ok := p.read(name)
				return ok && body == "user content in "+name
			}) {
				t.Errorf("%q reached the remote but never reached %s", name, p.name)
			}
		}
	}

	// Created while the engine was not watching: no event, so the only thing that
	// could ever push it is the sweep's local walk, which skips it by name.
	c2.stop()
	for _, name := range offlineReserved {
		if err := os.WriteFile(c2.data(name), []byte("made while c2 was down"), 0o644); err != nil {
			t.Fatalf("offline create: %v", err)
		}
	}
	c2.spec.Resync = true // force the sweep whose local walk is the subject
	c2.start()

	if !waitFor(fanoutWait, func() bool { return c2.logw.saw("[sweep] reconcile complete") }) {
		t.Fatal("c2's sweep never completed; the half of the case that needs it did not run")
	}
	settle(quietWait)

	for _, name := range offlineReserved {
		if _, ok := f.store.content(name); ok {
			t.Errorf("the sweep pushed %q; skipLocal no longer guards the local walk, "+
				"which means a conflict copy will be published next", name)
		}
		if body, ok := c2.read(name); !ok || body != "made while c2 was down" {
			t.Errorf("%q is no longer on c2 intact; a file the sweep will not push must at least be left alone", name)
		}
	}
}
