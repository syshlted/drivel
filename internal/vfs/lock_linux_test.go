// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package vfs

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/syshlted/drivel/internal/claim"
	"github.com/syshlted/drivel/internal/fsevent"
)

// A flock taken through the mount reaches the backing file. Before, the kernel
// kept it to itself, so the engine's claim.HeldByAnother — which is how it
// declines to upload a database caught mid-transaction — could see only lockers
// working on the backing tree directly, and a bbolt DB opened through the mount
// was uploaded mid-write. Linux only: it is the kernel that offers the
// capability, and this asserts that it was taken.
func TestAFlockThroughTheMountReachesTheBackingFile(t *testing.T) {
	backing := t.TempDir()
	mnt, _ := mountTest(t, backing, nil)
	if err := os.WriteFile(filepath.Join(mnt, "db"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f1, err := os.OpenFile(filepath.Join(mnt, "db"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f1.Close()
	b, err := os.Open(filepath.Join(backing, "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	if err := unix.Flock(int(f1.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("flock through the mount: %v", err)
	}
	if !claim.HeldByAnother(b) {
		t.Fatal("an exclusive flock taken through the mount is invisible on the backing file")
	}

	// Two lockers through the mount still exclude each other, as they did when
	// the kernel kept the locks.
	f2, err := os.OpenFile(filepath.Join(mnt, "db"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f2.Close()
	if err := unix.Flock(int(f2.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != unix.EWOULDBLOCK {
		t.Fatalf("a second exclusive flock through the mount = %v; want EWOULDBLOCK", err)
	}

	// A blocking flock waits for the holder and wakes when it lets go.
	got := make(chan error, 1)
	go func() { got <- unix.Flock(int(f2.Fd()), unix.LOCK_EX) }()
	select {
	case err := <-got:
		t.Fatalf("a blocking flock returned (%v) while the lock was held", err)
	case <-time.After(200 * time.Millisecond):
	}
	if err := unix.Flock(int(f1.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("the waiting flock: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a blocking flock did not wake when the holder unlocked")
	}

	// Released by closing, as flock is: the last descriptor going drops it.
	if err := f2.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for claim.HeldByAnother(b) {
		if time.Now().After(deadline) {
			t.Fatal("the lock outlived the handle that held it")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// next waits for the mount's next event, failing if none comes.
func next(t *testing.T, events <-chan fsevent.Event, why string) fsevent.Event {
	t.Helper()
	select {
	case ev := <-events:
		return ev
	case <-time.After(5 * time.Second):
		t.Fatalf("no event: %s", why)
	}
	return fsevent.Event{}
}

// quiet fails if the mount emits anything more within a short while.
func quiet(t *testing.T, events <-chan fsevent.Event, why string) {
	t.Helper()
	select {
	case ev := <-events:
		t.Fatalf("unexpected event %s %s: %s", ev.Op, ev.Path, why)
	case <-time.After(300 * time.Millisecond):
	}
}

// The engine declines to upload a file somebody holds an exclusive flock on, so
// a lock taken through the mount must be followed by an announcement once it is
// gone — or an append made under `flock f -c 'echo x >> f'`, whose lock holder
// never writes, would be declined and never sent at all. The second event is
// whole-file: the declined one's extents went with it.
func TestReleasingAFlockAnnouncesWhatWasWrittenUnderIt(t *testing.T) {
	backing := t.TempDir()
	if err := os.WriteFile(filepath.Join(backing, "log"), []byte("line 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mnt, events := mountTest(t, backing, nil)
	drain(events)

	holder, err := os.Open(filepath.Join(mnt, "log"))
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := unix.Flock(int(holder.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	app, err := os.OpenFile(filepath.Join(mnt, "log"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.WriteString("line 2\n"); err != nil {
		t.Fatal(err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	if ev := next(t, events, "the append's close"); ev.Op != fsevent.OpWrite || ev.Path != "log" {
		t.Fatalf("event = %s %s; want write log", ev.Op, ev.Path)
	}
	quiet(t, events, "nothing more while the lock stands")

	if err := unix.Flock(int(holder.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	ev := next(t, events, "the lock's release must re-announce what was written under it")
	if ev.Op != fsevent.OpWrite || ev.Path != "log" || ev.Dirty != nil {
		t.Fatalf("event = %s %s (extents %v); want a whole-file write of log", ev.Op, ev.Path, ev.Dirty)
	}
	quiet(t, events, "one re-announcement per release")
}

// The control: nothing locked, nothing owed — one write is one event.
func TestAnUnlockedWriteIsStillOneEvent(t *testing.T) {
	backing := t.TempDir()
	mnt, events := mountTest(t, backing, nil)
	drain(events)
	holder, err := os.Create(filepath.Join(mnt, "f"))
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(holder.Fd()), unix.LOCK_SH); err != nil { // shared: the engine does not wait for it
		t.Fatal(err)
	}
	if _, err := holder.WriteString("x"); err != nil {
		t.Fatal(err)
	}
	if err := holder.Close(); err != nil {
		t.Fatal(err)
	}
	if ev := next(t, events, "the new file's close"); ev.Op != fsevent.OpCreate {
		t.Fatalf("event = %s %s; want create", ev.Op, ev.Path)
	}
	quiet(t, events, "a shared lock owes nothing")
}

// A database created under its own lock announces itself at its first close,
// while that lock still stands, so the engine declines it; the release that
// follows is what sends it.
func TestAFileCreatedUnderItsOwnLockIsAnnouncedAgainAtRelease(t *testing.T) {
	backing := t.TempDir()
	mnt, events := mountTest(t, backing, nil)
	drain(events)
	db, err := os.Create(filepath.Join(mnt, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(db.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	if _, err := db.WriteString("page"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if ev := next(t, events, "the create"); ev.Op != fsevent.OpCreate {
		t.Fatalf("event = %s %s; want create", ev.Op, ev.Path)
	}
	if ev := next(t, events, "the release of the lock it was announced under"); ev.Op != fsevent.OpWrite || ev.Dirty != nil {
		t.Fatalf("event = %s %s (extents %v); want a whole-file write", ev.Op, ev.Path, ev.Dirty)
	}
}

// A holder that writes too: its release owes the other handle's write as well as
// its own, so it reports the whole file rather than its own extents — a range
// write of those alone would leave the declined write's bytes unsent.
func TestAnOwedReleaseIsAWholeFileEvent(t *testing.T) {
	backing := t.TempDir()
	if err := os.WriteFile(filepath.Join(backing, "db"), make([]byte, 64<<10), 0o644); err != nil {
		t.Fatal(err)
	}
	mnt, events := mountTest(t, backing, nil)
	drain(events)
	holder, err := os.OpenFile(filepath.Join(mnt, "db"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(holder.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	other, err := os.OpenFile(filepath.Join(mnt, "db"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.WriteAt([]byte("theirs"), 32<<10); err != nil {
		t.Fatal(err)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	if ev := next(t, events, "the other handle's close"); ev.Dirty == nil {
		t.Fatal("the other handle's own event carries no extents; the control is not testing anything")
	}
	if _, err := holder.WriteAt([]byte("mine"), 0); err != nil {
		t.Fatal(err)
	}
	if err := holder.Close(); err != nil {
		t.Fatal(err)
	}
	ev := next(t, events, "the holder's release")
	if ev.Op != fsevent.OpWrite || ev.Dirty != nil {
		t.Fatalf("event = %s %s (extents %v); want a whole-file write", ev.Op, ev.Path, ev.Dirty)
	}
	quiet(t, events, "one event for the release")
}
