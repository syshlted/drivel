// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package syncengine

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/syshlted/drivel/provider"
)

// The push side of §6 (diverged.go). Each test starts from a path synced at
// "base", then moves the remote to "theirs" behind the engine's back — another
// client's push the pull has not delivered yet — and pushes.

// hashlessStore is a patchStore that declares no content digest, the shape of
// SFTP and every M17–M21 backend, where "is our copy unchanged?" cannot be asked.
type hashlessStore struct{ *patchStore }

func (hashlessStore) Capabilities() provider.CapabilitySet {
	return provider.AllCapabilities.Without(provider.CapContentHasher).Without(provider.CapRangePutter)
}

// getFailStore cannot read the remote back, which is the case the guard must
// refuse to overwrite through.
type getFailStore struct{ *patchStore }

func (getFailStore) Get(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("get refused")
}

func divergedFixture(t *testing.T, base, theirs, ours string) (dir string, store *patchStore, logs *capture, e func(provider.Store) *Engine) {
	t.Helper()
	dir = t.TempDir()
	st := newState(t)
	store = newPatchStore()
	syncedAt(t, st, store, "a.txt", []byte(base))
	store.seed("a.txt", []byte(theirs))
	writeFile(t, dir, "a.txt", ours)
	logs = &capture{}
	return dir, store, logs, func(s provider.Store) *Engine {
		return New(Config{Store: s, DataDir: dir, State: st, Logger: log.New(logs, "", 0)})
	}
}

// conflictCopies lists the §6 copies of a.txt in dir.
func conflictCopies(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, ent := range ents {
		if strings.HasPrefix(ent.Name(), "a (conflict ") {
			b, err := os.ReadFile(filepath.Join(dir, ent.Name()))
			if err != nil {
				t.Fatal(err)
			}
			out[ent.Name()] = string(b)
		}
	}
	return out
}

// THE case (MC-31): both sides edited, ours is pushed second. The local edit
// still wins the path — that has not changed — and the other side's edit is now
// kept beside it instead of vanishing. Remove the guard and the copy is missing.
func TestAPushOverADivergedRemoteKeepsTheirVersion(t *testing.T) {
	dir, store, logs, engine := divergedFixture(t, "base", "theirs", "ours")
	if err := engine(store).pushContent(context.Background(), "a.txt", nil); err != nil {
		t.Fatal(err)
	}
	if got := string(store.remoteBytes("a.txt")); got != "ours" {
		t.Errorf("remote = %q; the local edit should still win the path", got)
	}
	copies := conflictCopies(t, dir)
	if len(copies) != 1 {
		t.Fatalf("conflict copies = %v; want exactly one holding the remote's version", copies)
	}
	for _, body := range copies {
		if body != "theirs" {
			t.Errorf("conflict copy holds %q; want the overwritten remote, \"theirs\"", body)
		}
	}
	if logs.count("[sync] conflict a.txt") != 1 {
		t.Errorf("the conflict was not reported\n%s", strings.Join(logs.lines(), "\n"))
	}
}

// A remote that still holds our baseline is the ordinary rewrite: no copy.
func TestAPushOverAnUnchangedRemoteMakesNoCopy(t *testing.T) {
	dir, store, _, engine := divergedFixture(t, "base", "base", "ours")
	if err := engine(store).pushContent(context.Background(), "a.txt", nil); err != nil {
		t.Fatal(err)
	}
	if copies := conflictCopies(t, dir); len(copies) != 0 {
		t.Errorf("conflict copies = %v; want none — nothing diverged", copies)
	}
	if got := string(store.remoteBytes("a.txt")); got != "ours" {
		t.Errorf("remote = %q; want the push", got)
	}
}

// Our copy is still the baseline — a touch, an identical save — and the remote
// has moved on. Pushing would replace their newer bytes with our older ones, so
// the push is dropped and the pull applies theirs. Remove the check and the
// remote reads "base" again.
func TestAnUnchangedLocalCopyDoesNotOverwriteANewerRemote(t *testing.T) {
	dir, store, _, engine := divergedFixture(t, "base", "theirs", "base")
	if err := engine(store).pushContent(context.Background(), "a.txt", nil); err != nil {
		t.Fatal(err)
	}
	if got := string(store.remoteBytes("a.txt")); got != "theirs" {
		t.Errorf("remote = %q; an unedited local copy overwrote the newer remote", got)
	}
	if copies := conflictCopies(t, dir); len(copies) != 0 {
		t.Errorf("conflict copies = %v; want none — there is no local edit to conflict", copies)
	}
}

// Both sides arrived at the same bytes: nothing to push, nothing to keep.
func TestAConvergedRemoteIsNotPushedOrCopied(t *testing.T) {
	dir, store, _, engine := divergedFixture(t, "base", "same", "same")
	if err := engine(store).pushContent(context.Background(), "a.txt", nil); err != nil {
		t.Fatal(err)
	}
	if ops := store.ops(); len(ops) != 0 {
		t.Errorf("ops = %v; want none", ops)
	}
	if copies := conflictCopies(t, dir); len(copies) != 0 {
		t.Errorf("conflict copies = %v; want none", copies)
	}
}

// With no digest the guard cannot tell a touch from an edit, so it keeps the
// copy — a spare file, against a lost one.
func TestAHashlessBackendKeepsTheCopy(t *testing.T) {
	dir, store, _, engine := divergedFixture(t, "base", "theirs", "base")
	if err := engine(hashlessStore{store}).pushContent(context.Background(), "a.txt", nil); err != nil {
		t.Fatal(err)
	}
	if copies := conflictCopies(t, dir); len(copies) != 1 {
		t.Errorf("conflict copies = %v; want one — without a digest the remote must be kept", copies)
	}
}

// A remote that cannot be read back must not be overwritten: the push fails and
// the engine's retry decides, which is the whole of the guard's safety.
func TestARemoteThatCannotBeKeptIsNotOverwritten(t *testing.T) {
	_, store, _, engine := divergedFixture(t, "base", "theirs", "ours")
	if err := engine(getFailStore{store}).pushContent(context.Background(), "a.txt", nil); err == nil {
		t.Fatal("pushContent succeeded although the diverged remote could not be preserved")
	}
	if got := string(store.remoteBytes("a.txt")); got != "theirs" {
		t.Errorf("remote = %q; it was overwritten without a copy", got)
	}
}

// The pull got there first and resolved it "local newer", leaving a copy named
// for this exact remote version and the local edit for us to push. One copy, not
// two — and the pull's copy is left as it was.
func TestACopyThePullAlreadyMadeIsNotMadeAgain(t *testing.T) {
	dir, store, _, engine := divergedFixture(t, "base", "theirs", "ours")
	when := time.Date(2026, 10, 17, 9, 30, 0, 0, time.UTC)
	pulled := filepath.Join(dir, filepath.FromSlash(conflictName("a.txt", when)))
	if err := os.WriteFile(pulled, []byte("theirs, as the pull saved it"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := engine(modifiedStore{store, when}).pushContent(context.Background(), "a.txt", nil); err != nil {
		t.Fatal(err)
	}
	copies := conflictCopies(t, dir)
	if len(copies) != 1 || copies[filepath.Base(pulled)] != "theirs, as the pull saved it" {
		t.Errorf("conflict copies = %v; want only the pull's, untouched", copies)
	}
	if got := string(store.remoteBytes("a.txt")); got != "ours" {
		t.Errorf("remote = %q; the local edit the pull left for us was not pushed", got)
	}
}

// modifiedStore reports a fixed modification time, as every real backend does.
type modifiedStore struct {
	*patchStore
	at time.Time
}

func (m modifiedStore) Stat(ctx context.Context, p string) (provider.RemoteFile, bool, error) {
	rf, ok, err := m.patchStore.Stat(ctx, p)
	rf.Modified = m.at
	return rf, ok, err
}

// No baseline at all — a state DB that was replaced, two clients creating one
// path — and a remote object already standing at the path. For a file the M6
// gates Stat anyway, it is kept before the Put replaces it. Remove the no-baseline
// branch and the remote's bytes are gone.
func TestAFirstPushOverAnUnknownRemoteKeepsIt(t *testing.T) {
	dir := t.TempDir()
	store := newPatchStore()
	store.seed("a.bin", bigBody('t'))
	if err := os.WriteFile(filepath.Join(dir, "a.bin"), bigBody('o'), 0o644); err != nil {
		t.Fatal(err)
	}
	logs := &capture{}
	e := New(Config{Store: store, DataDir: dir, State: newState(t), Logger: log.New(logs, "", 0)})
	if err := e.pushContent(context.Background(), "a.bin", nil); err != nil {
		t.Fatal(err)
	}
	if got := store.remoteBytes("a.bin"); len(got) == 0 || got[0] != 'o' {
		t.Error("the local file should still win the path")
	}
	var copies []string
	ents, _ := os.ReadDir(dir)
	for _, ent := range ents {
		if strings.HasPrefix(ent.Name(), "a (conflict ") {
			b, _ := os.ReadFile(filepath.Join(dir, ent.Name()))
			if len(b) == 0 || b[0] != 't' {
				t.Errorf("%s does not hold the remote's bytes", ent.Name())
			}
			copies = append(copies, ent.Name())
		}
	}
	if len(copies) != 1 {
		t.Fatalf("conflict copies = %v; want one holding the remote file this mount had no record of", copies)
	}
	if logs.count("no record of") != 1 {
		t.Errorf("the conflict did not say why\n%s", strings.Join(logs.lines(), "\n"))
	}
}
