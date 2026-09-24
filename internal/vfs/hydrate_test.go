// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package vfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/syshlted/drivel/internal/fsevent"
	"github.com/syshlted/drivel/internal/mount"
	"github.com/syshlted/drivel/internal/testenv"
)

// testHydrator is a mount.Hydrator over a real backing dir: "placeholders" are
// sparse files it knows the true content for, and Hydrate writes those bytes in.
// It counts fetches so a test can assert hydrate-once and no-fetch-on-truncate.
type testHydrator struct {
	dir string

	mu       sync.Mutex
	holes    map[string]string // rel -> the content a fetch would produce
	fetches  atomic.Int32
	failWith error
}

func newTestHydrator(dir string) *testHydrator {
	return &testHydrator{dir: dir, holes: map[string]string{}}
}

// place creates a sparse placeholder in the backing dir: right size, no bytes.
func (h *testHydrator) place(t *testing.T, rel, content string) {
	t.Helper()
	p := filepath.Join(h.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(int64(len(content))); err != nil {
		t.Fatal(err)
	}
	f.Close()
	h.mu.Lock()
	h.holes[rel] = content
	h.mu.Unlock()
}

func (h *testHydrator) IsPlaceholder(rel string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.holes[rel]
	return ok
}

func (h *testHydrator) Hydrate(_ context.Context, rel string) error {
	h.fetches.Add(1)
	if h.failWith != nil {
		return h.failWith
	}
	h.mu.Lock()
	content, ok := h.holes[rel]
	h.mu.Unlock()
	if !ok {
		return nil
	}
	// Write in place, exactly as the real hydrator does: an fd may already be open
	// on this inode.
	f, err := os.OpenFile(filepath.Join(h.dir, filepath.FromSlash(rel)), os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		return err
	}
	h.mu.Lock()
	delete(h.holes, rel)
	h.mu.Unlock()
	return nil
}

func (h *testHydrator) Discard(rel string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.holes, rel)
	return nil
}

var _ mount.Hydrator = (*testHydrator)(nil)

// mountTest brings up a real FUSE mount over backing and tears it down when the
// test ends. It skips when the environment cannot mount (no /dev/fuse, no
// fusermount3, unprivileged container).
//
// Each opt is applied to the mount.Options before Serve, for the tests that are
// about an option rather than about a file operation.
func mountTest(t *testing.T, backing string, hyd mount.Hydrator, opts ...func(*mount.Options)) (mnt string, events <-chan fsevent.Event) {
	t.Helper()
	if _, err := os.Stat("/dev/fuse"); err != nil {
		testenv.Unavailable(t, testenv.FUSE, "no /dev/fuse: "+err.Error())
	}

	mnt = t.TempDir()
	ch := make(chan fsevent.Event, 256)
	ctx, cancel := context.WithCancel(context.Background())

	o := mount.Options{
		Mountpoint: mnt,
		Backing:    backing,
		Events:     ch,
		FsName:     "drivel-test",
		Hydrator:   hyd,
	}
	for _, apply := range opts {
		apply(&o)
	}

	served := make(chan error, 1)
	go func() {
		served <- NewBackend().Serve(ctx, o)
	}()

	// Wait for the mount to come live: until it does, the mountpoint is just an
	// empty temp dir, so readiness has to be observed rather than assumed.
	probe := filepath.Join(backing, ".mount-probe")
	if err := os.WriteFile(probe, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(probe)

	deadline := time.Now().Add(10 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(mnt, ".mount-probe")); err == nil {
			ready = true
			break
		}
		select {
		case err := <-served:
			cancel()
			testenv.Unavailable(t, testenv.FUSE, fmt.Sprintf("Serve returned before the mount came up: %v", err))
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ready {
		cancel()
		testenv.Unavailable(t, testenv.FUSE, "mount did not become ready within 10s")
	}

	t.Cleanup(func() {
		cancel()
		select {
		case <-served:
		case <-time.After(10 * time.Second):
			t.Log("mount did not unmount cleanly; try: fusermount3 -u " + mnt)
		}
	})
	return mnt, ch
}

// Reading a placeholder through the mount faults its content in and returns the
// real bytes — the core M5 promise.
func TestReadHydratesPlaceholder(t *testing.T) {
	const body = "content that was never resident until now"
	backing := t.TempDir()
	hyd := newTestHydrator(backing)
	hyd.place(t, "lazy.txt", body)

	mnt, _ := mountTest(t, backing, hyd)

	got, err := os.ReadFile(filepath.Join(mnt, "lazy.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Errorf("read = %q; want %q", got, body)
	}
	if n := hyd.fetches.Load(); n != 1 {
		t.Errorf("fetches = %d; want 1", n)
	}

	// A second read is served from the now-resident backing file.
	if _, err := os.ReadFile(filepath.Join(mnt, "lazy.txt")); err != nil {
		t.Fatal(err)
	}
	if n := hyd.fetches.Load(); n != 1 {
		t.Errorf("fetches after re-read = %d; want 1 (content is resident)", n)
	}
}

// stat must report the placeholder's true size without fetching anything —
// otherwise `ls -l` on a lazy tree would download it.
func TestStatDoesNotHydrate(t *testing.T) {
	const body = "0123456789abcdef"
	backing := t.TempDir()
	hyd := newTestHydrator(backing)
	hyd.place(t, "lazy.txt", body)

	mnt, _ := mountTest(t, backing, hyd)

	info, err := os.Stat(filepath.Join(mnt, "lazy.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != int64(len(body)) {
		t.Errorf("size = %d; want %d", info.Size(), len(body))
	}
	if n := hyd.fetches.Load(); n != 0 {
		t.Errorf("stat triggered %d fetch(es); want 0", n)
	}
}

// A truncating open replaces the content wholesale, so fetching the old bytes
// first would be pure waste.
func TestTruncatingOpenDiscardsWithoutFetching(t *testing.T) {
	backing := t.TempDir()
	hyd := newTestHydrator(backing)
	hyd.place(t, "lazy.txt", "old remote content")

	mnt, _ := mountTest(t, backing, hyd)

	f, err := os.OpenFile(filepath.Join(mnt, "lazy.txt"), os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("brand new"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if n := hyd.fetches.Load(); n != 0 {
		t.Errorf("a truncating open fetched %d time(s); want 0", n)
	}
	got, err := os.ReadFile(filepath.Join(backing, "lazy.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "brand new" {
		t.Errorf("backing content = %q; want %q", got, "brand new")
	}
	if hyd.IsPlaceholder("lazy.txt") {
		t.Error("the placeholder mark should have been discarded")
	}
}

// Opening for a partial write hydrates first: writing into an unhydrated sparse
// file would surround the written extent with zeros.
func TestWriteOpenHydratesFirst(t *testing.T) {
	const body = "AAAAAAAAAAAAAAAAAAAA"
	backing := t.TempDir()
	hyd := newTestHydrator(backing)
	hyd.place(t, "lazy.txt", body)

	mnt, _ := mountTest(t, backing, hyd)

	f, err := os.OpenFile(filepath.Join(mnt, "lazy.txt"), os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("BB"), 0); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if n := hyd.fetches.Load(); n != 1 {
		t.Errorf("fetches = %d; want 1 (a partial write needs the rest of the file)", n)
	}
	got, err := os.ReadFile(filepath.Join(backing, "lazy.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "BB" + body[2:]; string(got) != want {
		t.Errorf("content = %q; want %q — the untouched tail must survive", got, want)
	}
}

// A failed hydration surfaces as an I/O error, never as a short read of zeros:
// serving a hole as content would be silent corruption.
func TestFailedHydrationReturnsEIO(t *testing.T) {
	backing := t.TempDir()
	hyd := newTestHydrator(backing)
	hyd.place(t, "lazy.txt", "unreachable")
	hyd.failWith = errors.New("network is down")

	mnt, _ := mountTest(t, backing, hyd)

	_, err := os.ReadFile(filepath.Join(mnt, "lazy.txt"))
	if err == nil {
		t.Fatal("read of an unhydratable placeholder should fail, not return zeros")
	}
	if !errors.Is(err, syscall.EIO) {
		t.Errorf("err = %v; want EIO", err)
	}
}

// Files that are not placeholders pass straight through, unchanged from M1–M4.
func TestNonPlaceholderReadsPassThrough(t *testing.T) {
	backing := t.TempDir()
	hyd := newTestHydrator(backing)
	if err := os.WriteFile(filepath.Join(backing, "normal.txt"), []byte("resident"), 0o644); err != nil {
		t.Fatal(err)
	}

	mnt, _ := mountTest(t, backing, hyd)

	got, err := os.ReadFile(filepath.Join(mnt, "normal.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "resident" {
		t.Errorf("read = %q; want %q", got, "resident")
	}
	if n := hyd.fetches.Load(); n != 0 {
		t.Errorf("a resident file triggered %d fetch(es); want 0", n)
	}
}

// With no hydrator wired (eager mode), the mount behaves exactly as before.
func TestEagerModeUnaffected(t *testing.T) {
	backing := t.TempDir()
	if err := os.WriteFile(filepath.Join(backing, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	mnt, events := mountTest(t, backing, nil)

	if err := os.WriteFile(filepath.Join(mnt, "b.txt"), []byte("written"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(mnt, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Errorf("read = %q; want %q", got, "hello")
	}
	// The mutation still surfaces as a change event.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-events:
			if ev.Path == "b.txt" {
				return
			}
		case <-deadline:
			t.Fatal("no change event for b.txt")
		}
	}
}

// Opening a placeholder without reading it must not fetch anything. This is what
// deferring hydration from Open to first I/O buys: tools that open files to probe
// them (and directory walks generally) stay free on a lazy tree.
func TestOpenWithoutReadDoesNotHydrate(t *testing.T) {
	backing := t.TempDir()
	hyd := newTestHydrator(backing)
	hyd.place(t, "lazy.txt", "never fetched")

	mnt, _ := mountTest(t, backing, hyd)

	f, err := os.Open(filepath.Join(mnt, "lazy.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Stat(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if n := hyd.fetches.Load(); n != 0 {
		t.Errorf("open+stat+close fetched %d time(s); want 0", n)
	}
	if !hyd.IsPlaceholder("lazy.txt") {
		t.Error("the file should still be a placeholder")
	}
}

// Concurrent readers of one placeholder must all get correct content.
func TestConcurrentReadsOfPlaceholder(t *testing.T) {
	const body = "shared content read by many"
	backing := t.TempDir()
	hyd := newTestHydrator(backing)
	hyd.place(t, "lazy.txt", body)

	mnt, _ := mountTest(t, backing, hyd)

	var wg sync.WaitGroup
	results := make([]string, 8)
	errs := make([]error, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			b, err := os.ReadFile(filepath.Join(mnt, "lazy.txt"))
			results[i], errs[i] = string(b), err
		}(i)
	}
	wg.Wait()

	for i := range results {
		if errs[i] != nil {
			t.Errorf("reader %d: %v", i, errs[i])
			continue
		}
		if results[i] != body {
			t.Errorf("reader %d got %q; want %q", i, results[i], body)
		}
	}
}
