// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package syncengine

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/syshlted/drivel/internal/state"
	"github.com/syshlted/drivel/provider"
)

// The pull and the push can meet one conflict at the same moment, and each
// decides the winner on its own terms — the pull by mtime, the push by being
// the pusher. A live run over Drive (2026-10-19, mc31-policy.sh) had them do
// it concurrently: the pull moved the local edit away as the loser, the push
// kept the remote version as the loser too and uploaded the edit through the
// handle it already had open. Both tests below set up that moment, one per
// order, with the push's handle opened before either side starts.

// raceSetup is a.txt synced at "base", edited here to "ours" an hour ago, and
// a remote "theirs" that is newer: the pull would call the remote the winner,
// the push would call the local edit the winner.
func raceSetup(t *testing.T, store provider.Store, ps *patchStore) (dir string, st *state.Store, c *Conflicts, remote provider.RemoteFile) {
	t.Helper()
	dir = t.TempDir()
	st = newState(t)
	ps.seed("a.txt", []byte("theirs"))
	writeFile(t, dir, "a.txt", "ours")
	setMtime(t, dir, "a.txt", time.Now().Add(-time.Hour))
	_ = st.SetEcho("a.txt", state.Echo{Hash: md5hex("base"), Version: "1", At: time.Now()})
	c = &Conflicts{Action: ConflictsRemoteTrash}
	remote, _, _ = ps.Stat(context.Background(), "a.txt")
	remote.Modified = time.Now()
	return dir, st, c, remote
}

func openBacking(t *testing.T, dir, rel string) *os.File {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, rel))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// heldGetStore holds the first Get of a.txt — the pull's download of the
// winner, after it has already moved the local edit away — until release.
type heldGetStore struct {
	*trashingStore
	once    *sync.Once
	started chan struct{}
	release chan struct{}
}

func (s heldGetStore) Get(ctx context.Context, p string) (io.ReadCloser, error) {
	if p == "a.txt" {
		s.once.Do(func() { close(s.started); <-s.release })
	}
	return s.trashingStore.Get(ctx, p)
}

// The pull goes first. The push's handle is then the loser the pull moved
// away, and it must not be sent, whatever the baseline says by the time the
// push looks — the pull has made the remote's version the baseline, so the
// remote "matches" and only the handle shows the push is describing a file
// that is no longer there.
func TestAPushWaitsForThePullResolvingItsPath(t *testing.T) {
	ts := newTrashingStore()
	hs := heldGetStore{ts, &sync.Once{}, make(chan struct{}), make(chan struct{})}
	dir, st, c, remote := raceSetup(t, hs, ts.patchStore)
	d := NewDownloader(nil, hs, dir, st, DefaultCadence).Conflicts(c)
	e := New(Config{Store: hs, DataDir: dir, State: st, Conflicts: c})
	f := openBacking(t, dir, "a.txt")

	pulled := make(chan error, 1)
	go func() {
		pulled <- d.apply(context.Background(), provider.RemoteChange{Path: "a.txt", File: &remote})
	}()
	<-hs.started

	type result struct {
		proceed bool
		err     error
	}
	pushed := make(chan result, 1)
	go func() {
		p, _, err := e.preserveDivergedRemote(context.Background(), "a.txt", f, remoteView{})
		pushed <- result{p, err}
	}()
	close(hs.release)
	if err := <-pulled; err != nil {
		t.Fatal(err)
	}
	d.sending.Wait()
	r := <-pushed

	if r.proceed || !errors.Is(r.err, errReplaced) {
		t.Errorf("push of the moved-away handle: proceed=%v err=%v; want errReplaced and no upload", r.proceed, r.err)
	}
	if got, _ := readBacking(t, dir, "a.txt"); got != "theirs" {
		t.Errorf("a.txt = %q; the pull's winner should stand", got)
	}
	if got := ts.trashed2(); len(got) != 1 || onlyValue(got) != "ours" {
		t.Errorf("trashed %v; want the local edit, once, and never a copy of the winner", got)
	}
}

// heldStatStore holds the first Stat of a.txt — the push's divergence check,
// which runs under the path's lock — until release.
type heldStatStore struct {
	*trashingStore
	once    *sync.Once
	started chan struct{}
	release chan struct{}
}

func (s heldStatStore) Stat(ctx context.Context, p string) (provider.RemoteFile, bool, error) {
	if p == "a.txt" {
		s.once.Do(func() { close(s.started); <-s.release })
	}
	return s.trashingStore.Stat(ctx, p)
}

// The push goes first. The pull must wait for it, and must then not decide
// again: the push has kept the remote version as the loser and is about to
// send the local edit, which an mtime decision would move away.
func TestThePullWaitsForAPushResolvingItsPath(t *testing.T) {
	ts := newTrashingStore()
	hs := heldStatStore{ts, &sync.Once{}, make(chan struct{}), make(chan struct{})}
	dir, st, c, remote := raceSetup(t, hs, ts.patchStore)
	d := NewDownloader(nil, hs, dir, st, DefaultCadence).Conflicts(c)
	e := New(Config{Store: hs, DataDir: dir, State: st, Conflicts: c})
	f := openBacking(t, dir, "a.txt")

	type result struct {
		proceed bool
		err     error
	}
	pushed := make(chan result, 1)
	go func() {
		p, _, err := e.preserveDivergedRemote(context.Background(), "a.txt", f, remoteView{})
		pushed <- result{p, err}
	}()
	<-hs.started

	pulled := make(chan error, 1)
	go func() {
		pulled <- d.apply(context.Background(), provider.RemoteChange{Path: "a.txt", File: &remote})
	}()
	// Unserialised, the pull finishes well inside this; serialised, it cannot
	// finish until the push lets go, so nothing here waits on a correct build.
	var pullErr error
	early := false
	select {
	case pullErr = <-pulled:
		early = true
		t.Error("the pull applied while the push was resolving the same path")
	case <-time.After(300 * time.Millisecond):
	}
	close(hs.release)
	r := <-pushed
	if !early {
		pullErr = <-pulled
	}
	if pullErr != nil {
		t.Errorf("pull: %v", pullErr)
	}
	d.sending.Wait()

	if !r.proceed || r.err != nil {
		t.Errorf("push: proceed=%v err=%v; want it to go ahead over the kept remote", r.proceed, r.err)
	}
	if got, _ := readBacking(t, dir, "a.txt"); got != "ours" {
		t.Errorf("a.txt = %q; the pull replaced the edit the push is sending", got)
	}
	if got := ts.trashed2(); len(got) != 1 || onlyValue(got) != "theirs" {
		t.Errorf("trashed %v; want the remote version, once", got)
	}
}
