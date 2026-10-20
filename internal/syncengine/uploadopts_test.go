// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package syncengine

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/syshlted/drivel/internal/fsevent"
	"github.com/syshlted/drivel/provider"
)

// A push through provider.Uploader carries the local modification time and the
// remote it expects to replace. Both shipped backends offer it; these drive the
// engine's half against a store whose behaviour each test chooses.

// uploadStore is a patchStore that is also an Uploader and a VersionGetter.
// meanwhile, when set, runs once inside an Upload of an existing path, after the
// content has been read and before the expectation is checked — another
// client's write landing during the upload. With displace the store behaves
// like Drive (no conditional write: it replaces, then reports what it
// displaced); without, like SFTP (it refuses with ErrChanged).
type uploadStore struct {
	*patchStore
	displace  bool
	meanwhile func(s *uploadStore, p string)

	omu      sync.Mutex
	opts     []provider.UploadOptions
	versions map[string][]byte
}

func newUploadStore() *uploadStore {
	return &uploadStore{patchStore: newPatchStore(), versions: map[string][]byte{}}
}

func (s *uploadStore) Upload(_ context.Context, p string, r io.Reader, opts provider.UploadOptions) (provider.Uploaded, error) {
	s.omu.Lock()
	s.opts = append(s.opts, opts)
	hook := s.meanwhile
	s.meanwhile = nil
	s.omu.Unlock()
	b, err := io.ReadAll(r)
	if err != nil {
		return provider.Uploaded{}, err
	}
	if hook != nil {
		hook(s, p)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, had := s.content[p]
	if opts.Create && had {
		return provider.Uploaded{}, fmt.Errorf("upload %s: %w", p, provider.ErrExist)
	}
	var out provider.Uploaded
	if opts.Replaces != nil && had && !provider.Matches(*opts.Replaces, s.file(p)) {
		if !s.displace {
			s.record("Upload(%s) refused: changed", p)
			return provider.Uploaded{}, fmt.Errorf("upload %s: %w", p, provider.ErrChanged)
		}
		d := s.file(p)
		d.Version = fmt.Sprintf("was-%d", len(s.versions))
		d.Modified = time.Date(2026, 10, 20, 9, 30, 0, 0, time.UTC)
		s.omu.Lock()
		s.versions[d.Version] = append([]byte(nil), cur...)
		s.omu.Unlock()
		out.Displaced = &d
	}
	s.content[p] = b
	s.record("Upload(%s,bytes=%d)", p, len(b))
	out.File = s.file(p)
	out.File.Modified = opts.Modified
	return out, nil
}

func (s *uploadStore) GetVersion(_ context.Context, p, version string) (io.ReadCloser, error) {
	s.omu.Lock()
	defer s.omu.Unlock()
	b, ok := s.versions[version]
	if !ok {
		return nil, fmt.Errorf("%s at %s: %w", p, version, provider.ErrNotExist)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (s *uploadStore) lastOpts(t *testing.T) provider.UploadOptions {
	t.Helper()
	s.omu.Lock()
	defer s.omu.Unlock()
	if len(s.opts) == 0 {
		t.Fatal("nothing was uploaded through Upload")
	}
	return s.opts[len(s.opts)-1]
}

// noVersions is an uploadStore that cannot read a displaced version back.
type noVersions struct{ *uploadStore }

func (noVersions) Capabilities() provider.CapabilitySet {
	return provider.AllCapabilities.Without(provider.CapVersionGetter)
}

// The file is dated, on the remote, to its last local edit rather than to the
// upload.
func TestAPushCarriesTheLocalModificationTime(t *testing.T) {
	dir := t.TempDir()
	store := newUploadStore()
	writeFile(t, dir, "a.txt", "body")
	when := time.Date(2002, 3, 4, 5, 6, 7, 0, time.UTC)
	setMtime(t, dir, "a.txt", when)
	e := New(Config{Store: store, DataDir: dir, State: newState(t)})

	e.handle(context.Background(), fsevent.Event{Op: fsevent.OpWrite, Path: "a.txt"})

	if got := store.lastOpts(t).Modified; !got.Equal(when) {
		t.Fatalf("Modified = %v; want the local mtime %v", got, when)
	}
}

// A rewrite of a synced file expects to replace exactly the remote it checked.
func TestAPushExpectsToReplaceWhatItChecked(t *testing.T) {
	store := newUploadStore()
	_, _, mk := divergedFixtureWith(t, store, "base", "base", "ours")
	mk().handle(context.Background(), fsevent.Event{Op: fsevent.OpWrite, Path: "a.txt"})

	opts := store.lastOpts(t)
	if opts.Replaces == nil || opts.Replaces.Hash != md5hex("base") {
		t.Fatalf("Replaces = %+v; want the remote the check saw (base)", opts.Replaces)
	}
}

// The window this closes: the remote was checked, then somebody else's edit
// landed while ours was uploading. A store that can refuse does; the retry looks
// again, keeps their edit as a conflict copy, and then replaces it.
func TestAnEditLandingDuringThePushIsKeptNotOverwritten(t *testing.T) {
	store := newUploadStore()
	store.meanwhile = func(s *uploadStore, p string) { s.seed(p, []byte("their edit, made meanwhile")) }
	dir, logs, mk := divergedFixtureWith(t, store, "base", "base", "ours")
	e := mk()
	e.retry = retryPolicy{max: 5, base: time.Microsecond, cap: 100 * time.Microsecond}

	e.executeWithRetry(context.Background(), fsevent.Event{Op: fsevent.OpWrite, Path: "a.txt"})

	if got := string(store.remoteBytes("a.txt")); got != "ours" {
		t.Fatalf("remote = %q; want our edit, which still wins the path", got)
	}
	copies := conflictCopies(t, dir)
	if len(copies) != 1 || !containsValue(copies, "their edit, made meanwhile") {
		t.Fatalf("conflict copies = %v; want their edit kept", copies)
	}
	if !strings.Contains(logs.String(), "the remote changed during the upload") {
		t.Fatalf("the refused attempt was not logged:\n%s", logs)
	}
}

// A store that can only tell afterwards (Drive) reports what it displaced, and
// that version is read back and kept as the conflict's loser.
func TestADisplacedEditIsKeptAsAConflictCopy(t *testing.T) {
	store := newUploadStore()
	store.displace = true
	store.meanwhile = func(s *uploadStore, p string) { s.seed(p, []byte("their edit, made meanwhile")) }
	dir, logs, mk := divergedFixtureWith(t, store, "base", "base", "ours")
	e := mk()

	e.handle(context.Background(), fsevent.Event{Op: fsevent.OpWrite, Path: "a.txt"})

	if got := string(store.remoteBytes("a.txt")); got != "ours" {
		t.Fatalf("remote = %q; want our edit", got)
	}
	copies := conflictCopies(t, dir)
	if len(copies) != 1 || !containsValue(copies, "their edit, made meanwhile") {
		t.Fatalf("conflict copies = %v; want the displaced edit kept", copies)
	}
	if !strings.Contains(logs.String(), "an edit made on the remote during this upload was replaced by it") {
		t.Fatalf("the displaced edit was not reported:\n%s", logs)
	}
}

// Without a way to read it back the edit is in the backend's history, and the
// log says so rather than nothing.
func TestADisplacedEditTheBackendCannotReadBackIsReported(t *testing.T) {
	inner := newUploadStore()
	inner.displace = true
	inner.meanwhile = func(s *uploadStore, p string) { s.seed(p, []byte("their edit, made meanwhile")) }
	_, logs, mk := divergedFixtureWith(t, noVersions{inner}, "base", "base", "ours")
	e := mk()

	e.handle(context.Background(), fsevent.Event{Op: fsevent.OpWrite, Path: "a.txt"})

	if !strings.Contains(logs.String(), "it is in the backend's version history") {
		t.Fatalf("the unrecoverable displaced edit was not reported:\n%s", logs)
	}
}

// conflicts = "overwrite" asks for no checking, and so for no expectation.
func TestOverwriteExpectsNothing(t *testing.T) {
	dir := t.TempDir()
	st := newState(t)
	store := newUploadStore()
	syncedAt(t, st, store.patchStore, "a.txt", []byte("base"))
	writeFile(t, dir, "a.txt", "ours")
	e := New(Config{Store: store, DataDir: dir, State: st, Conflicts: &Conflicts{Action: ConflictsOverwrite}})
	e.handle(context.Background(), fsevent.Event{Op: fsevent.OpWrite, Path: "a.txt"})
	if r := store.lastOpts(t).Replaces; r != nil {
		t.Fatalf("Replaces = %+v under conflicts = overwrite; want none", r)
	}
}

// divergedFixtureWith is divergedFixture over a store of the test's choosing:
// a.txt synced at base, the remote then at theirs, and ours written locally.
func divergedFixtureWith(t *testing.T, s interface {
	provider.Store
	seed(string, []byte)
}, base, theirs, ours string,
) (string, *capture, func() *Engine) {
	t.Helper()
	dir, store, logs, mk := divergedFixture(t, base, theirs, ours)
	s.seed("a.txt", store.remoteBytes("a.txt"))
	return dir, logs, func() *Engine { return mk(s) }
}

func containsValue(m map[string]string, v string) bool {
	for _, x := range m {
		if x == v {
			return true
		}
	}
	return false
}
