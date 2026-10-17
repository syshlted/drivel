// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package app

import (
	"os"
	"testing"
	"time"

	"github.com/syshlted/drivel/internal/state"
)

// A push lost to a crash is found by the sweep's fingerprint row (M27 part 4),
// but only if a sweep runs — and on a provider with a feed and a stored cursor,
// with -sweep-interval 0, nothing started one. The unclean-shutdown mark is what
// does now. These run the whole mount, because the mark's correctness is in its
// placement around Run's drains, which no lower test can see.

// crashAfter makes p's last run look like one that was killed: the mark a live
// Run sets, still set, and a rewrite of rel that reached the backing file and
// was never pushed — written past the mount, so no event exists for it, and
// stamped before the next start so it is not the next engine's to finish.
func crashAfter(t *testing.T, p *peer, rel, body string, leaveMark bool) {
	t.Helper()
	st, err := state.Open(p.spec.StateDB)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st.Running(); err != nil || ok {
		t.Fatalf("after a clean stop: running mark ok=%v err=%v; want it cleared", ok, err)
	}
	if leaveMark {
		if err := st.SetRunning(time.Now().Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.data(rel), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Add(-time.Minute)
	if err := os.Chtimes(p.data(rel), stamp, stamp); err != nil {
		t.Fatal(err)
	}
}

func TestARestartAfterAnUncleanShutdownRepushesALostWrite(t *testing.T) {
	f := newFleet(t, 1)
	c1 := f.peers[0]
	c1.write("a.txt", "old")
	if !waitFor(10*time.Second, func() bool { b, _ := f.store.content("a.txt"); return string(b) == "old" }) {
		t.Fatal("the first write never reached the remote")
	}
	c1.stop()
	crashAfter(t, c1, "a.txt", "new, and lost to the crash", true)

	from := c1.logw.mark()
	c1.start()
	if !waitFor(15*time.Second, func() bool {
		b, _ := f.store.content("a.txt")
		return string(b) == "new, and lost to the crash"
	}) {
		b, _ := f.store.content("a.txt")
		t.Fatalf("remote = %q; the write lost to the crash was never re-sent", b)
	}
	if !c1.logw.sawSince(from, "did not shut down cleanly") {
		t.Error("the recovery sweep did not say why it ran")
	}
}

// The control, and the reason the mark exists rather than a sweep on every
// start: after a clean stop nothing sweeps, so the same out-of-band write stays
// where it is. Remove the mark's trigger from startFeed and the first test fails;
// make every start sweep and this one does.
func TestARestartAfterACleanShutdownDoesNotSweep(t *testing.T) {
	f := newFleet(t, 1)
	c1 := f.peers[0]
	c1.write("a.txt", "old")
	if !waitFor(10*time.Second, func() bool { b, _ := f.store.content("a.txt"); return string(b) == "old" }) {
		t.Fatal("the first write never reached the remote")
	}
	c1.stop()
	crashAfter(t, c1, "a.txt", "written while the mount was down", false)

	from := c1.logw.mark()
	c1.start()
	time.Sleep(2 * time.Second)
	if b, _ := f.store.content("a.txt"); string(b) != "old" {
		t.Errorf("remote = %q; a clean restart pushed without a sweep being due", b)
	}
	if c1.logw.sawSince(from, "[sweep]") {
		t.Error("a clean restart with a stored cursor ran a sweep")
	}
}
