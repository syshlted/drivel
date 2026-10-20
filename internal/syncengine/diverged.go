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
// conditional write no shipped backend offers.
//
// A path with no baseline — a first push, a state DB that was replaced, two
// clients creating one path — has no "since" to have diverged from, but a remote
// object already standing there is still someone's content that this Put would
// replace. It is kept on the same terms, but only when the M6 gates already paid
// for the Stat (a file of at least one block): a first push of a small file would
// otherwise cost a round trip it never paid before, and on Drive that is a
// second name query per create, which is what bounds a bulk copy's rate.

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
//
// create asks the caller to upload with provider.Creator instead of Put: the
// path has no baseline and nothing has looked at the remote yet, and a Create
// that refuses is how the check is made without a round trip of its own.
// pushContent comes back here with the Stat a refusal calls for.
func (e *Engine) preserveDivergedRemote(ctx context.Context, p string, f *os.File, seen remoteView) (proceed, create bool, checked remoteView, err error) {
	proceed, create, err = e.checkRemote(ctx, p, f, &seen)
	return proceed, create, seen, err
}

// checkRemote is preserveDivergedRemote, leaving in *seen the remote it judged —
// which the upload that follows expects to replace (Engine.replacing).
func (e *Engine) checkRemote(ctx context.Context, p string, f *os.File, view *remoteView) (proceed, create bool, err error) {
	seen := *view
	defer func() { *view = seen }()
	if e.state == nil {
		return true, false, nil
	}
	// The pull may be resolving this same path; whichever goes second sees what
	// the first did (Conflicts.resolving). If the pull went first and replaced
	// the file, f is the loser it moved away, and nothing about it may be sent.
	defer e.conflicts.lockPath(p)()
	if !e.stillAt(p, statOf(f)) {
		return false, false, errReplaced
	}
	echo, ok, err := e.state.GetEcho(p)
	if err != nil {
		// The same read failing is no evidence either way; the push proceeds as it
		// always did, and the error is said rather than swallowed.
		e.logf("[sync] reading the baseline for %s: %v (uploading without checking for a newer remote)", p, err)
		return true, false, nil //nolint:nilerr // reported above; failing the push would strand a local edit on a DB read
	}
	if !ok {
		// No baseline: a first push. Whatever stands at the path is content this
		// mount has never seen, so it is kept like any other loser — unless the
		// mount said to overwrite, which is the one setting that skips the check.
		if !seen.statted {
			if e.conflicts.action() == ConflictsOverwrite {
				return true, false, nil
			}
			_, canCreate := provider.AsCreator(e.store)
			_, canUpload := provider.AsUploader(e.store)
			if canCreate || canUpload {
				return true, true, nil
			}
			// No Creator: one Stat buys the same answer.
			remote, exists, err := e.store.Stat(ctx, p)
			if err != nil {
				e.logf("[sync] stat %s: %v (uploading without checking for a remote file)", p, err)
				return true, false, nil
			}
			seen = remoteView{statted: true, exists: exists, rf: remote}
		}
		// A directory at the path is not ours to copy, and the Put will say so.
		if !seen.exists || seen.rf.IsDir {
			return true, false, nil
		}
	} else if !seen.statted {
		remote, exists, err := e.store.Stat(ctx, p)
		if err != nil {
			// The Put this precedes needs the same remote and will meet the same
			// failure; checking first is not worth inventing a new way to fail.
			e.logf("[sync] stat %s: %v (uploading without checking for a newer remote)", p, err)
			return true, false, nil
		}
		seen = remoteView{statted: true, exists: exists, rf: remote}
	}
	if ok && (!seen.exists || echo.Matches(seen.rf.Hash, seen.rf.Version)) {
		// Gone remotely (a re-create, which the deletion side owns) or still ours.
		return true, false, nil
	}

	// The remote moved. Two outcomes need no copy, and both need a digest of the
	// local bytes in the provider's own algorithm; without one, keep the copy.
	if local, ok := e.localDigest(f); ok {
		if local == seen.rf.Hash {
			seen.rf.Path = p
			e.recordEchoOf(seen.rf, statOf(f))
			e.logf("[sync] skip %s: the remote changed and already holds these bytes", p)
			e.mon.count(cUnchangedSkips, 1)
			return false, false, nil
		}
		if echo.Hash != "" && local == echo.Hash {
			// Our copy is what we last synced, so this push carries no edit — a
			// touch, a save of identical bytes — and sending it would replace the
			// other side's newer content with older content. Leave the path to
			// the pull, which applies the remote's version as an ordinary change.
			e.logf("[sync] skip %s: unchanged here since the last sync, and the remote has a newer version (the pull will apply it)", p)
			return false, false, nil
		}
	}

	where, err := e.conflicts.keepRemote(ctx, e.conflictEnv(), p, seen.rf)
	if err != nil {
		return false, false, fmt.Errorf("keeping the remote version of %s before overwriting it: %w", p, err)
	}
	if where == "" {
		// Kept already: the pull met this same remote version first and resolved
		// it "local newer", leaving the local edit for us to push.
		return true, false, nil
	}
	why := "the remote changed since the last sync"
	if !ok {
		why = "the remote holds a file this mount has no record of"
	}
	e.logf("[sync] conflict %s: %s; kept it in %s before uploading the local copy", p, why, where)
	e.mon.count(cConflicts, 1)
	return true, false, nil
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
// All-zero blocks are left as holes (copySparse). A file it replaces keeps its
// permission bits — a script edited on another machine stays executable here —
// without setuid, setgid or sticky, which content from a remote must never
// carry (DESIGN.md §10). A new file is 0600: nothing says what else it should
// be, and the backend has no mode to tell.
//
// observe sees the Get's outcome and nothing else — reachability is a fact
// about the provider, and a full local disk is not one (M14).
func fetchTo(ctx context.Context, store provider.Store, path, dst string, modified time.Time, observe func(error)) (int64, error) {
	return fetchFrom(func() (io.ReadCloser, error) { return store.Get(ctx, path) }, dst, modified, observe)
}

// fetchFrom is fetchTo for content from any source: open is called once, after
// the destination directory exists.
func fetchFrom(open func() (io.ReadCloser, error), dst string, modified time.Time, observe func(error)) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, err
	}
	rc, err := open()
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

	n, err := copySparse(tmp, rc)
	if err != nil {
		tmp.Close()
		return n, err
	}
	if fi, err := os.Lstat(dst); err == nil && fi.Mode().IsRegular() {
		if err := tmp.Chmod(fi.Mode().Perm()); err != nil {
			tmp.Close()
			return n, err
		}
	}
	if err := tmp.Close(); err != nil {
		return n, err
	}
	if !modified.IsZero() {
		_ = os.Chtimes(tmpName, modified, modified)
	}
	return n, os.Rename(tmpName, dst)
}
