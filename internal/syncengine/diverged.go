// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package syncengine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/syshlted/drivel/provider"
)

// A whole-file Put over a remote that moved since our baseline used to lose the
// other side's edit with no trace: §6's conflict copy is made by the PULL, and
// it only fires when the remote change arrives before our push. Two clients
// editing one file concurrently therefore ended with the later pusher's bytes
// and no copy anywhere (MC-31, MC-60) — on Drive the loser survived only in the
// revision history drivel never exposes, and on SFTP and every M17–M21 backend
// it was simply gone.
//
// preserveDivergedRemote is the push side of the same policy. Before a
// whole-file Put of a path with a baseline it asks whether the remote still
// holds what the baseline describes, and when it does not, it keeps the remote's
// version as a §6 conflict copy before overwriting it. Who wins the path does
// not change — the local edit still does, exactly as before — so the only
// difference a user can see is that the overwritten version now exists.
//
// It narrows the window rather than closing it: a remote write landing between
// this Stat and the Put is still overwritten unseen, and closing that needs a
// conditional write no shipped backend offers. And it says nothing about a path
// with no baseline — a first push, a state DB that was replaced — where there is
// no "since" to have diverged from, so those keep the old behaviour.

// remoteView is what one Stat of the remote said, carried from the M6 gates to
// the divergence check so a push pays for at most one round trip between them.
type remoteView struct {
	statted bool
	exists  bool
	rf      provider.RemoteFile
}

// preserveDivergedRemote runs between the M6 gates and the whole-file Put of p,
// and reports whether the Put should go ahead. A false with a nil error means
// the push has been settled without uploading; an error fails the push, so the
// engine's retry decides what happens next — which is the point of failing it:
// a remote that could not be preserved must not be overwritten.
func (e *Engine) preserveDivergedRemote(ctx context.Context, p string, f *os.File, seen remoteView) (bool, error) {
	if e.state == nil {
		return true, nil
	}
	echo, ok, err := e.state.GetEcho(p)
	if err != nil {
		// The same read failing is no evidence either way; the push proceeds as it
		// always did, and the error is said rather than swallowed.
		e.logf("[sync] reading the baseline for %s: %v (uploading without checking for a newer remote)", p, err)
		return true, nil //nolint:nilerr // reported above; failing the push would strand a local edit on a DB read
	}
	if !ok {
		return true, nil // no baseline: nothing to have diverged from
	}
	if !seen.statted {
		remote, exists, err := e.store.Stat(ctx, p)
		if err != nil {
			// The Put this precedes needs the same remote and will meet the same
			// failure; checking first is not worth inventing a new way to fail.
			e.logf("[sync] stat %s: %v (uploading without checking for a newer remote)", p, err)
			return true, nil
		}
		seen = remoteView{statted: true, exists: exists, rf: remote}
	}
	if !seen.exists || echo.Matches(seen.rf.Hash, seen.rf.Version) {
		// Gone remotely (a re-create, which the deletion side owns) or still ours.
		return true, nil
	}

	// The remote moved. Two outcomes need no copy, and both need a digest of the
	// local bytes in the provider's own algorithm; without one, keep the copy.
	if local, ok := e.localDigest(f); ok {
		if local == seen.rf.Hash {
			seen.rf.Path = p
			e.recordEchoOf(seen.rf, statOf(f))
			e.logf("[sync] skip %s: the remote changed and already holds these bytes", p)
			e.mon.count(cUnchangedSkips, 1)
			return false, nil
		}
		if echo.Hash != "" && local == echo.Hash {
			// Our copy is what we last synced, so this push carries no edit — a
			// touch, a save of identical bytes — and sending it would replace the
			// other side's newer content with older content. Leave the path to
			// the pull, which applies the remote's version as an ordinary change.
			e.logf("[sync] skip %s: unchanged here since the last sync, and the remote has a newer version (the pull will apply it)", p)
			return false, nil
		}
	}

	modified := seen.rf.Modified
	if modified.IsZero() {
		modified = time.Now()
	}
	copyRel := conflictName(p, modified)
	copyDst := filepath.Join(e.dataDir, filepath.FromSlash(copyRel))
	if _, err := os.Lstat(copyDst); err == nil {
		// The pull met this same remote version first and resolved it "local
		// newer": the copy it made is this one, by name, and it left the local
		// edit for us to push. Making a second would be the same bytes again.
		return true, nil
	}
	if _, err := fetchTo(ctx, e.store, p, copyDst, modified, e.mon.observe); err != nil {
		return false, fmt.Errorf("keeping the newer remote copy of %s before overwriting it: %w", p, err)
	}
	e.logf("[sync] conflict %s: the remote changed since the last sync; saved it as %s before uploading the local copy", p, copyRel)
	e.mon.count(cConflicts, 1)
	e.mon.applied(copyRel, StatusLocalOnly)
	return true, nil
}

// localDigest hashes f with the provider's own algorithm, reporting ok=false when
// the provider offers none or the read fails — both of which mean "cannot tell",
// never "different".
func (e *Engine) localDigest(f *os.File) (string, bool) {
	hasher, ok := provider.AsContentHasher(e.store)
	if !ok {
		return "", false
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", false
	}
	sum, err := hasher.HashContent(f)
	if _, sErr := f.Seek(0, io.SeekStart); sErr != nil {
		err = errors.Join(err, sErr)
	}
	if err != nil || sum == "" {
		return "", false
	}
	return sum, true
}

// fetchTo streams the object at path into dst atomically (temp file + rename in
// the destination directory) and stamps its mtime to modified, reporting how
// many bytes it wrote. Shared by the pull's downloads and the push's conflict
// copy, because a conflict copy written any less carefully is the one file in
// the tree nothing will ever re-fetch.
//
// observe sees the Get's outcome and nothing else — reachability is a fact
// about the provider, and a full local disk is not one (M14).
func fetchTo(ctx context.Context, store provider.Store, path, dst string, modified time.Time, observe func(error)) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, err
	}
	rc, err := store.Get(ctx, path)
	observe(err)
	if err != nil {
		return 0, err
	}
	defer rc.Close()

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".drivel-*")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename

	n, err := io.Copy(tmp, rc)
	if err != nil {
		tmp.Close()
		return n, err
	}
	if err := tmp.Close(); err != nil {
		return n, err
	}
	if !modified.IsZero() {
		_ = os.Chtimes(tmpName, modified, modified)
	}
	return n, os.Rename(tmpName, dst)
}
