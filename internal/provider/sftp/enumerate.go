// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package sftp

import (
	"context"
	"encoding/json"
	"fmt"
	"path"

	"github.com/zishmusic/drivel/provider"
)

// enumPageSize bounds one Enumerate call. The sweep persists its cursor after
// every page (see syncengine.runSweep), so this is also how much a resumed sweep
// re-does — and how much of the tree is held in memory at once. A page is
// metadata only; a thousand entries is a few hundred kilobytes.
const enumPageSize = 1000

// enumCursor is the sweep's resume point: the directories still to be listed.
//
// It is a breadth-first frontier rather than a position in a flat listing,
// because that is what SFTP can actually offer — there is no recursive list, so
// enumeration is one READDIR per directory and the only thing worth remembering
// between pages is which directories are still owed. It also makes the walk
// parent-first by construction, so none of the flat path's parking machinery
// (gdrive's "park a child on an unseen parent") has any counterpart here.
type enumCursor struct {
	// Pending is root-relative, in the order they will be listed.
	Pending []string `json:"pending"`
}

// Enumerate lists the tree under the mount root (provider.Enumerator, M7b).
//
// On this backend the sweep is not a periodic safety net beneath a change feed —
// it is the *entire* inbound path, because SFTP has no notification of any kind
// (see the ChangeSource note in sftp.go). Everything a remote edit does reaches
// the mount through here, so -sweep-interval is the poll interval and its 24h
// default is wrong for every SFTP mount.
//
// Two rules about failure, and they pull in opposite directions on purpose.
//
// A directory that has disappeared between being queued and being listed is
// skipped: it is genuinely gone, its children with it, and failing on it would
// abandon a sweep every time anything was deleted while one ran.
//
// Every *other* listing failure abandons the sweep. That looks harsh for, say, one
// unreadable subdirectory, and it is the safe direction: a sweep that quietly
// omits a subtree still reports itself complete, and M7b's delete pass reads
// "synced before, not observed now" as a deletion. Skipping an unreadable
// directory would therefore propose deleting everything under it. Abandoning the
// pass instead is M7b's own rule — refuse a pass you cannot justify — applied one
// level down.
func (s *Store) Enumerate(ctx context.Context, cursor string) ([]provider.RemoteFile, string, error) {
	type page struct {
		files []provider.RemoteFile
		next  string
	}
	pg, err := do(ctx, s, func(c *conn) (page, error) {
		pending := decodeCursor(cursor)

		var (
			out     []provider.RemoteFile
			skipped int
		)
		for len(pending) > 0 && len(out) < enumPageSize {
			if err := ctx.Err(); err != nil {
				return page{}, err
			}
			dir := pending[0]
			pending = pending[1:]

			entries, err := c.cli.ReadDirContext(ctx, c.abs(dir))
			if err != nil {
				if isNotExist(err) && dir != "" {
					continue
				}
				// The root is never skipped, whatever the reason. A sweep that
				// reports an empty tree is the dangerous shape — every synced path
				// then looks deleted — so an unreadable root is an error, exactly as
				// M7b row 4 requires of gdrive.Enumerate.
				return page{}, fmt.Errorf("listing %s: %w", c.abs(dir), err)
			}

			for _, fi := range entries {
				name := fi.Name()
				if name == "." || name == ".." || isTemp(name) {
					// A temp file is an upload of ours in flight, or one a dropped
					// connection abandoned. Either way it is not a file the user has:
					// reporting it would materialise a .drivel-upload.* beside every
					// interrupted push, and on the next sweep infer its deletion.
					continue
				}
				rel := path.Join(dir, name)
				switch {
				case fi.IsDir():
					out = append(out, remoteFile(rel, fi))
					pending = append(pending, rel)
				case fi.Mode().IsRegular():
					out = append(out, remoteFile(rel, fi))
				default:
					// Symlinks, sockets, fifos and device nodes. The seam carries a
					// byte stream and a size; there is nothing honest to report for
					// these, and M15 defers symlinks deliberately. ReadDir uses LSTAT,
					// so a symlink arrives here as a symlink rather than as whatever
					// it points at — which is also what keeps a link loop from turning
					// this walk into a non-terminating one.
					skipped++
				}
			}
		}
		if skipped > 0 {
			s.lg.Printf("[sftp] enumerate: skipped %d special file(s): no byte stream to sync", skipped)
		}

		next, err := encodeCursor(pending)
		if err != nil {
			return page{}, err
		}
		return page{files: out, next: next}, nil
	})
	return pg.files, pg.next, err
}

// decodeCursor reads a resume point. The empty cursor starts a sweep at the root.
//
// It cannot fail, deliberately. A cursor we cannot parse is a state DB written by
// another version or another provider, and restarting the walk is both cheap and
// correct — where returning an error would abandon the sweep, which on this
// backend is the whole inbound path.
func decodeCursor(cursor string) []string {
	if cursor == "" {
		return []string{""}
	}
	var ec enumCursor
	if err := json.Unmarshal([]byte(cursor), &ec); err != nil || len(ec.Pending) == 0 {
		// Empty-but-parseable counts as unreadable, and this is the important half:
		// encodeCursor never emits an empty frontier (it returns "" for a finished
		// walk), so one can only arrive from corruption or from another provider's
		// state DB — and honouring it would list nothing while reporting a
		// *complete* sweep, which is M7b row 4's dangerous shape exactly. Every
		// synced path would then look remotely deleted.
		return []string{""}
	}
	return ec.Pending
}

// encodeCursor returns the token for the directories still owed, or "" when the
// walk is done — which is how the sweep learns it is complete.
func encodeCursor(pending []string) (string, error) {
	if len(pending) == 0 {
		return "", nil
	}
	b, err := json.Marshal(enumCursor{Pending: pending})
	if err != nil {
		return "", fmt.Errorf("encoding the sweep cursor: %w", err)
	}
	return string(b), nil
}
