package hydrate

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zishmusic/drivel/internal/provider"
	"github.com/zishmusic/drivel/internal/state"
	"github.com/zishmusic/drivel/internal/testenv"
)

// The state store is the intended Cache implementation; this pins the contract so
// a change to either side breaks the build rather than the mount.
var _ Cache = (*state.Store)(nil)

// fakeStore serves fixed content and counts Get calls, so a test can assert that
// hydration fetched exactly once (or not at all).
type fakeStore struct {
	content map[string]string
	gets    atomic.Int32
	err     error         // returned by Get when non-nil
	delay   time.Duration // held before serving, to widen the singleflight window
}

func (f *fakeStore) Get(_ context.Context, p string) (io.ReadCloser, error) {
	f.gets.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	c, ok := f.content[p]
	if !ok {
		return nil, errors.New("no such object: " + p)
	}
	return io.NopCloser(strings.NewReader(c)), nil
}

func (f *fakeStore) Put(context.Context, string, io.Reader) (provider.RemoteFile, error) {
	return provider.RemoteFile{}, errors.New("unexpected Put")
}
func (f *fakeStore) Mkdir(context.Context, string) (provider.RemoteFile, error) {
	return provider.RemoteFile{}, errors.New("unexpected Mkdir")
}
func (f *fakeStore) Move(context.Context, string, string) (provider.RemoteFile, error) {
	return provider.RemoteFile{}, errors.New("unexpected Move")
}
func (f *fakeStore) Remove(context.Context, string) error { return errors.New("unexpected Remove") }
func (f *fakeStore) Stat(context.Context, string) (provider.RemoteFile, bool, error) {
	return provider.RemoteFile{}, false, nil
}

// newHydrator returns a Hydrator over a temp backing dir, skipping the test when
// the filesystem cannot store the authoritative marker.
func newHydrator(t *testing.T, content map[string]string) (*Hydrator, *fakeStore, string) {
	t.Helper()
	dir := t.TempDir()
	fs := &fakeStore{content: content}
	h := New(dir, fs, nil)
	if !h.XattrsUsable() {
		testenv.Unavailable(t, testenv.Xattr, "backing filesystem does not store user.* attributes")
	}
	return h, fs, dir
}

func remote(p, content string) provider.RemoteFile {
	return provider.RemoteFile{
		Path:     p,
		Size:     int64(len(content)),
		Hash:     "h-" + p,
		Version:  "1",
		Modified: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
	}
}

// A placeholder has the remote file's apparent size and mtime but none of its
// bytes, and is recognised as a placeholder.
func TestCreatePlaceholder(t *testing.T) {
	const body = "hello placeholder world"
	h, fs, dir := newHydrator(t, map[string]string{"a.txt": body})
	rf := remote("a.txt", body)

	if err := h.CreatePlaceholder("a.txt", rf); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(filepath.Join(dir, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != rf.Size {
		t.Errorf("size = %d; want %d (ls -l must not lie about a placeholder)", info.Size(), rf.Size)
	}
	if !info.ModTime().Equal(rf.Modified) {
		t.Errorf("mtime = %v; want %v", info.ModTime(), rf.Modified)
	}
	if !h.IsPlaceholder("a.txt") {
		t.Error("IsPlaceholder = false; want true")
	}
	if fs.gets.Load() != 0 {
		t.Errorf("creating a placeholder fetched content %d time(s); want 0", fs.gets.Load())
	}

	m, ok, err := h.Marker("a.txt")
	if err != nil || !ok {
		t.Fatalf("Marker() = %v, %t, %v; want a marker", m, ok, err)
	}
	if m.Size != rf.Size || m.Hash != rf.Hash {
		t.Errorf("marker = %+v; want size %d hash %q", m, rf.Size, rf.Hash)
	}
}

// Hydration fills the content, clears the marker, and restores the remote mtime
// (so the downloader's last-writer-wins comparison doesn't read hydration as a
// local edit).
func TestHydrateFillsContentAndClearsMarker(t *testing.T) {
	const body = "the quick brown fox"
	h, fs, dir := newHydrator(t, map[string]string{"sub/a.txt": body})
	rf := remote("sub/a.txt", body)

	if err := h.CreatePlaceholder("sub/a.txt", rf); err != nil {
		t.Fatal(err)
	}
	if err := h.Hydrate(context.Background(), "sub/a.txt"); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "sub", "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Errorf("content = %q; want %q", got, body)
	}
	if h.IsPlaceholder("sub/a.txt") {
		t.Error("still marked a placeholder after hydration")
	}
	if fs.gets.Load() != 1 {
		t.Errorf("Get called %d times; want 1", fs.gets.Load())
	}
	info, err := os.Stat(filepath.Join(dir, "sub", "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(rf.Modified) {
		t.Errorf("mtime = %v; want the remote mtime %v restored after writing", info.ModTime(), rf.Modified)
	}

	// Hydrating again is a no-op, not a second fetch.
	if err := h.Hydrate(context.Background(), "sub/a.txt"); err != nil {
		t.Fatal(err)
	}
	if fs.gets.Load() != 1 {
		t.Errorf("re-hydrating fetched again (%d gets); want 1", fs.gets.Load())
	}
}

// This is the M5 data-loss guard. A placeholder must never look like a real empty
// file to the uploader, and hydration must be what clears that.
func TestPlaceholderIsNeverMistakenForEmptyFile(t *testing.T) {
	const body = "important remote content"
	h, _, dir := newHydrator(t, map[string]string{"doc.txt": body})

	if err := h.CreatePlaceholder("doc.txt", remote("doc.txt", body)); err != nil {
		t.Fatal(err)
	}
	// The bytes on disk really are absent — this is precisely why uploading it
	// would destroy the remote copy.
	raw, err := os.ReadFile(filepath.Join(dir, "doc.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, make([]byte, len(body))) {
		t.Fatalf("placeholder should read as zeros, got %q", raw)
	}
	if !h.IsPlaceholder("doc.txt") {
		t.Fatal("the guard must report true while the bytes are absent")
	}
	if err := h.Hydrate(context.Background(), "doc.txt"); err != nil {
		t.Fatal(err)
	}
	if h.IsPlaceholder("doc.txt") {
		t.Error("the guard must report false once the bytes are resident")
	}
}

// A failed hydration must leave the file marked, so the next open retries instead
// of exposing (or uploading) a hole. This is the crash/failure ordering: the
// marker is removed only after a successful, synced write.
func TestFailedHydrationKeepsPlaceholderMark(t *testing.T) {
	dir := t.TempDir()
	fs := &fakeStore{content: map[string]string{}, err: errors.New("network is down")}
	h := New(dir, fs, nil)
	if !h.XattrsUsable() {
		testenv.Unavailable(t, testenv.Xattr, "backing filesystem does not store user.* attributes")
	}
	if err := h.CreatePlaceholder("a.txt", remote("a.txt", "0123456789")); err != nil {
		t.Fatal(err)
	}

	if err := h.Hydrate(context.Background(), "a.txt"); err == nil {
		t.Fatal("Hydrate should report the fetch failure")
	}
	if !h.IsPlaceholder("a.txt") {
		t.Error("a failed hydration must leave the placeholder mark in place")
	}
}

// Discard drops the mark without fetching — the right answer for a truncating
// open, where the caller is replacing the content wholesale.
func TestDiscardDoesNotFetch(t *testing.T) {
	h, fs, _ := newHydrator(t, map[string]string{"a.txt": "abcdef"})
	if err := h.CreatePlaceholder("a.txt", remote("a.txt", "abcdef")); err != nil {
		t.Fatal(err)
	}
	if err := h.Discard("a.txt"); err != nil {
		t.Fatal(err)
	}
	if h.IsPlaceholder("a.txt") {
		t.Error("Discard should clear the placeholder mark")
	}
	if fs.gets.Load() != 0 {
		t.Errorf("Discard fetched content %d time(s); want 0", fs.gets.Load())
	}
	// Discarding a file that was never a placeholder is a no-op, not an error.
	if err := h.Discard("a.txt"); err != nil {
		t.Errorf("second Discard: %v", err)
	}
	if err := h.Discard("never-existed.txt"); err != nil {
		t.Errorf("Discard of an absent path: %v", err)
	}
}

// Concurrent opens of the same placeholder must fault the content in exactly once.
func TestHydrateSingleflight(t *testing.T) {
	const body = "concurrent"
	dir := t.TempDir()
	fs := &fakeStore{content: map[string]string{"a.txt": body}, delay: 50 * time.Millisecond}
	h := New(dir, fs, nil)
	if !h.XattrsUsable() {
		testenv.Unavailable(t, testenv.Xattr, "backing filesystem does not store user.* attributes")
	}
	if err := h.CreatePlaceholder("a.txt", remote("a.txt", body)); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = h.Hydrate(context.Background(), "a.txt")
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("hydrate %d: %v", i, err)
		}
	}
	if n := fs.gets.Load(); n != 1 {
		t.Errorf("Get called %d times for 8 concurrent opens; want 1", n)
	}
	got, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Errorf("content = %q; want %q", got, body)
	}
}

// A file that is not a placeholder — and one that does not exist — must not be
// reported as one, or the uploader would silently stop pushing real edits.
func TestNonPlaceholdersReportFalse(t *testing.T) {
	h, _, dir := newHydrator(t, nil)
	if err := os.WriteFile(filepath.Join(dir, "real.txt"), []byte("local edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	if h.IsPlaceholder("real.txt") {
		t.Error("a normal file must not be reported as a placeholder")
	}
	if h.IsPlaceholder("absent.txt") {
		t.Error("a missing file must not be reported as a placeholder")
	}
	// A genuinely empty local file is not a placeholder either — this is why the
	// marker exists rather than a size heuristic.
	if err := os.WriteFile(filepath.Join(dir, "empty.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if h.IsPlaceholder("empty.txt") {
		t.Error("a genuinely empty file must not be reported as a placeholder")
	}
}

// A corrupt marker is treated as an unhydrated placeholder: re-fetching content we
// may already have is recoverable, uploading content we may not have is not.
func TestCorruptMarkerFailsSafe(t *testing.T) {
	h, _, dir := newHydrator(t, map[string]string{"a.txt": "abc"})
	p := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := setxattr(p, XattrName, []byte("{not json")); err != nil {
		t.Fatal(err)
	}
	if !h.IsPlaceholder("a.txt") {
		t.Error("a corrupt marker must fail safe and report a placeholder")
	}
}

// The range bitmap is persisted through the cache: empty on creation, complete
// after hydration. M5 only stores these two states, but via the full schema.
func TestRangesTrackHydration(t *testing.T) {
	const body = "0123456789"
	dir := t.TempDir()
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	fs := &fakeStore{content: map[string]string{"a.txt": body}}
	h := New(dir, fs, st)
	if !h.XattrsUsable() {
		testenv.Unavailable(t, testenv.Xattr, "backing filesystem does not store user.* attributes")
	}

	if err := h.CreatePlaceholder("a.txt", remote("a.txt", body)); err != nil {
		t.Fatal(err)
	}
	rs, ok := h.Ranges("a.txt")
	if !ok {
		t.Fatal("no range set cached after CreatePlaceholder")
	}
	if !rs.Empty() {
		t.Error("a fresh placeholder should have no resident blocks")
	}

	if err := h.Hydrate(context.Background(), "a.txt"); err != nil {
		t.Fatal(err)
	}
	rs, ok = h.Ranges("a.txt")
	if !ok {
		t.Fatal("no range set cached after Hydrate")
	}
	if !rs.Complete() {
		t.Error("a hydrated file should be Complete")
	}
}

// Re-stamping an existing placeholder (the remote changed before anyone read it)
// updates its size without fetching anything.
func TestRestampPlaceholder(t *testing.T) {
	h, fs, dir := newHydrator(t, map[string]string{"a.txt": "grown much longer"})
	if err := h.CreatePlaceholder("a.txt", remote("a.txt", "short")); err != nil {
		t.Fatal(err)
	}
	if err := h.CreatePlaceholder("a.txt", remote("a.txt", "grown much longer")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != int64(len("grown much longer")) {
		t.Errorf("size = %d; want %d", info.Size(), len("grown much longer"))
	}
	if !h.IsPlaceholder("a.txt") {
		t.Error("a re-stamped placeholder is still a placeholder")
	}
	if fs.gets.Load() != 0 {
		t.Errorf("re-stamping fetched content %d time(s); want 0", fs.gets.Load())
	}
}
