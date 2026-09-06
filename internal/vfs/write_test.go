package vfs

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/zishmusic/drivel/internal/fsevent"
)

// A partial write through a handle the client opened write-only has to land, and
// has to leave the bytes around it alone.
//
// The test is nearly vacuous on Linux, whose FUSE write path does not read back
// through the write handle, and it is the whole guard on FreeBSD, where fusefs
// fills a cache block by issuing READ against whatever handle it has — write-only
// included. Before node.Open opened the backing file readable regardless of the
// client's access mode, this failed with EBADF from a pread on a write-only fd,
// reported to the caller as the write failing. Nothing in the suite caught it,
// because on Linux every shape of it passes.
//
// The two assertions are separate claims. That the write succeeds says the read
// side of the handle works; that the surrounding bytes survive says the kernel's
// read-modify-write got real content rather than a short read of zeros, which is
// the failure that would corrupt a file quietly instead of loudly.
func TestPartialWriteThroughWriteOnlyHandle(t *testing.T) {
	const (
		size  = 12 * 1024 // more than a page, less than a cache block: no write covers a whole one
		off   = 1024      // deliberately not aligned to anything
		fill  = 0xA5
		patch = "patched"
	)
	backing := t.TempDir()
	body := bytes.Repeat([]byte{fill}, size)
	if err := os.WriteFile(filepath.Join(backing, "f.bin"), body, 0o644); err != nil {
		t.Fatal(err)
	}

	mnt, events := mountTest(t, backing, nil)
	drain(events)

	f, err := os.OpenFile(filepath.Join(mnt, "f.bin"), os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte(patch), off); err != nil {
		t.Fatalf("partial write through an O_WRONLY handle: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, events, fsevent.OpWrite, "f.bin")

	got, err := os.ReadFile(filepath.Join(backing, "f.bin"))
	if err != nil {
		t.Fatal(err)
	}
	want := append([]byte(nil), body...)
	copy(want[off:], patch)
	if !bytes.Equal(got, want) {
		if len(got) != len(want) {
			t.Fatalf("backing file is %d bytes; want %d", len(got), len(want))
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("backing file differs at byte %d: got %#x, want %#x", i, got[i], want[i])
			}
		}
	}
}
