// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package plugin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/syshlted/drivel/provider"
	"github.com/syshlted/drivel/ranges"
)

// These tests launch a real plugin process.
//
// Everything cheaper was considered and rejected. A gRPC round trip over a
// bufconn would exercise the protocol but not the thing M9 actually adds — that
// a backend in another process can be started, talked to, crash, and be started
// again — and every bug this package can have that matters is on that path. The
// fake backend under testdata is built once per run and driven entirely by the
// TOML settings each test sends it, so the cost is one `go build`.

// fakeBin builds the fake provider once per test binary and returns a directory
// laid out the way Loader expects to find one.
var fakeBin = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "drivel-plugin-test-")
	if err != nil {
		return "", err
	}
	out := filepath.Join(dir, BinaryPrefix+"fake")
	cmd := exec.Command("go", "build", "-o", out, "./testdata/"+BinaryPrefix+"fake")
	if combined, berr := cmd.CombinedOutput(); berr != nil {
		return "", fmt.Errorf("building the fake plugin: %w\n%s", berr, combined)
	}
	return dir, nil
})

// pluginDir returns a directory holding the fake plugin, skipping the test if
// the toolchain is not available to build it.
func pluginDir(t *testing.T) string {
	t.Helper()
	dir, err := fakeBin()
	if err != nil {
		t.Fatalf("%v", err)
	}
	return dir
}

// logBuf collects a mount's log output so a test can assert on what was
// reported, which for this package is half the behaviour: a plugin that failed
// silently is the failure mode.
type logBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// openFake launches the fake backend with the given settings.
func openFake(t *testing.T, cfg map[string]any) (provider.Store, *logBuf) {
	t.Helper()
	dir := pluginDir(t)
	lb := &logBuf{}
	lg := log.New(lb, "", 0)

	l := NewLoader([]string{dir}, lg)
	f, err := l.Factory("fake")
	if err != nil {
		t.Fatalf("Factory: %v", err)
	}
	store, err := f(context.Background(), provider.Params{
		Config: provider.MustEncodeConfig(cfg),
		Log:    lg,
	})
	if err != nil {
		t.Fatalf("opening the fake plugin: %v", err)
	}
	t.Cleanup(func() {
		if c, ok := store.(io.Closer); ok {
			_ = c.Close()
		}
	})
	return store, lb
}

func TestPluginRoundTripsTheRequiredSurface(t *testing.T) {
	store, _ := openFake(t, map[string]any{})
	ctx := context.Background()

	rf, err := store.Put(ctx, "a/b.txt", strings.NewReader("hello plugin"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if rf.Path != "a/b.txt" || rf.Size != 12 {
		t.Errorf("Put returned %+v", rf)
	}
	// A modification time has to survive the wire intact. It travels as a
	// message rather than as a Unix count precisely so that the zero time stays
	// distinguishable, and the non-zero case is what proves the encoding is not
	// just always dropping it.
	if got := rf.Modified.UTC(); got != fixedFakeTime {
		t.Errorf("Modified = %v; want %v", got, fixedFakeTime)
	}

	got, ok, err := store.Stat(ctx, "a/b.txt")
	if err != nil || !ok {
		t.Fatalf("Stat: %v ok=%v", err, ok)
	}
	if got.Hash != "len:12" || got.Version != "v12" {
		t.Errorf("Stat returned %+v", got)
	}

	rc, err := store.Get(ctx, "a/b.txt")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	body, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(body) != "hello plugin" {
		t.Errorf("Get returned %q", body)
	}

	if _, err := store.Move(ctx, "a/b.txt", "c.txt"); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if _, ok, _ := store.Stat(ctx, "a/b.txt"); ok {
		t.Error("the old path still exists after Move")
	}

	if err := store.Remove(ctx, "c.txt"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	// Absence is an ordinary answer, not an error. It travels in the response for
	// that reason; if it ever came back as a status code, this is what would fail.
	if _, ok, err := store.Stat(ctx, "c.txt"); err != nil || ok {
		t.Errorf("Stat after Remove: ok=%v err=%v", ok, err)
	}
}

// A large transfer has to cross the seam in chunks and arrive byte for byte.
// This is the test that would catch a chunking bug, which would otherwise
// surface as silently corrupted content on a file big enough that nobody was
// looking.
func TestPluginStreamsLargeContent(t *testing.T) {
	store, _ := openFake(t, map[string]any{})
	ctx := context.Background()

	body := make([]byte, 3*chunkSize+1237)
	for i := range body {
		body[i] = byte(i * 7)
	}
	if _, err := store.Put(ctx, "big.bin", bytes.NewReader(body)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rc, err := store.Get(ctx, "big.bin")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("round trip changed the content: %d bytes in, %d out", len(body), len(got))
	}
}

// A local file that fails to read half way through must fail the push, not
// complete it with what was read.
//
// This is the one place where the plugin seam could quietly behave differently
// from an in-process backend: streaming means the backend can be handed a
// perfectly well-formed prefix and told the file ended there, so it stores a
// truncated object and reports success — and the engine then records an echo for
// content the local file does not hold. In process the read error simply
// propagates out of Put.
func TestPluginFailsAPutWhoseLocalReadFails(t *testing.T) {
	store, _ := openFake(t, map[string]any{})
	ctx := context.Background()

	boom := errors.New("disk went away")
	r := io.MultiReader(
		bytes.NewReader(bytes.Repeat([]byte("a"), chunkSize+16)),
		failingReader{err: boom},
	)
	if _, err := store.Put(ctx, "half.txt", r); err == nil {
		t.Fatal("a Put whose source failed mid-read reported success")
	} else if !errors.Is(err, boom) {
		t.Errorf("the read error was replaced rather than reported: %v", err)
	}

	// And nothing that claims to be the file may be left behind.
	if _, ok, err := store.Stat(ctx, "half.txt"); err != nil {
		t.Fatalf("Stat: %v", err)
	} else if ok {
		t.Error("a truncated object was stored for a Put that failed")
	}
}

// failingReader fails on the first Read, standing in for a local file that
// becomes unreadable part way through an upload.
type failingReader struct{ err error }

func (f failingReader) Read([]byte) (int, error) { return 0, f.err }

// The capability answer is the whole reason provider.Capability exists. The
// proxy implements every optional interface, so without negotiation a backend
// with no change feed would look like it had one.
func TestPluginNarrowsCapabilities(t *testing.T) {
	store, _ := openFake(t, map[string]any{
		"capabilities": []string{"enumerator", "range-getter"},
	})

	caps := provider.Capabilities(store)
	if !caps.Has(provider.CapEnumerator) || !caps.Has(provider.CapRangeGetter) {
		t.Errorf("declared capabilities lost: %s", caps)
	}
	for _, c := range []provider.Capability{
		provider.CapChangeSource, provider.CapRangePutter, provider.CapContentHasher,
	} {
		if caps.Has(c) {
			t.Errorf("%s was not offered but is reported", c)
		}
	}
	if _, ok := provider.AsChangeSource(store); ok {
		t.Error("AsChangeSource succeeded for a backend with no change feed")
	}
	if _, ok := provider.AsEnumerator(store); !ok {
		t.Error("AsEnumerator failed for a backend that offers it")
	}
}

func TestPluginServesOptionalCapabilities(t *testing.T) {
	store, _ := openFake(t, map[string]any{
		"capabilities": []string{"change-source", "enumerator", "range-getter", "content-hasher"},
		"seed":         []string{"one.txt=alpha", "two.txt=beta"},
	})
	ctx := context.Background()

	cs, ok := provider.AsChangeSource(store)
	if !ok {
		t.Fatal("no change source")
	}
	cur, err := cs.StartCursor(ctx)
	if err != nil {
		t.Fatalf("StartCursor: %v", err)
	}
	changes, next, err := cs.Changes(ctx, cur)
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if next != cur+"+" {
		t.Errorf("next cursor = %q", next)
	}
	// A removal carries no file, and the nil-ness is what the engine reads in
	// more places than the boolean. It has to survive as nil rather than as a
	// zero-valued message.
	var sawRemoval bool
	for _, c := range changes {
		if c.Removed {
			sawRemoval = true
			if c.File != nil {
				t.Errorf("a removal arrived carrying a file: %+v", c.File)
			}
		} else if c.File == nil {
			t.Errorf("a change for %q arrived with no file", c.Path)
		}
	}
	if !sawRemoval {
		t.Error("no removal in the change feed")
	}
	// MovedFrom is optional in both directions, so the thing that can break it is
	// silence: a field dropped by the converter looks exactly like a backend that
	// does not report moves, and the receiving side then re-downloads a renamed
	// subtree instead of renaming it (MC-27).
	var moves int
	for _, c := range changes {
		if c.MovedFrom != "" {
			moves++
			if c.MovedFrom != "moved/old.txt" || c.Path != "moved/new.txt" {
				t.Errorf("move arrived as %q -> %q", c.MovedFrom, c.Path)
			}
		}
	}
	if moves != 1 {
		t.Errorf("%d change(s) carried MovedFrom across the seam, want 1", moves)
	}

	e, _ := provider.AsEnumerator(store)
	files, next, err := e.Enumerate(ctx, "")
	if err != nil || next != "" {
		t.Fatalf("Enumerate: %v next=%q", err, next)
	}
	if len(files) != 2 {
		t.Errorf("Enumerate returned %d files; want 2", len(files))
	}

	rg, _ := provider.AsRangeGetter(store)
	rc, err := rg.GetRange(ctx, "one.txt", 1, 3)
	if err != nil {
		t.Fatalf("GetRange: %v", err)
	}
	part, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(part) != "lph" {
		t.Errorf("GetRange returned %q; want %q", part, "lph")
	}

	h, _ := provider.AsContentHasher(store)
	sum, err := h.HashContent(strings.NewReader("abcdef"))
	if err != nil {
		t.Fatalf("HashContent: %v", err)
	}
	if sum != "len:6" {
		t.Errorf("HashContent = %q", sum)
	}
}

// PutRange is the one call where the plugin reads back from the host. The fake
// reads its extents in descending order on purpose, so a host that had quietly
// turned the io.ReaderAt into a single forward pass would fail here.
func TestPluginPutRangeReadsBackThroughTheBroker(t *testing.T) {
	store, _ := openFake(t, map[string]any{
		"capabilities": []string{"range-putter"},
		"seed":         []string{"f.bin=" + strings.Repeat("o", 32)},
	})
	ctx := context.Background()

	rp, ok := provider.AsRangePutter(store)
	if !ok {
		t.Fatal("no range putter")
	}
	local := []byte(strings.Repeat("o", 32))
	copy(local[4:], "AAAA")
	copy(local[20:], "BBBB")

	if _, err := rp.PutRange(ctx, "f.bin", bytes.NewReader(local), 32, []ranges.Range{
		{Off: 4, Len: 4}, {Off: 20, Len: 4},
	}); err != nil {
		t.Fatalf("PutRange: %v", err)
	}
	rc, err := store.Get(ctx, "f.bin")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(got, local) {
		t.Errorf("patched file = %q\n            want %q", got, local)
	}
}

// Create's refusal and Trash both cross the socket. The refusal is the shape
// that matters: the fake refuses before reading a byte, so the host is still
// sending a large body when the stream closes, and what it must report is the
// backend's reason — ErrExist — rather than the broken send.
func TestPluginServesCreateAndTrash(t *testing.T) {
	store, _ := openFake(t, map[string]any{
		"capabilities": []string{"creator", "trasher"},
		"seed":         []string{"taken.txt=old"},
	})
	ctx := context.Background()
	c, ok := provider.AsCreator(store)
	if !ok {
		t.Fatal("no creator")
	}
	big := bytes.Repeat([]byte("x"), 4<<20)
	if _, err := c.Create(ctx, "taken.txt", bytes.NewReader(big)); !errors.Is(err, provider.ErrExist) {
		t.Fatalf("Create over an occupied path = %v; want ErrExist", err)
	}
	rc, err := store.Get(ctx, "taken.txt")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != "old" {
		t.Errorf("a refused Create changed the path: %q", got)
	}
	if _, err := c.Create(ctx, "new.txt", bytes.NewReader(big)); err != nil {
		t.Fatalf("Create of a free path: %v", err)
	}

	tr, ok := provider.AsTrasher(store)
	if !ok {
		t.Fatal("no trasher")
	}
	if err := tr.Trash(ctx, "new.txt"); err != nil {
		t.Fatalf("Trash: %v", err)
	}
	if _, ok, _ := store.Stat(ctx, "new.txt"); ok {
		t.Error("a trashed path is still there")
	}
	if err := tr.Trash(ctx, "never.txt"); !errors.Is(err, provider.ErrNotExist) {
		t.Errorf("Trash of an absent path = %v; want ErrNotExist", err)
	}
}

// Copy crosses the socket with both of its refusals.
func TestPluginServesCopy(t *testing.T) {
	store, _ := openFake(t, map[string]any{
		"capabilities": []string{"copier"},
		"seed":         []string{"a.txt=body", "taken.txt=old"},
	})
	ctx := context.Background()
	c, ok := provider.AsCopier(store)
	if !ok {
		t.Fatal("no copier")
	}
	if _, err := c.Copy(ctx, "a.txt", "copies/a.txt"); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	rc, err := store.Get(ctx, "copies/a.txt")
	if err != nil {
		t.Fatalf("Get of the copy: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != "body" {
		t.Errorf("copy = %q; want body", got)
	}
	if _, err := c.Copy(ctx, "a.txt", "taken.txt"); !errors.Is(err, provider.ErrExist) {
		t.Errorf("Copy over an occupied path = %v; want ErrExist", err)
	}
	if _, err := c.Copy(ctx, "never.txt", "x.txt"); !errors.Is(err, provider.ErrNotExist) {
		t.Errorf("Copy of an absent path = %v; want ErrNotExist", err)
	}
}

// Upload's options cross the socket on the stream's first message: a time the
// object carries afterwards, a create that refuses, and an expectation the
// backend checks — refused before anything is written, as ErrChanged.
func TestPluginServesUploadWithItsOptions(t *testing.T) {
	store, _ := openFake(t, map[string]any{
		"capabilities": []string{"uploader"},
		"seed":         []string{"taken.txt=old"},
	})
	ctx := context.Background()
	u, ok := provider.AsUploader(store)
	if !ok {
		t.Fatal("no uploader")
	}
	when := time.Date(2001, 2, 3, 4, 5, 6, 789000000, time.UTC)
	up, err := u.Upload(ctx, "new.txt", strings.NewReader("hello"), provider.UploadOptions{Modified: when})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if !up.File.Modified.Equal(when) || up.Displaced != nil {
		t.Fatalf("Upload = %+v; want the modification time it was given, and nothing displaced", up)
	}
	big := bytes.Repeat([]byte("x"), 4<<20)
	if _, err := u.Upload(ctx, "taken.txt", bytes.NewReader(big), provider.UploadOptions{Create: true}); !errors.Is(err, provider.ErrExist) {
		t.Fatalf("a creating Upload over an occupied path = %v; want ErrExist", err)
	}
	stale := provider.RemoteFile{Path: "taken.txt", Hash: "len:99"}
	if _, err := u.Upload(ctx, "taken.txt", bytes.NewReader(big), provider.UploadOptions{Replaces: &stale}); !errors.Is(err, provider.ErrChanged) {
		t.Fatalf("an Upload replacing a stale view = %v; want ErrChanged", err)
	}
	rc, err := store.Get(ctx, "taken.txt")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != "old" {
		t.Fatalf("a refused Upload changed the path: %q", got)
	}
}

// A backend that can only tell afterwards reports what it displaced, every
// version of it in order, and each reads back through GetVersion.
func TestPluginReportsADisplacedVersion(t *testing.T) {
	store, _ := openFake(t, map[string]any{
		"capabilities":  []string{"uploader", "version-getter"},
		"seed":          []string{"doc.txt=someone else's edit"},
		"displace":      true,
		"displace-also": "an earlier edit",
	})
	ctx := context.Background()
	u, _ := provider.AsUploader(store)
	expected := provider.RemoteFile{Path: "doc.txt", Hash: "len:4"}
	up, err := u.Upload(ctx, "doc.txt", strings.NewReader("mine"), provider.UploadOptions{Replaces: &expected})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if len(up.Displaced) != 2 {
		t.Fatalf("%d displaced version(s) reported across the socket; want both", len(up.Displaced))
	}
	v, ok := provider.AsVersionGetter(store)
	if !ok {
		t.Fatal("no version getter")
	}
	for i, want := range []string{"an earlier edit", "someone else's edit"} {
		rc, err := v.GetVersion(ctx, "doc.txt", up.Displaced[i].Version)
		if err != nil {
			t.Fatalf("GetVersion %d: %v", i, err)
		}
		got, _ := io.ReadAll(rc)
		_ = rc.Close()
		if string(got) != want {
			t.Fatalf("displaced version %d = %q; want %q", i, got, want)
		}
	}
	// A streamed call's refusal arrives with its first read, as Get's does.
	rc, err := v.GetVersion(ctx, "doc.txt", "no-such-version")
	if err == nil {
		_, err = io.ReadAll(rc)
		_ = rc.Close()
	}
	if !errors.Is(err, provider.ErrNotExist) {
		t.Fatalf("reading an unknown version = %v; want ErrNotExist", err)
	}
}

// None is reachable on a backend that did not offer it: the proxy has every
// method, so this is rule 1 for the newest capabilities.
func TestPluginWithholdsUndeclaredCreateAndTrash(t *testing.T) {
	store, _ := openFake(t, map[string]any{"capabilities": []string{"enumerator"}})
	if _, ok := provider.AsCopier(store); ok {
		t.Error("AsCopier succeeded for a backend that did not offer it")
	}
	if _, ok := provider.AsCreator(store); ok {
		t.Error("AsCreator succeeded for a backend that did not offer it")
	}
	if _, ok := provider.AsTrasher(store); ok {
		t.Error("AsTrasher succeeded for a backend that did not offer it")
	}
	if _, ok := provider.AsUploader(store); ok {
		t.Error("AsUploader succeeded for a backend that did not offer it")
	}
	if _, ok := provider.AsVersionGetter(store); ok {
		t.Error("AsVersionGetter succeeded for a backend that did not offer it")
	}
}

// The classifications the engine switches on have to survive the process
// boundary. Each one, lost, is a silent behaviour change rather than a visible
// failure — see the note at the top of errors.go.
func TestPluginPreservesErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		name  string
		kind  string
		check func(t *testing.T, err error)
	}{
		{"not-exist", "not-exist", func(t *testing.T, err error) {
			if !errors.Is(err, provider.ErrNotExist) {
				t.Errorf("errors.Is(ErrNotExist) = false for %v", err)
			}
		}},
		{"cursor-expired", "cursor-expired", func(t *testing.T, err error) {
			if !errors.Is(err, provider.ErrCursorExpired) {
				t.Errorf("errors.Is(ErrCursorExpired) = false for %v", err)
			}
		}},
		{"retryable", "retryable", func(t *testing.T, err error) {
			if !provider.IsRetryable(err) {
				t.Errorf("IsRetryable = false for %v", err)
			}
		}},
		{"exist", "exist", func(t *testing.T, err error) {
			if !errors.Is(err, provider.ErrExist) {
				t.Errorf("errors.Is(ErrExist) = false for %v", err)
			}
		}},
		{"changed", "changed", func(t *testing.T, err error) {
			if !errors.Is(err, provider.ErrChanged) {
				t.Errorf("errors.Is(ErrChanged) = false for %v", err)
			}
		}},
		{"canceled", "canceled", func(t *testing.T, err error) {
			// The sentinel has to survive, because the engine and the pull loop
			// both read a cancellation as "we are shutting down" rather than as a
			// failure to retry...
			if !errors.Is(err, context.Canceled) {
				t.Errorf("errors.Is(context.Canceled) = false for %v", err)
			}
			// ...and so does the message, because a context the backend cancelled
			// itself arrives here identically to one the host did, and reporting
			// the first as a bare "context canceled" is how a Drive mount whose
			// token refresh had a dead context reported that it was shutting down.
			if provider.IsRetryable(err) {
				t.Errorf("IsRetryable = true for a cancellation: %v", err)
			}
		}},
		{"plain", "", func(t *testing.T, err error) {
			if provider.IsRetryable(err) {
				t.Errorf("IsRetryable = true for an unclassified %v", err)
			}
			if errors.Is(err, provider.ErrNotExist) {
				t.Errorf("an unclassified error matched ErrNotExist: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, _ := openFake(t, map[string]any{"fail": "put", "kind": tc.kind})
			_, err := store.Put(context.Background(), "x", strings.NewReader("y"))
			if err == nil {
				t.Fatal("Put succeeded; it was configured to fail")
			}
			tc.check(t, err)
			if !strings.Contains(err.Error(), "fake put") {
				t.Errorf("the backend's own message was lost: %v", err)
			}
		})
	}
}

// A backend that dies must not take the mount with it. The next call relaunches
// it, and the call that arrives during the backoff reports a retryable error —
// which is what puts the engine's existing retry loop in charge of the wait.
func TestPluginSurvivesABackendCrash(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "opens")
	store, lb := openFake(t, map[string]any{
		"exit-on": "remove",
		"marker":  marker,
	})
	ctx := context.Background()

	if _, err := store.Put(ctx, "a.txt", strings.NewReader("before")); err != nil {
		t.Fatalf("Put before the crash: %v", err)
	}
	if err := store.Remove(ctx, "a.txt"); err == nil {
		t.Fatal("Remove succeeded; the backend was configured to die on it")
	}

	// The first call after the crash notices the exit and schedules a restart.
	// It is retryable, because the engine is the thing that should be waiting.
	_, err := store.Put(ctx, "b.txt", strings.NewReader("during"))
	if err == nil {
		t.Fatal("a call immediately after the crash succeeded without a restart")
	}
	if !provider.IsRetryable(err) {
		t.Errorf("a call during the restart backoff must be retryable: %v", err)
	}

	// Wait out the backoff and prove the backend really came back.
	deadline := time.Now().Add(15 * time.Second)
	for {
		_, err = store.Put(ctx, "b.txt", strings.NewReader("after"))
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the backend never came back: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}

	opens, rerr := os.ReadFile(marker)
	if rerr != nil {
		t.Fatalf("reading the launch marker: %v", rerr)
	}
	if n := strings.Count(string(opens), "open"); n != 2 {
		t.Errorf("the backend opened %d times; want 2 (the original and the restart)", n)
	}
	if !strings.Contains(lb.String(), "backend exited") {
		t.Errorf("the crash was not reported in the mount's log:\n%s", lb.String())
	}
}

// A backend that refuses to open is not a transport failure, and the difference
// matters: "run drivel login" is not something a retry can fix.
func TestPluginOpenFailureIsNotRetryable(t *testing.T) {
	dir := pluginDir(t)
	l := NewLoader([]string{dir}, log.New(io.Discard, "", 0))
	f, err := l.Factory("fake")
	if err != nil {
		t.Fatalf("Factory: %v", err)
	}
	_, err = f(context.Background(), provider.Params{
		Config: provider.MustEncodeConfig(map[string]any{"open-fails": true}),
		Log:    log.New(io.Discard, "", 0),
	})
	if err == nil {
		t.Fatal("Open succeeded; it was configured to fail")
	}
	if provider.IsRetryable(err) {
		t.Errorf("a refused Open must not be retryable: %v", err)
	}
	if !strings.Contains(err.Error(), "refusing to open") {
		t.Errorf("the backend's reason was lost: %v", err)
	}
}

// Settings that the backend does not define are an error at Open, on the far
// side of the seam. M8 rule 6 has to hold for a provider in another process, or
// a typo in a `[mount.provider]` table becomes a silent no-op again.
func TestPluginRejectsUnknownSettings(t *testing.T) {
	dir := pluginDir(t)
	l := NewLoader([]string{dir}, log.New(io.Discard, "", 0))
	f, _ := l.Factory("fake")
	_, err := f(context.Background(), provider.Params{
		Config: provider.Config("capabilitys = []\n"),
		Log:    log.New(io.Discard, "", 0),
	})
	if err == nil {
		t.Fatal("a misspelt setting was accepted")
	}
	if !strings.Contains(err.Error(), "capabilitys") {
		t.Errorf("the error should name the key: %v", err)
	}
}

// A plugin's own log output belongs to the mount that launched it, prefixed with
// the kind. M8 rule 8 again: with several mounts, unattributed output is
// unreadable.
func TestPluginLogOutputReachesTheMountLog(t *testing.T) {
	_, lb := openFake(t, map[string]any{"seed": []string{"x=y"}})
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(lb.String(), "fake: fake backend open with 1 seeded paths") {
		if time.Now().After(deadline) {
			t.Fatalf("the plugin's log line never arrived:\n%s", lb.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// fixedFakeTime mirrors the timestamp the fake backend stamps on everything. It
// is duplicated rather than imported because the fake is a main package under
// testdata, which is exactly where it should stay — see its doc comment.
var fixedFakeTime = mustTime("2026-09-11T12:00:00.123456Z")

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}
