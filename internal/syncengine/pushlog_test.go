// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package syncengine

import (
	"bytes"
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/syshlted/drivel/internal/fsevent"
	"github.com/syshlted/drivel/provider"
)

// What a successful push says about itself.
//
// This is normally the wrong thing to test — pinning log text pins an
// implementation detail. Here the log IS the interface. Tier B of the
// multi-client campaign derives transfer volume from these lines
// (docs/dev/multiclient-test-plan.md §2.7), because per-process network accounting
// on a shared host is awkward and the logs already name every path. That plan
// was written against lines the engine did not emit: before this, a successful
// upload logged nothing at all, and every "[sync] push" in a log was a failure.
// The first real MC-03 run against Drive is what found it — three clients
// converged, two downloads appeared, and the upload counter read zero.

// capture is a logger whose output a test can read back.
type capture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *capture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *capture) lines() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, l := range strings.Split(c.buf.String(), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

func (c *capture) count(sub string) int {
	n := 0
	for _, l := range c.lines() {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

// Every kind of push that reaches the remote leaves exactly one line naming it.
func TestEverySuccessfulPushLogsOneLine(t *testing.T) {
	dataDir := t.TempDir()
	fs := newFakeStore()
	logs := &capture{}
	e := New(Config{Store: fs, DataDir: dataDir, Logger: log.New(logs, "", 0)})
	ctx := context.Background()

	writeFile(t, dataDir, "a.txt", "hello")
	e.handle(ctx, fsevent.Event{Op: fsevent.OpCreate, Path: "a.txt"})
	e.handle(ctx, fsevent.Event{Op: fsevent.OpMkdir, Path: "sub"})
	e.handle(ctx, fsevent.Event{Op: fsevent.OpRename, Path: "a.txt", NewPath: "b.txt"})
	e.handle(ctx, fsevent.Event{Op: fsevent.OpUnlink, Path: "b.txt"})

	for _, want := range []string{
		"[sync] upload   a.txt (5 B)", // the byte count is the transfer volume
		"[sync] mkdir    sub",
		"[sync] rename   a.txt -> b.txt",
		"[sync] delete   b.txt",
	} {
		if n := logs.count(want); n != 1 {
			t.Errorf("logged %q %d time(s); want exactly 1\nfull log:\n%s",
				want, n, strings.Join(logs.lines(), "\n"))
		}
	}
}

// A metadata-only event reaches no remote and must say nothing. Otherwise every
// chmod inflates the upload count that MC-20 and MC-22 report as bandwidth.
func TestAMetadataOnlyEventLogsNoTransfer(t *testing.T) {
	dataDir := t.TempDir()
	logs := &capture{}
	e := New(Config{Store: newFakeStore(), DataDir: dataDir, Logger: log.New(logs, "", 0)})

	writeFile(t, dataDir, "a.txt", "hello")
	e.handle(context.Background(), fsevent.Event{Op: fsevent.OpSetattr, Path: "a.txt"})

	if n := logs.count("[sync] upload"); n != 0 {
		t.Errorf("a setattr logged %d upload line(s); want 0\nfull log:\n%s", n, strings.Join(logs.lines(), "\n"))
	}
}

// The M6 unchanged-content gate declines the upload, so it must not report one.
// This is the case MC-21 turns into an assertion: `touch` on an unmodified file
// costs the fleet nothing, and a log that claimed an upload would make a working
// optimisation look like a regression in every published number.
func TestASkippedPushLogsNoUpload(t *testing.T) {
	dir := t.TempDir()
	st := newState(t)
	store := newPatchStore()
	logs := &capture{}

	// The remote already holds exactly these bytes, and the echo says so. Big
	// enough to clear hashSkipMinSize, or the gate never runs at all.
	body := bigBody('a')
	if err := os.WriteFile(filepath.Join(dir, "a.bin"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	syncedAt(t, st, store, "a.bin", body)

	e := New(Config{Store: store, DataDir: dir, State: st, Logger: log.New(logs, "", 0)})
	// Dirty nil: extents unknown, so the range-write gate declines and the
	// unchanged-content gate is the one under test.
	e.handle(context.Background(), fsevent.Event{Op: fsevent.OpWrite, Path: "a.bin"})

	if n := logs.count("remote already holds"); n != 1 {
		t.Fatalf("the unchanged-content gate did not fire; this test proves nothing\nfull log:\n%s",
			strings.Join(logs.lines(), "\n"))
	}
	if n := logs.count("[sync] upload"); n != 0 {
		t.Errorf("a skipped push logged %d upload line(s); want 0 — it reported a transfer that never happened\nfull log:\n%s",
			n, strings.Join(logs.lines(), "\n"))
	}
	if got := store.ops(); len(got) != 0 {
		t.Fatalf("the gate let an upload through after all: %v", got)
	}
}

// growingStore appends to the file being pushed, once, partway through reading
// it. It stands in for the ordinary case MC-40 found live: a copy into the mount
// that takes longer than the debounce window is pushed while it is still being
// written, and Put streams it to EOF, so the whole of it goes across.
type growingStore struct {
	*fakeStore
	path  string // backing path to extend
	extra string
	once  bool
}

func (g *growingStore) Put(ctx context.Context, p string, r io.Reader) (provider.RemoteFile, error) {
	return g.fakeStore.Put(ctx, p, readerFunc(func(b []byte) (int, error) {
		if !g.once {
			g.once = true
			f, err := os.OpenFile(g.path, os.O_WRONLY|os.O_APPEND, 0o600)
			if err != nil {
				return 0, err
			}
			if _, err := f.WriteString(g.extra); err != nil {
				return 0, err
			}
			if err := f.Close(); err != nil {
				return 0, err
			}
		}
		return r.Read(b)
	}))
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// The byte count on an upload line is the bytes sent, not the size the file had
// when the push started. The two differ exactly when the number matters — a file
// still being written — and the campaign reads these lines as transfer volume.
func TestUploadLineReportsTheBytesActuallySent(t *testing.T) {
	dataDir := t.TempDir()
	logs := &capture{}
	writeFile(t, dataDir, "a.txt", "hello")
	fs := &growingStore{
		fakeStore: newFakeStore(),
		path:      filepath.Join(dataDir, "a.txt"),
		extra:     " world",
	}
	e := New(Config{Store: fs, DataDir: dataDir, Logger: log.New(logs, "", 0)})

	e.handle(context.Background(), fsevent.Event{Op: fsevent.OpCreate, Path: "a.txt"})

	// 11 bytes crossed: "hello" plus the " world" appended during the push. The
	// pre-transfer Stat said 5.
	if n := logs.count("[sync] upload   a.txt (11 B)"); n != 1 {
		t.Errorf("want one line reporting 11 bytes sent; got:\n%s", strings.Join(logs.lines(), "\n"))
	}
	if n := logs.count("(5 B)"); n != 0 {
		t.Errorf("the line reported the size at push time, not the transfer:\n%s", strings.Join(logs.lines(), "\n"))
	}
}
