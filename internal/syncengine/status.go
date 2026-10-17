// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package syncengine

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/syshlted/drivel/internal/state"
)

// Path statuses (M14). Every value is derived from state that already exists
// for another reason; none is computed by asking a provider, which is what keeps
// a folder repaint from becoming a billed listing.
const (
	// StatusSynced: a §4 baseline exists and the local file still matches the
	// fingerprint recorded with it. For a directory, a baseline exists — which
	// says nothing about what is inside it; there is no rolled-up answer.
	StatusSynced = "synced"
	// StatusPending: the outbound path holds a change for it (debouncing,
	// queued, or held by a pause).
	StatusPending = "pending"
	// StatusUploading: a worker is executing an operation on it now.
	StatusUploading = "uploading"
	// StatusRetrying: the last attempt failed and another is scheduled.
	StatusRetrying = "retrying"
	// StatusError: the engine gave up on it. A repair is recorded and the next
	// sweep re-sends it (M27).
	StatusError = "error"
	// StatusPlaceholder: M5 — a placeholder, read from the authoritative xattr.
	StatusPlaceholder = "placeholder"
	// StatusConflict: a §6 conflict copy sits beside it.
	StatusConflict = "conflict"
	// StatusLocalOnly: exists here and is not synced, by policy — a §6 conflict
	// copy, a special file (M15), one of drivel's own temporaries.
	StatusLocalOnly = "local-only"
	// StatusUnknown: we have no record that vouches for it. NOT "everything is
	// fine": reporting synced for a path never observed is the same class of lie
	// as mistaking a placeholder for an empty file (DESIGN.md §9/M14). Detail
	// says which kind of unknown it is.
	StatusUnknown = "unknown"
	// StatusDeleted appears only in the event stream: the path has gone.
	StatusDeleted = "deleted"
)

// PathStatus is one path's answer.
type PathStatus struct {
	Path   string
	Status string
	Kind   string // "file", "dir", "symlink", "other"; "" when absent locally
	Size   int64
	// Detail explains the status in a sentence where the bare value would
	// mislead: which kind of unknown, why local-only, what is being retried.
	Detail      string
	Attempt     int
	NextAttempt time.Time
	Error       string
	SyncedAt    time.Time // when the baseline was recorded
	Conflicts   []string  // names of conflict copies beside it
}

// ErrNoSuchPath is returned for a path that neither exists locally nor has
// anything in motion or recorded for it.
var ErrNoSuchPath = errors.New("no such path")

// ErrBadPath is returned for a path that is not a clean root-relative path.
var ErrBadPath = errors.New("not a path inside the mount")

// StatusReader answers status queries for one mount from local state alone: the
// monitor, the state store, the backing tree and the placeholder marker.
type StatusReader struct {
	Monitor *Monitor
	State   *state.Store // nil for a log-only mount
	DataDir string
	Holes   Placeholders // nil in eager mode
}

// MaxChildren bounds a directory listing. The load pattern is a file manager
// repainting a folder — one request for the directory, never one per file —
// and a folder past this is paged with After.
const MaxChildren = 10000

// cleanRel validates a root-relative path; "" and "." both mean the root.
func cleanRel(rel string) (string, error) {
	rel = strings.TrimPrefix(rel, "/")
	if rel == "" || rel == "." {
		return "", nil
	}
	if !relPathOK(rel) {
		return "", ErrBadPath
	}
	return rel, nil
}

// Path reports the status of one path.
func (r StatusReader) Path(rel string) (PathStatus, error) {
	rel, err := cleanRel(rel)
	if err != nil {
		return PathStatus{}, err
	}
	abs := r.abs(rel)
	fi, statErr := os.Lstat(abs)
	var siblings []string
	if statErr == nil && fi.Mode().IsRegular() && rel != "" {
		siblings = r.conflictCopiesOf(rel)
	}
	out, err := r.resolve([]entry{{rel: rel, fi: fi, statErr: statErr, conflicts: siblings}})
	if err != nil {
		return PathStatus{}, err
	}
	return out[0], nil
}

// Children reports the status of rel's direct children, sorted by name,
// starting after the name `after`, at most limit of them. truncated says there
// are more. A directory's status is never a roll-up of its children's: that
// answer would need a maintained per-directory index, with invariants and a
// drift bug of its own, and v1 deliberately does not have one.
func (r StatusReader) Children(rel, after string, limit int) (out []PathStatus, truncated bool, err error) {
	rel, err = cleanRel(rel)
	if err != nil {
		return nil, false, err
	}
	if limit <= 0 || limit > MaxChildren {
		limit = MaxChildren
	}
	des, err := os.ReadDir(r.abs(rel)) // sorted by name
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, ErrNoSuchPath
		}
		return nil, false, err
	}
	names := make([]string, 0, len(des))
	for _, de := range des {
		names = append(names, de.Name())
	}
	copies := conflictCopiesIn(names)
	var es []entry
	for _, de := range des {
		if de.Name() <= after {
			continue
		}
		if len(es) == limit {
			truncated = true
			break
		}
		child := path.Join(rel, de.Name())
		fi, statErr := de.Info()
		e := entry{rel: child, fi: fi, statErr: statErr}
		if statErr == nil && fi.Mode().IsRegular() {
			e.conflicts = copies[de.Name()]
		}
		es = append(es, e)
	}
	out, err = r.resolve(es)
	return out, truncated, err
}

type entry struct {
	rel       string
	fi        os.FileInfo
	statErr   error
	conflicts []string
}

func (r StatusReader) abs(rel string) string {
	return filepath.Join(r.DataDir, filepath.FromSlash(rel))
}

// resolve answers for a batch: one monitor acquisition and one read transaction,
// however many entries.
func (r StatusReader) resolve(es []entry) ([]PathStatus, error) {
	paths := make([]string, len(es))
	for i, e := range es {
		paths[i] = e.rel
	}
	moving := r.Monitor.Lookup(paths)
	var recs map[string]state.Record
	if r.State != nil {
		var err error
		if recs, err = r.State.Lookup(paths); err != nil {
			return nil, err
		}
	}
	out := make([]PathStatus, 0, len(es))
	for _, e := range es {
		ps, err := r.one(e, moving[e.rel], recs[e.rel])
		if err != nil {
			return nil, err
		}
		out = append(out, ps)
	}
	return out, nil
}

// one decides a single path's status. The order is the precedence: what is
// happening now outranks what is recorded, and anything that cannot be vouched
// for is unknown with a reason, never synced by default.
func (r StatusReader) one(e entry, t Transient, rec state.Record) (PathStatus, error) {
	ps := PathStatus{Path: e.rel}
	if rec.HasEcho {
		ps.SyncedAt = rec.Echo.At
	}
	exists := e.statErr == nil
	if e.statErr != nil && !errors.Is(e.statErr, fs.ErrNotExist) {
		return ps, e.statErr
	}
	if exists {
		ps.Kind, ps.Size = kindName(e.fi), e.fi.Size()
		if e.fi.IsDir() {
			ps.Size = 0
		}
	}

	if t.Status != "" {
		ps.Status, ps.Attempt, ps.NextAttempt, ps.Error = t.Status, t.Attempt, t.NextAttempt, t.Error
		if t.Status == StatusError {
			ps.Detail = "the engine gave up on this push; the next sweep re-sends it"
		}
		return ps, nil
	}

	if !exists {
		switch {
		case rec.OwesPush:
			ps.Status, ps.Detail = StatusError, "an abandoned push is recorded for this path, which no longer exists locally"
		case rec.OwesFetch:
			ps.Status, ps.Detail = StatusError, "a remote change to this path failed to apply here; it is retried"
		case rec.HasEcho:
			ps.Status, ps.Detail = StatusUnknown, "absent locally but a sync record remains; the next sweep decides whether it was deleted"
		default:
			return ps, ErrNoSuchPath
		}
		return ps, nil
	}

	if e.rel == "" {
		ps.Status, ps.Detail = StatusSynced, "the mount root"
		if r.State == nil {
			ps.Status, ps.Detail = StatusUnknown, "this mount has no provider (log-only)"
		}
		return ps, nil
	}

	mode := e.fi.Mode()
	base := path.Base(e.rel)
	switch {
	case !mode.IsRegular() && !mode.IsDir():
		ps.Status, ps.Detail = StatusLocalOnly, kindOf(mode)+": not synced (no byte stream to send)"
		return ps, nil
	case strings.HasPrefix(base, ".drivel-"):
		ps.Status, ps.Detail = StatusLocalOnly, "one of drivel's own temporary files"
		return ps, nil
	case conflictRe.MatchString(base):
		ps.Status, ps.Detail = StatusLocalOnly, "a conflict copy: kept locally and never uploaded"
		return ps, nil
	}

	if r.State == nil {
		ps.Status, ps.Detail = StatusUnknown, "this mount has no provider (log-only)"
		return ps, nil
	}

	if mode.IsRegular() && r.Holes != nil && r.Holes.IsPlaceholder(e.rel) {
		ps.Status, ps.Detail = StatusPlaceholder, "content is fetched on first read"
		return ps, nil
	}

	switch {
	case rec.OwesPush:
		ps.Status, ps.Detail = StatusError, "an earlier push was abandoned; the next sweep re-sends it"
	case rec.OwesFetch:
		ps.Status, ps.Detail = StatusError, "a remote change to this path failed to apply here; it is retried"
	case !rec.HasEcho:
		ps.Status, ps.Detail = StatusUnknown, "no sync record: never pushed or pulled by this mount"
	case mode.IsDir():
		ps.Status = StatusSynced
	case rec.Echo.LocalMTime.IsZero():
		ps.Status, ps.Detail = StatusUnknown, "the sync record has no local fingerprint, so whether this copy still matches it cannot be told without hashing"
	case fingerprintDiverged(e.fi, rec.Echo):
		ps.Status, ps.Detail = StatusUnknown, "differs from the last synced version and no upload is queued (changed outside the mount?); the next sweep compares it"
	default:
		ps.Status = StatusSynced
	}

	if len(e.conflicts) > 0 && mode.IsRegular() {
		ps.Conflicts = e.conflicts
		if ps.Status == StatusSynced || ps.Status == StatusUnknown {
			ps.Status, ps.Detail = StatusConflict, "a conflict copy sits beside it"
		}
	}
	return ps, nil
}

func kindName(fi os.FileInfo) string {
	switch m := fi.Mode(); {
	case m.IsRegular():
		return "file"
	case m.IsDir():
		return "dir"
	case m&fs.ModeSymlink != 0:
		return "symlink"
	}
	return "other"
}

// conflictCopiesOf names the §6 copies beside rel. It reads the parent
// directory, which is the price of asking about one file; Children computes the
// same answer from the listing it already has.
func (r StatusReader) conflictCopiesOf(rel string) []string {
	des, err := os.ReadDir(r.abs(path.Dir(rel)))
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(des))
	for _, de := range des {
		names = append(names, de.Name())
	}
	return conflictCopiesIn(names)[path.Base(rel)]
}

// conflictCopiesIn maps each original name to the conflict copies of it among
// names, inverting conflictName.
func conflictCopiesIn(names []string) map[string][]string {
	out := map[string][]string{}
	for _, n := range names {
		loc := conflictRe.FindStringSubmatchIndex(n)
		if loc == nil {
			continue
		}
		orig := n[:loc[0]]
		if loc[2] >= 0 {
			orig += n[loc[2]:loc[3]]
		}
		out[orig] = append(out[orig], n)
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}
