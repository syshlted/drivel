// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package app

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/syshlted/drivel/internal/hydrate"
	"github.com/syshlted/drivel/internal/state"
	"github.com/syshlted/drivel/internal/testenv"
)

// Group V of docs/dev/multiclient-test-plan.md — failure, restart, degradation —
// in Tier A: what a fleet does when one client crashes, loses a database, or
// loses the network, with the remote and the other clients carrying on.
//
// The faults come from faultStore (faultstore_test.go) and are per peer, because
// every case here is about one client being broken while the fleet is not.
//
// Five of the group's nine cases are here — MC-41, MC-43 (in two shapes), MC-44b,
// MC-45 and MC-46 — and the other four are elsewhere by mechanism rather than by
// scheduling. MC-40 (kill -9 mid-upload) needs a process that dies rather than one
// asked to stop, and a resumable upload session to interrupt; the same is true of
// MC-41's literal form, which is why the case below interrupts a sweep by
// cancelling it. MC-42's subject is the Drive path index, which is provider-private
// — a path-addressed fake has none to lose — so its regression tests are in
// internal/provider/gdrive, over a cold index and a restart. MC-44's subject is the
// transport, which memStore does not have. And MC-47 is about what two *processes*
// do to each other: all three of its shapes are refused by app.Validate inside one
// process (the fleet runs it over its own specs), so an in-process version would
// either re-test that guard or deliberately bypass it and measure something no real
// process can reach.
//
// Wall clock: these are the slowest tests in the package, and the floor is the
// product's, not the harness's. The pull loop drops to its 30s idle cadence after
// a single failed poll (syncengine.DefaultCadence), so a peer that was offline
// takes up to 30s to notice it is not, and the engine's retry budget is five
// attempts over several seconds of jittered backoff. Nothing here sleeps for
// longer than it has to; it waits for the thing it is about to assert on.

// --- MC-41: a sweep interrupted mid-enumeration ------------------------------

// An interrupted sweep resumes where it stopped, and the pages the dead run
// consumed are not read as remote deletions.
//
// That is the whole reason internal/state keeps the seen marks on disk. The
// delete pass asks "does this baseline name something the sweep did not find
// remotely?", and after a resume the answer for every page the previous process
// consumed comes from bbolt or from nowhere: the resumed run never lists those
// objects again. An in-memory seen-set would therefore report each of them as
// gone from the remote, and since the local copies still match their baselines
// the pass would delete them — quietly, as the correct resolution of a bulk
// remote delete, on a client whose only fault was being stopped at the wrong
// moment.
//
// The two numbers that make this a real test rather than a shape: the sweep is
// paged, so there is something to resume from, and the baselines predate it, so
// the delete pass does not exempt them for the other reason it has (an echo
// written during a sweep describes a file created after its page was listed, and
// applyDeletes skips those regardless of the marks).
func TestFleetResumesAnInterruptedSweepWithoutInferringDeletes(t *testing.T) {
	const (
		files = 6
		page  = 2 // three pages, interrupted after the first
	)
	name := func(i int) string { return "f" + strconv.Itoa(i) + ".txt" }

	f := newFleet(t, 2)
	c1, c2 := f.peers[0], f.peers[1]

	for i := range files {
		c1.write(name(i), "content "+strconv.Itoa(i))
	}
	f.waitConverged(fanoutWait)

	// c2's baselines are now all older than the sweep that is about to run, which
	// is what makes them delete candidates at all.
	c2.stop()
	c2.faults.pageEnumeration(page)
	held := c2.faults.holdAfterFirstPage()
	c2.spec.Resync = true // c2's cursor is valid, so nothing else would sweep

	c2.start()
	select {
	case <-held:
	case <-time.After(fanoutWait):
		t.Fatal("c2's sweep never asked for a second page; the enumeration was not paged, " +
			"so there is nothing for this case to interrupt")
	}

	// The interruption. A graceful stop is not a kill -9 — that is MC-40's half and
	// stays Tier B — but it is the same thing from the sweep's side: the context is
	// cancelled mid-enumeration, the pages already applied stand, and the sweep
	// record is left on disk unfinished with its cursor pointing at page two.
	c2.stop()
	sw, ok := sweepRecord(t, c2.spec.StateDB)
	if !ok {
		t.Fatal("c2's interrupted sweep left no record to resume from: either the hold did not take " +
			"effect and the sweep finished, or an interruption now discards the sweep instead of parking it")
	}
	if sw.Cursor == "" {
		t.Fatal("c2's parked sweep has no enumeration cursor, so resuming it would restart the listing; " +
			"the cursor is persisted per page for exactly this")
	}
	c2.faults.releaseHold()
	c2.spec.Resync = false // resumption has to be automatic, not asked for

	mark := c2.logw.mark()
	c2.start()
	if !waitFor(fanoutWait, func() bool { return c2.logw.sawSince(mark, "[sweep] resuming the enumeration interrupted at") }) {
		t.Fatal("c2 did not resume the interrupted sweep; with no resume it starts a fresh one, " +
			"which is slower but safe — and leaves the seen marks' real job untested")
	}

	// The count is the discriminator, and without it this test cannot fail: a
	// resumed sweep lists the four objects it had not reached, while a sweep that
	// silently restarted would list all six and mark every one of them seen — which
	// is exactly the state in which the marks are never consulted. It is also what
	// says the resumed sweep *finished*: every other line here has appeared once
	// already, since a peer's log accumulates across its restarts.
	done := strconv.Itoa(files-page) + " remote object(s) listed"
	if !waitFor(fanoutWait, func() bool { return c2.logw.sawSince(mark, done) }) {
		t.Fatalf("c2's resumed sweep did not complete over exactly the %d objects left after page one. "+
			"If it reported %d it restarted rather than resumed, and the seen marks were never under test",
			files-page, files)
	}

	settle(quietWait)
	for i := range files {
		if _, ok := c2.read(name(i)); !ok {
			t.Errorf("c2 deleted %s after resuming an interrupted sweep. The pages the previous run "+
				"consumed are not listed again, so their seen marks are the only thing standing between "+
				"them and the delete pass — these are persistent for this reason", name(i))
		}
		if _, ok := f.store.content(name(i)); !ok {
			t.Errorf("%s is gone from the remote: the resumed sweep read a page it had already consumed "+
				"as a remote deletion and propagated it for the whole fleet", name(i))
		}
	}
	if c2.logw.sawSince(mark, "deleted locally") || c2.logw.sawSince(mark, "deleted remotely") {
		t.Error("c2's resumed sweep deleted something; nothing was deleted on either side")
	}
	f.waitConverged(fanoutWait)
}

// sweepRecord reads a stopped peer's parked sweep, if it has one. Stopped, for
// the reason corruptCursor is: bbolt is single-writer, so opening a live mount's
// state DB waits out its five-second timeout and fails.
func sweepRecord(t *testing.T, dbPath string) (state.Sweep, bool) {
	t.Helper()
	st, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("opening the state DB to read its sweep record: %v", err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			t.Errorf("closing the state DB: %v", err)
		}
	}()
	sw, ok, err := st.Sweep()
	if err != nil {
		t.Fatalf("reading the sweep record: %v", err)
	}
	return sw, ok
}

// --- MC-43: a lazy client loses its state DB ---------------------------------

// Placeholders survive the loss of the state DB, because the marker is on the
// file and the DB is not a fallback for it (M5 invariant 1).
//
// The danger is specific and is the worst outcome in the system: a placeholder is
// a full-size file holding no bytes, so anything that mistakes one for ordinary
// local content uploads zeros over the remote copy. Losing the state DB removes
// every echo, which is the baseline both the sweep's local walk and the pull
// loop's conflict check read — so after the loss, a placeholder is a local file
// that matches no baseline and whose bytes differ from the remote's, which is the
// definition of a §6 conflict and one step away from a push.
//
// What saves it is that the pull path asks the hydrator before it hashes anything:
// applyTo re-stamps a placeholder instead of computing the digest of a hole. That
// is the guard this case exercises, and it is the only one it can — the sweep
// applies its pages before it walks the tree, so by the time reconcile's local walk
// runs, every placeholder has a fresh echo and is skipped for having a baseline
// rather than for being a placeholder. Removing the walk's own check changes
// nothing here, which is why the case below it exists.
func TestFleetKeepsPlaceholdersSafeWhenTheStateDBIsLost(t *testing.T) {
	bodies := map[string]string{
		"docs/a.txt": strings.Repeat("alpha ", 64),
		"docs/b.txt": strings.Repeat("bravo ", 64),
		"c.txt":      strings.Repeat("charlie ", 64),
	}

	f := newFleet(t, 2)
	c1, c2 := f.peers[0], f.peers[1]

	// c2 becomes the lazy client. Restarting it with the flag changed is how the
	// fleet expresses "this machine is configured differently", and it has to be a
	// restart: the hydrator is built in Open.
	c2.stop()
	c2.spec.Lazy = true
	if !hydrate.New(c2.spec.DataDir, nil, nil, 1).XattrsUsable() {
		c2.spec.Lazy = false
		c2.start() // leave the fleet as the harness's cleanup expects to find it
		testenv.Unavailable(t, testenv.Xattr, "backing filesystem cannot hold "+hydrate.XattrName+
			", so there are no placeholders to lose a state DB under")
	}

	for p, body := range bodies {
		c1.write(p, body)
	}
	for p := range bodies {
		if !waitFor(fanoutWait, func() bool { _, ok := f.store.content(p); return ok }) {
			t.Fatalf("%s never reached the remote", p)
		}
	}

	c2.start()
	for p := range bodies {
		if !waitFor(fanoutWait, func() bool { return placeholderAt(c2, p) }) {
			t.Fatalf("c2 never made a placeholder for %s; the rest of this case is about losing "+
				"the state DB under one", p)
		}
	}
	// Apparent size, no resident bytes: the shape that makes a mistaken push
	// catastrophic rather than merely wrong.
	for p, body := range bodies {
		fi, err := os.Stat(c2.data(p))
		if err != nil {
			t.Fatalf("stat %s on c2: %v", p, err)
		}
		if fi.Size() != int64(len(body)) {
			t.Errorf("c2's placeholder for %s is %d bytes; want the remote's apparent size %d",
				p, fi.Size(), len(body))
		}
	}

	// The event. Not a corruption — the whole file, as a user clearing out
	// ~/.local/state or a packaging change moving it would produce.
	c2.stop()
	if err := os.Remove(c2.spec.StateDB); err != nil {
		t.Fatalf("removing c2's state DB: %v", err)
	}
	before := map[string]int{}
	for p := range bodies {
		before[p] = f.store.putCount(p)
	}
	mark := c2.logw.mark()
	c2.start()

	// With no cursor there is no feed to resume, so this start sweeps — the one
	// code path that walks c2's whole backing tree while holding no baseline for
	// any of it.
	if !waitFor(fanoutWait, func() bool { return c2.logw.sawSince(mark, "[sweep] reconcile complete") }) {
		t.Fatal("c2's post-loss sweep never completed; the local walk this case is about did not run")
	}
	settle(quietWait)

	for p, body := range bodies {
		if got := f.store.putCount(p); got != before[p] {
			t.Errorf("c2 pushed %s after losing its state DB (%d puts, was %d). A placeholder holds no "+
				"bytes, so this is zeros over the remote copy: the M5 catastrophe", p, got, before[p])
		}
		if got, ok := f.store.content(p); !ok || string(got) != body {
			t.Errorf("the remote's copy of %s is no longer what c1 wrote", p)
		}
		if !placeholderAt(c2, p) {
			t.Errorf("c2's %s is no longer a placeholder; the sweep materialised or hashed it instead of "+
				"re-stamping it, which is a download per file per sweep at best", p)
		}
		if c2.conflicts() != nil {
			t.Errorf("c2 holds conflict copies %v; a placeholder has no resident bytes and cannot have "+
				"diverged, so hashing one manufactured a conflict", c2.conflicts())
		}
	}

	// And the promise still holds: the marker that survived is not just a label,
	// it is what the first read uses to fetch the bytes.
	for p, body := range bodies {
		got, err := os.ReadFile(c2.mnt(p))
		if err != nil {
			t.Errorf("reading %s through c2's mountpoint: %v", p, err)
			continue
		}
		if string(got) != body {
			t.Errorf("hydrating %s on c2 after the state DB loss gave %d bytes, want %d",
				p, len(got), len(body))
		}
	}
}

// The same client, the same lost DB, and one of the files deleted remotely while
// it was down: the shape where the local walk's placeholder skip is the only
// thing in front of a push.
//
// It is worth separating from the case above because in that one the guard is
// never reached. The sweep applies its pages before it walks the tree, and
// applying a page re-stamps each placeholder and writes it a fresh echo — so by
// the time the walk runs, every placeholder has a baseline and is skipped for
// that reason instead. Remove the remote object and the page never mentions it:
// no re-stamp, no echo, not seen remotely, which is the "new locally" row with a
// file holding no bytes in it.
//
// Two things are asserted and they are not the same claim. That nothing was
// pushed is the invariant, and it survives either guard — the engine's M5 gate
// refuses a placeholder whatever asked it to. That the sweep did not even offer
// it is the one that distinguishes them, and it is the difference between a
// design with two independent guards and a design with one and a fallback.
//
// What it also records: the local file is left as an un-hydratable stub. It has
// no baseline, so the delete pass may not touch it (a path without a baseline is
// never deleted), and its content lives at a remote path that no longer exists,
// so the next read of it fails. That is the sweep's version of the EIO stub
// MC-60 asks about for the feed path, and nothing in the system resolves it.
func TestFleetNeverPushesAPlaceholderWhoseRemoteIsGone(t *testing.T) {
	const (
		kept = "kept.txt"
		gone = "gone.txt"
	)
	body := strings.Repeat("placeholder bytes ", 32)

	f := newFleet(t, 2)
	c1, c2 := f.peers[0], f.peers[1]

	c2.stop()
	c2.spec.Lazy = true
	if !hydrate.New(c2.spec.DataDir, nil, nil, 1).XattrsUsable() {
		c2.spec.Lazy = false
		c2.start()
		testenv.Unavailable(t, testenv.Xattr, "backing filesystem cannot hold "+hydrate.XattrName)
	}

	c1.write(kept, body)
	c1.write(gone, body)
	for _, p := range []string{kept, gone} {
		if !waitFor(fanoutWait, func() bool { _, ok := f.store.content(p); return ok }) {
			t.Fatalf("%s never reached the remote", p)
		}
	}

	c2.start()
	for _, p := range []string{kept, gone} {
		if !waitFor(fanoutWait, func() bool { return placeholderAt(c2, p) }) {
			t.Fatalf("c2 never made a placeholder for %s", p)
		}
	}

	// Both halves of the setup happen with c2 down, so it learns about neither.
	c2.stop()
	if err := os.Remove(c2.spec.StateDB); err != nil {
		t.Fatalf("removing c2's state DB: %v", err)
	}
	if err := os.Remove(c1.mnt(gone)); err != nil {
		t.Fatalf("deleting %s through c1's mount: %v", gone, err)
	}
	if !waitFor(fanoutWait, func() bool { _, ok := f.store.content(gone); return !ok }) {
		t.Fatalf("c1's delete of %s never reached the remote", gone)
	}

	mark := c2.logw.mark()
	c2.start()
	if !waitFor(fanoutWait, func() bool { return c2.logw.sawSince(mark, "[sweep] reconcile complete") }) {
		t.Fatal("c2's post-loss sweep never completed")
	}
	settle(quietWait)

	if _, ok := f.store.content(gone); ok {
		t.Errorf("%s is back on the remote. c2 uploaded a file holding no bytes over a path its owner "+
			"deleted — the M5 catastrophe and a resurrection in one", gone)
	}
	if c2.logw.sawSince(mark, "[sweep] push    "+gone) {
		t.Errorf("the sweep offered %s to the uploader. The engine's M5 gate refused it (look for "+
			"\"unhydrated placeholder\"), so nothing was lost this time — but the local walk's own "+
			"placeholder check is what makes that a second line of defence rather than the only one", gone)
	}
	// The file that still exists remotely is untouched, so the walk has not simply
	// stopped pushing.
	if got, ok := f.store.content(kept); !ok || string(got) != body {
		t.Errorf("the remote's copy of %s changed", kept)
	}

	// And the stub is still there, because nothing is allowed to delete it.
	if !placeholderAt(c2, gone) {
		t.Errorf("c2's %s is no longer a placeholder; it was either hydrated from a path that does not "+
			"exist or quietly replaced", gone)
	}
	if _, err := os.ReadFile(c2.mnt(gone)); err == nil {
		t.Errorf("reading %s through c2's mountpoint succeeded; its content is at a remote path that no "+
			"longer exists, so a successful read means zeros were served as content", gone)
	}
}

// placeholderAt reports whether p is still an un-hydrated placeholder in this
// peer's backing store, read the way the product reads it: the marker on the
// file, and nothing else. A Hydrator with no store behind it is enough — Marker
// only ever does a getxattr — and using the real one keeps the per-platform
// spelling of XattrName out of this test.
func placeholderAt(p *peer, rel string) bool {
	return hydrate.New(p.spec.DataDir, nil, nil, 1).IsPlaceholder(rel)
}

// --- MC-44b: one client partitioned from the remote --------------------------

// A client cut off from the remote keeps serving, loses nothing locally, and
// catches up inbound when the network returns — but the push it was making when
// the outage started is abandoned, and nothing retries it.
//
// The two directions have different budgets and that is the finding. Inbound
// retries forever: a failed poll backs the cadence off to 30s and the loop keeps
// polling, so any outage shorter than the cursor's life is survivable. Outbound
// has five attempts over a few seconds of jittered backoff, after which
// executeWithRetry logs and *drops* the event — so an outage of more than a few
// seconds silently leaves the local file as the only copy, and the only thing
// that ever repairs it is an M7b sweep. A fleet running with -sweep-interval 0,
// which is what §2.3 of the test plan specifies for the live rig, has no sweep
// scheduled at all.
//
// So the assertion is not "it converges". It is that it does not, that nothing
// said so beyond one log line, and that the documented repair works.
func TestFleetAbandonsAPushAcrossAnOutageAndRecoversOnlyBySweeping(t *testing.T) {
	f := newFleet(t, 2)
	c1, c2 := f.peers[0], f.peers[1]

	c1.write("before.txt", "written while the network was up")
	f.waitConverged(fanoutWait)

	c1.faults.goOffline()

	// The mount does not depend on the provider, so this write succeeds exactly as
	// it would online: §2.1's "an FS op never blocks on the network" is what makes
	// the outage invisible to the user, and also what makes the loss invisible.
	c1.write("during.txt", "written while c1 was partitioned")
	if body, ok := c1.read("during.txt"); !ok || body != "written while c1 was partitioned" {
		t.Fatal("c1's local write did not land during the outage; FS operations must not depend on the provider")
	}
	// Meanwhile the fleet carries on without it.
	c2.write("peer.txt", "written on c2 while c1 was away")

	if !waitFor(fanoutWait, func() bool { return c1.logw.saw("failed after") }) {
		t.Fatal("c1's push never exhausted its retry budget; this case needs an outage longer than " +
			"the backoff, or the budget has become unbounded")
	}
	if _, ok := f.store.content("during.txt"); ok {
		t.Fatal("during.txt reached the remote during the outage; the fault was not applied")
	}

	c1.faults.goOnline()

	// Inbound recovers on its own, and the bound is the idle cadence rather than
	// anything about the outage: one failed poll sets the wait to Slow, so the
	// first successful poll after the network returns is up to 30s later.
	if !waitFor(90*time.Second, func() bool {
		body, ok := c1.read("peer.txt")
		return ok && body == "written on c2 while c1 was away"
	}) {
		t.Fatal("c1 never pulled what c2 wrote during the outage; the pull loop stopped retrying, " +
			"which is inbound sync ending silently")
	}

	// Outbound does not. The feed is demonstrably live again — peer.txt proves it —
	// and the file c1 wrote during the outage is still nowhere but c1.
	settle(quietWait)
	if _, ok := f.store.content("during.txt"); ok {
		t.Error("during.txt reached the remote without a sweep. M27 schedules a repair sweep, but " +
			"only after repairDelay — minutes, far longer than this wait — so something else is " +
			"retrying the push and the case and the plan's §3 counter table both need updating")
	}
	if why := f.disagreement(); why == "" {
		t.Error("the fleet converged; the abandoned push did not reproduce")
	}
	if _, ok := c2.read("during.txt"); ok {
		t.Error("c2 holds during.txt, which never reached the remote")
	}

	// M27: giving up now leaves a record behind. Before it, this file was
	// forgotten outright and the only repair was a sweep nobody had scheduled.
	if !c1.logw.saw("recorded for repair by the next sweep") {
		t.Error("the abandoned push was not recorded for repair; the file is forgotten, not merely unsent")
	}

	// The repair, and the thing that changed. This used to need -resync: the
	// abandoned path has no baseline here, so the sweep's local walk would
	// eventually find it, but nothing ever asked for a sweep. Now the record
	// itself is a sweep trigger, so an ordinary restart is enough — no flag, and
	// with sweep-interval still 0.
	c1.stop()
	// The record outlives the process, which is the whole point of it being in
	// bbolt rather than in the engine's memory. Readable only now, because the
	// running mount holds the file's lock.
	st, err := state.Open(c1.spec.StateDB)
	if err != nil {
		t.Fatalf("reopening c1's state DB: %v", err)
	}
	var owed []string
	if err := st.EachRepair(func(p string, _ time.Time) error { owed = append(owed, p); return nil }); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	if len(owed) != 1 || owed[0] != "during.txt" {
		t.Fatalf("repair list across the restart = %v; want [during.txt]", owed)
	}

	c1.start()
	f.waitConverged(2 * fanoutWait)
	if body, ok := f.store.content("during.txt"); !ok || string(body) != "written while c1 was partitioned" {
		t.Errorf("a restart did not recover the abandoned push; content = %q, ok = %v", body, ok)
	}
	if !c1.logw.saw("[sweep] repush  during.txt") {
		t.Error("the file arrived, but not through the repair pass; something else re-sent it")
	}
}

// --- MC-46: a transfer that stops partway ------------------------------------

// A download that fails mid-stream leaves nothing at the destination path, and
// nothing later pushes a short file up as the remote's new content.
//
// The guard is the temp-file-plus-rename in Downloader.download, and what it
// prevents is the worst outcome available to a sync engine: a partial file at a
// real path has no baseline, so the next sweep's local walk reads it as content
// the user created and uploads it — replacing the remote's complete copy with a
// truncated one, for every other client.
//
// The second half is a finding rather than a guard. An apply failure does not
// hold the cursor: the page's error is logged and the cursor advances anyway, so
// the feed never offers that object again and the file stays missing until a
// sweep. On Drive's 24h default that is a day; with -sweep-interval 0 it is until
// someone restarts the mount.
func TestFleetNeverLandsAPartialDownloadAndNeverPushesItBack(t *testing.T) {
	const (
		path   = "big.txt"
		prefix = 8 // bytes served before the transfer drops
	)
	body := strings.Repeat("0123456789abcdef", 256) // 4 KiB, well past one Read

	f := newFleet(t, 2)
	c1, c2 := f.peers[0], f.peers[1]

	c2.faults.truncateGet(path, prefix)
	c1.write(path, body)

	if !waitFor(fanoutWait, func() bool { _, ok := f.store.content(path); return ok }) {
		t.Fatal("c1 never uploaded the file")
	}
	if !waitFor(fanoutWait, func() bool { return c2.logw.saw("[pull] apply " + path) }) {
		t.Fatal("c2's download never failed; the truncating fault was not applied")
	}

	// Nothing at the path, rather than something short at it.
	if got, ok := c2.read(path); ok {
		t.Fatalf("c2 holds %d bytes at %s after a failed transfer; a partial file at a real path is "+
			"what the next sweep uploads as the user's content (local bytes: %q)", len(got), path, got)
	}
	for _, stray := range strays(t, c2) {
		t.Errorf("c2 left a download temporary behind: %s", stray)
	}

	// The feed has moved on: it delivers the next change and never re-offers the
	// one it failed. This is the half nothing guards.
	c1.write("next.txt", "written after the failed download")
	if !waitFor(fanoutWait, func() bool { _, ok := c2.read("next.txt"); return ok }) {
		t.Fatal("c2 stopped applying changes after one failed download; a failed apply must not stall the feed")
	}
	if _, ok := c2.read(path); ok {
		t.Error("the feed re-delivered the failed download. That is better than this test was written " +
			"against — the cursor now waits for a failed apply — so update the case and the plan's MC-46 row")
	}

	// A sweep while the transfer is still broken must not make it worse. It finds a
	// remote object with no local copy and no baseline, which is a materialise, and
	// it finds no local file to push.
	puts := f.store.putCount(path)
	c2.stop()
	c2.spec.Resync = true
	mark := c2.logw.mark()
	c2.start()
	if !waitFor(fanoutWait, func() bool { return c2.logw.sawSince(mark, "[sweep] reconcile complete") }) {
		t.Fatal("c2's sweep never completed")
	}
	if got := f.store.putCount(path); got != puts {
		t.Errorf("c2 pushed %s during a sweep while its own copy was missing (%d puts, was %d)", path, got, puts)
	}
	if got, ok := f.store.content(path); !ok || string(got) != body {
		t.Fatalf("the remote's copy of %s changed: %d bytes, want %d", path, len(got), len(body))
	}

	// And once the transfer works, the sweep is the repair. -resync is still set, so
	// this start sweeps again rather than relying on the feed, which has long since
	// moved past the change that failed.
	c2.faults.healGet(path)
	c2.stop()
	c2.start()
	if !waitFor(fanoutWait, func() bool {
		got, ok := c2.read(path)
		return ok && got == body
	}) {
		t.Fatal("a sweep after the transfer recovered did not materialise the file c2 failed to download")
	}
	f.waitConverged(fanoutWait)
}

// strays lists drivel's own temporaries left in a peer's backing dir. The mount
// probe is one by name and is written by the harness on every start, so it is the
// one exclusion; anything else with that prefix is a download or placeholder
// temporary that should have been renamed into place or removed.
func strays(t *testing.T, p *peer) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(p.spec.DataDir, func(path string, e os.DirEntry, err error) error {
		if err != nil {
			return skipVanished(err)
		}
		if e.IsDir() || e.Name() == mountProbe {
			return nil
		}
		if strings.HasPrefix(e.Name(), ".drivel-") {
			rel, relErr := filepath.Rel(p.spec.DataDir, path)
			if relErr != nil {
				return relErr
			}
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("%s: walking the backing dir: %v", p.name, err)
	}
	return out
}

// --- MC-45: two clients sharing one token file -------------------------------

// MC-45 asks whether two clients sharing one token.json can corrupt it across a
// refresh. They cannot, because nothing in a mount ever writes one: gauth.SaveToken
// has exactly one caller, `drivel login`, and the provider only ever loads a token
// and hands it to oauth2, which refreshes in memory and never writes back.
//
// That is worth a test rather than a note, because the property is an absence and
// the absence is load-bearing in two directions. It is what makes a shared token
// file safe (§2.1 of the test plan requires one per client; this says why that is
// belt-and-braces rather than a correctness rule), and it is what makes M26's
// secret-service work small — read at mount, write at login, no write-back across
// the plugin seam. A later change that persisted a refreshed token would reopen
// both questions at once, in a place nobody would look.
//
// It greps, for the same reason internal/provider/sftp greps the tree for the
// host-key escape hatch: the mistake it guards against is a *new* caller, in a
// package that does not exist yet. (Naming that test here is what the grep
// catches — its own name contains the string it forbids — so it is described
// rather than named.)
func TestOnlyLoginWritesTheOAuthToken(t *testing.T) {
	root := repoRoot(t)
	var callers []string
	err := filepath.WalkDir(root, func(path string, e os.DirEntry, err error) error {
		if err != nil {
			return skipVanished(err)
		}
		if e.IsDir() {
			switch e.Name() {
			case ".git", "site", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			return nil
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		// The definition itself lives in internal/gauth; every other mention is a
		// call.
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if filepath.ToSlash(rel) == "internal/gauth/token.go" {
			return nil
		}
		if bytes.Contains(b, []byte("SaveToken")) {
			callers = append(callers, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the tree: %v", err)
	}
	want := []string{"cmd/drivel/login.go"}
	if len(callers) != len(want) || callers[0] != want[0] {
		t.Errorf("gauth.SaveToken is called from %v; want only %v.\n"+
			"A mount that writes a token file makes two things true again that this tree relies on being "+
			"false: clients may not share a token path (test plan §2.1, MC-45), and reading a credential "+
			"at mount time is no longer the whole of M26's read side.", callers, want)
	}
}

// repoRoot walks up from the test's working directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the working directory")
		}
		dir = parent
	}
}
