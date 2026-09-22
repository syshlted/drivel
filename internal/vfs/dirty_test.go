// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package vfs

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/zishmusic/drivel/internal/fsevent"
	"github.com/zishmusic/drivel/ranges"
)

// --- dirtyTracker (pure, no mount) ------------------------------------------

// An untouched handle reports nothing to push.
func TestDirtyTrackerUntouched(t *testing.T) {
	var d dirtyTracker
	written, extents := d.snapshot(0)
	if written {
		t.Error("an untouched handle should not report a write")
	}
	if extents != nil {
		t.Errorf("an untouched handle should have no extents, got %v", extents)
	}
}

// Writes accumulate into block extents, and the file's size grows to cover them.
func TestDirtyTrackerAccumulatesExtents(t *testing.T) {
	const bs = ranges.DefaultBlockSize
	var d dirtyTracker
	d.mark(0, 10)      // block 0
	d.mark(3*bs, 4096) // block 3

	// The file is 5 blocks long; the tracker only knows that from the size it is
	// handed, and both marked blocks must come out full-width.
	written, extents := d.snapshot(5 * bs)
	if !written || extents == nil {
		t.Fatalf("want a tracked write with extents; got written=%v extents=%v", written, extents)
	}
	want := []ranges.Range{{Off: 0, Len: bs}, {Off: 3 * bs, Len: bs}}
	if got := extents.Extents(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Extents() = %v; want %v", got, want)
	}
}

// A short write records only the bytes the kernel accepted.
func TestDirtyTrackerIgnoresEmptyWrites(t *testing.T) {
	var d dirtyTracker
	d.mark(0, 0)
	if written, _ := d.snapshot(0); written {
		t.Error("a zero-length write should not mark the handle written")
	}
}

// A resize poisons the extent map: the offsets no longer describe the file, so
// the handle must report "unknown" and let the engine push everything.
func TestDirtyTrackerPoisonDropsExtents(t *testing.T) {
	var d dirtyTracker
	d.mark(0, 10)
	d.poison()

	written, extents := d.snapshot(1 << 20)
	if !written {
		t.Error("poisoning must not erase the fact that a write happened")
	}
	if extents != nil {
		t.Errorf("a poisoned tracker must report unknown extents, got %v", extents.Extents())
	}
}

// Poisoning alone does not manufacture a push: a bare truncate through an open
// handle stays metadata-only, as it was before M6.
func TestDirtyTrackerPoisonAloneDoesNotTriggerPush(t *testing.T) {
	var d dirtyTracker
	d.poison()
	if written, _ := d.snapshot(0); written {
		t.Error("poison without any write must not report the handle written")
	}
}

// The snapshot must be a deep copy — writes can still arrive on another thread
// sharing the handle while the first snapshot is in flight on the event channel.
func TestDirtyTrackerSnapshotIsIsolated(t *testing.T) {
	const bs = ranges.DefaultBlockSize
	var d dirtyTracker
	d.mark(0, 10)

	_, snap := d.snapshot(8 * bs)
	d.mark(5*bs, 10)

	if got := len(snap.Extents()); got != 1 {
		t.Fatalf("snapshot saw %d extents after a later write; want 1", got)
	}
}

// --- end to end through a real mount ----------------------------------------

// waitEvent pulls events until one matches op+path, or the deadline passes.
func waitEvent(t *testing.T, events <-chan fsevent.Event, op fsevent.Op, path string) fsevent.Event {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-events:
			if ev.Op == op && ev.Path == path {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s %s", op, path)
		}
	}
}

// A partial write to an existing file surfaces on the event as the extents it
// touched, not as the whole file — the M6 promise the uploader depends on.
func TestWriteEmitsDirtyExtents(t *testing.T) {
	const bs = ranges.DefaultBlockSize
	backing := t.TempDir()

	// 12 MiB over 4 MiB blocks: three blocks, so a middle-block edit is provably
	// narrower than the file.
	big := make([]byte, 3*bs)
	if err := os.WriteFile(filepath.Join(backing, "big.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}

	mnt, events := mountTest(t, backing, nil)
	drain(events)

	f, err := os.OpenFile(filepath.Join(mnt, "big.bin"), os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("patched"), bs+1024); err != nil { // inside block 1
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	ev := waitEvent(t, events, fsevent.OpWrite, "big.bin")
	if ev.Dirty == nil {
		t.Fatal("a plain partial write should report its extents, not unknown")
	}
	want := []ranges.Range{{Off: bs, Len: bs}}
	if got := ev.Dirty.Extents(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Dirty.Extents() = %v; want %v (only the touched block)", got, want)
	}
	if ev.Dirty.Size != 3*bs {
		t.Fatalf("Dirty.Size = %d; want %d (the file's length)", ev.Dirty.Size, 3*bs)
	}
}

// Truncating through the same handle that wrote invalidates the offsets, so the
// event must report unknown extents and get a whole-file push.
func TestTruncateThroughHandleReportsUnknownExtents(t *testing.T) {
	const bs = ranges.DefaultBlockSize
	backing := t.TempDir()
	if err := os.WriteFile(filepath.Join(backing, "big.bin"), make([]byte, 3*bs), 0o644); err != nil {
		t.Fatal(err)
	}

	mnt, events := mountTest(t, backing, nil)
	drain(events)

	f, err := os.OpenFile(filepath.Join(mnt, "big.bin"), os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("patched"), bs+1024); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(2 * bs); err != nil { // moves the file's end under our extents
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	ev := waitEvent(t, events, fsevent.OpWrite, "big.bin")
	if ev.Dirty != nil {
		t.Fatalf("a resize must invalidate the extent map; got %v", ev.Dirty.Extents())
	}
}

// Eager mode (no hydrator) tracks extents just the same: M6 is not gated on M5.
func TestDirtyTrackingIndependentOfLazyMode(t *testing.T) {
	const bs = ranges.DefaultBlockSize
	backing := t.TempDir()
	if err := os.WriteFile(filepath.Join(backing, "f.bin"), make([]byte, 2*bs), 0o644); err != nil {
		t.Fatal(err)
	}

	hyd := newTestHydrator(backing) // lazy mode, but f.bin is fully resident
	mnt, events := mountTest(t, backing, hyd)
	drain(events)

	f, err := os.OpenFile(filepath.Join(mnt, "f.bin"), os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("x"), 0); err != nil {
		t.Fatal(err)
	}
	f.Close()

	ev := waitEvent(t, events, fsevent.OpWrite, "f.bin")
	if ev.Dirty == nil {
		t.Fatal("lazy mode should still report extents for a resident file")
	}
	if got, want := ev.Dirty.Extents(), []ranges.Range{{Off: 0, Len: bs}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Extents() = %v; want %v", got, want)
	}
}

// drain empties any events buffered by test setup so a later wait sees only the
// events the test itself provoked.
func drain(events <-chan fsevent.Event) {
	for {
		select {
		case <-events:
		default:
			return
		}
	}
}

// A file that shrank behind this handle's back makes every recorded offset
// suspect, so the snapshot reports unknown rather than extents past the new EOF.
func TestDirtyTrackerShrunkFileReportsUnknown(t *testing.T) {
	const bs = ranges.DefaultBlockSize
	var d dirtyTracker
	d.mark(3*bs, 10)

	written, extents := d.snapshot(2 * bs) // file is now shorter than our marks
	if !written {
		t.Error("the write still happened")
	}
	if extents != nil {
		t.Errorf("extents past the new EOF must be reported unknown, got %v", extents.Extents())
	}
}

// An unreadable size (-1) degrades to unknown, never to a set measured against
// a length nobody verified.
func TestDirtyTrackerUnknownSizeReportsUnknown(t *testing.T) {
	var d dirtyTracker
	d.mark(0, 10)
	if _, extents := d.snapshot(-1); extents != nil {
		t.Errorf("an unknown size must yield unknown extents, got %v", extents.Extents())
	}
}
