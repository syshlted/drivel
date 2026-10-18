// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package syncengine

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/syshlted/drivel/internal/state"
	"github.com/syshlted/drivel/internal/trash"
	"github.com/syshlted/drivel/provider"
)

// §6 decides who wins a path when both sides changed it; this file decides what
// happens to the side that lost. Until 2026-10-18 there was one answer, a
// sibling file named "name (conflict <time>).ext" that never left the machine
// it was made on. There are now four, chosen per mount, and every place that
// resolves a conflict — the pull's two branches and the push's divergence check
// — goes through the one Conflicts value the mount builds, so the four can never
// disagree about where a lost edit went.
//
// Who wins is not configurable and nothing here changes it.

// ConflictDirName is the per-directory folder conflict copies are kept in. It is
// reserved the way the claim marker is: a path with this component is a
// conflict copy, and what syncs it is the mount's ConflictPlace, never the
// ordinary rules.
const ConflictDirName = ".drivel-conflicts"

// ConflictAction is where the losing side of a conflict goes.
type ConflictAction string

const (
	// ConflictsDir keeps it in the directory's .drivel-conflicts folder. The default.
	ConflictsDir ConflictAction = "dir"
	// ConflictsLocalTrash puts it in this user's home trash (freedesktop.org).
	ConflictsLocalTrash ConflictAction = "local-trash"
	// ConflictsRemoteTrash puts it in the backend's trash. Refused at open on a
	// backend without one (provider.Trasher).
	ConflictsRemoteTrash ConflictAction = "remote-trash"
	// ConflictsOverwrite keeps nothing: the winner replaces the loser. It also
	// skips the check a first push makes for a remote file this mount has no
	// record of, so it is the right setting only where nothing else writes.
	ConflictsOverwrite ConflictAction = "overwrite"
)

// ConflictActions lists the accepted values, in the order the docs give them.
var ConflictActions = []ConflictAction{ConflictsDir, ConflictsLocalTrash, ConflictsRemoteTrash, ConflictsOverwrite}

// ParseConflictAction reads a configured value. An unknown one is an error,
// never the default: a misspelling of "dir" read as "overwrite" would be the
// one mistake in this setting that loses data.
func ParseConflictAction(s string) (ConflictAction, error) {
	for _, a := range ConflictActions {
		if string(a) == s {
			return a, nil
		}
	}
	return "", fmt.Errorf("unknown conflicts setting %q (want %s)", s, joinValues(ConflictActions))
}

// ConflictPlace is where a .drivel-conflicts folder lives, for ConflictsDir.
type ConflictPlace string

const (
	// ConflictsBoth syncs the folder like any other: on the remote and on every
	// client that also syncs it. The default.
	ConflictsBoth ConflictPlace = "both"
	// ConflictsLocal keeps it on the machine that resolved the conflict.
	ConflictsLocal ConflictPlace = "local"
	// ConflictsRemote keeps it on the remote only; no client downloads it.
	ConflictsRemote ConflictPlace = "remote"
)

// ConflictPlaces lists the accepted values.
var ConflictPlaces = []ConflictPlace{ConflictsBoth, ConflictsLocal, ConflictsRemote}

// ParseConflictPlace reads a configured value; unknown is an error.
func ParseConflictPlace(s string) (ConflictPlace, error) {
	for _, p := range ConflictPlaces {
		if string(p) == s {
			return p, nil
		}
	}
	return "", fmt.Errorf("unknown conflicts-dir setting %q (want %s)", s, joinValues(ConflictPlaces))
}

// ResolveConflicts reads the two settings as a user wrote them, "" meaning
// unmentioned, and is what every boundary — flags, config file, fstab options —
// calls, so the three agree. conflicts-dir describes a folder only "dir" makes,
// so naming it beside any other action is refused rather than ignored: a
// setting that looks applied and is not. For any action but "dir" the place
// comes back empty, which is what every later check reads as "unmentioned".
func ResolveConflicts(action, dir string) (ConflictAction, ConflictPlace, error) {
	a, p := ConflictsDir, ConflictsBoth
	var err error
	if action != "" {
		if a, err = ParseConflictAction(action); err != nil {
			return "", "", err
		}
	}
	if dir != "" {
		if p, err = ParseConflictPlace(dir); err != nil {
			return "", "", err
		}
		if a != ConflictsDir {
			return "", "", fmt.Errorf("conflicts-dir = %s applies only to conflicts = %s, not %s", dir, ConflictsDir, a)
		}
	}
	if a != ConflictsDir {
		p = ""
	}
	return a, p, nil
}

func joinValues[T ~string](vs []T) string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = string(v)
	}
	return strings.Join(out, ", ")
}

// Conflicts is one mount's conflict policy, shared by its engine and its pull
// loop. The zero value — and a nil pointer — is the default: ConflictsDir,
// ConflictsBoth.
type Conflicts struct {
	Action ConflictAction
	Place  ConflictPlace

	// Trash is the home trash, for ConflictsLocalTrash; MountPath is the root a
	// user sees, which a trash entry names as the file's origin so that a
	// desktop's Restore writes it back through the mount.
	Trash     *trash.Trash
	MountPath string

	// kept remembers, per path, the remote version already kept. One conflict
	// is usually met twice — the pull finds it ("local newer") and then the push
	// of the local edit finds the same remote version diverged from its baseline
	// — and the second must not keep a second copy. A copy in the local folder
	// is found by name; this is what finds one that went to a trash or to the
	// remote only. Also in the state store (state.SetKept), so a restart between
	// the two meetings does not keep a duplicate.
	mu   sync.Mutex
	kept map[string]string
}

func (c *Conflicts) action() ConflictAction {
	if c == nil || c.Action == "" {
		return ConflictsDir
	}
	return c.Action
}

func (c *Conflicts) place() ConflictPlace {
	if c == nil || c.Place == "" {
		return ConflictsBoth
	}
	return c.Place
}

// String names the policy for a log line or a listing: "dir (both)", "overwrite".
func (c *Conflicts) String() string {
	if c.action() == ConflictsDir {
		return fmt.Sprintf("%s (%s)", ConflictsDir, c.place())
	}
	return string(c.action())
}

// inConflictDir reports whether p has a .drivel-conflicts component.
func inConflictDir(p string) bool {
	for _, part := range strings.Split(p, "/") {
		if part == ConflictDirName {
			return true
		}
	}
	return false
}

// Syncs reports whether this mount syncs p in either direction. Everything
// outside a conflicts folder does; a path inside one does only when the folder
// is configured to live on both sides. Every pass that moves content — the
// push, the pull, the sweep's remote pass, its delete pass and its walk — asks
// this, because a conflicts folder synced by one of them and not another is a
// copy uploaded and then inferred deleted, or the reverse.
func (c *Conflicts) Syncs(p string) bool {
	if !inConflictDir(p) {
		return true
	}
	return c.action() == ConflictsDir && c.place() == ConflictsBoth
}

// copyPath is where a conflict copy of rel goes inside its directory's folder,
// named for the losing version's time so that the pull and the push name the
// same version identically.
func copyPath(rel string, t time.Time) string {
	dir := path.Dir(rel)
	name := conflictName(path.Base(rel), t)
	if dir == "." {
		return path.Join(ConflictDirName, name)
	}
	return path.Join(dir, ConflictDirName, name)
}

// conflictEnv is what a resolution needs from the component calling it.
type conflictEnv struct {
	store   provider.Store
	state   *state.Store
	dataDir string
	logf    func(string, ...any)
	observe func(error)

	// later, when set, takes the remote half of keeping a loser — an upload, a
	// trash — off the caller's goroutine, and nil runs it inline. The pull loop
	// sets it, because an upload there held up every inbound change behind it
	// and ignored a pause; the engine does not, because its workers are already
	// off the event path and already stop at the pause gate. Only the remote
	// half is deferred: the loser is safe on local disk before the caller goes
	// on to replace it.
	later func(copyRel string, job func(context.Context) string)
}

// finish runs job, the remote half of keeping the copy at copyRel, inline or
// through env.later, and returns where the loser is: job's own answer, or
// pending when it has been queued.
func (env conflictEnv) finish(ctx context.Context, copyRel, pending string, job func(context.Context) string) string {
	if env.later == nil {
		return job(ctx)
	}
	env.later(copyRel, job)
	return pending
}

func (env conflictEnv) abs(rel string) string {
	return filepath.Join(env.dataDir, filepath.FromSlash(rel))
}

// keepLocal preserves the local file at rel, which is about to be replaced by
// the remote's version. It returns where the loser went, for the caller's log
// line. An error means it could not be preserved, and the caller must then not
// replace it.
func (c *Conflicts) keepLocal(ctx context.Context, env conflictEnv, rel string, mtime time.Time) (string, error) {
	src := env.abs(rel)
	switch c.action() {
	case ConflictsOverwrite:
		return "nowhere (conflicts = overwrite)", nil

	case ConflictsLocalTrash:
		dst, err := c.Trash.Move(src, c.origin(rel), time.Now())
		if err != nil {
			return "", fmt.Errorf("moving the local edit to the trash: %w", err)
		}
		return "the trash, as " + filepath.Base(dst), nil
	}

	copyRel := uniqueCopy(env, copyPath(rel, mtime))
	dst := env.abs(copyRel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(src, dst); err != nil {
		return "", err
	}
	return c.publish(ctx, env, copyRel), nil
}

// keepRemote preserves the remote object rf at rel, which is about to be
// replaced by the local version. Same contract as keepLocal; an empty "where"
// with a nil error means this version had already been kept.
func (c *Conflicts) keepRemote(ctx context.Context, env conflictEnv, rel string, rf provider.RemoteFile) (string, error) {
	if c.action() == ConflictsOverwrite {
		return "nowhere (conflicts = overwrite)", nil
	}
	modified := rf.Modified
	if modified.IsZero() {
		modified = time.Now()
	}
	identity := rf.Version + "\x00" + rf.Hash
	if c.alreadyKept(env, rel, identity) {
		return "", nil
	}
	copyRel := copyPath(rel, modified)

	var where string
	switch c.action() {
	case ConflictsLocalTrash:
		e, err := c.Trash.Reserve(c.origin(rel), time.Now())
		if err != nil {
			return "", err
		}
		if _, err := fetchTo(ctx, env.store, rel, e.Path, modified, env.observe); err != nil {
			e.Abandon()
			return "", err
		}
		where = "the trash, as " + filepath.Base(e.Path)

	default:
		dst := env.abs(copyRel)
		if _, err := os.Lstat(dst); err == nil {
			// Made already, by name: the same remote version, kept by the pull
			// before this push met it (or by an earlier run).
			c.remember(env, rel, identity)
			return "", nil
		}
		if _, err := fetchTo(ctx, env.store, rel, dst, modified, env.observe); err != nil {
			return "", err
		}
		where = c.publish(ctx, env, copyRel)
	}
	c.remember(env, rel, identity)
	return where, nil
}

// publish puts a copy already in the local folder where the policy wants it,
// and says where that is: kept there, uploaded beside it, or uploaded and the
// local one removed — into the remote trash for ConflictsRemoteTrash. A failed
// upload is reported and leaves the copy local, which still preserves it: with
// the folder on both sides the sweep's walk uploads it later, and otherwise it
// stays where it is. This is the remote half of keeping a loser, so it goes
// through env.finish.
func (c *Conflicts) publish(ctx context.Context, env conflictEnv, copyRel string) string {
	switch {
	case c.action() == ConflictsDir && c.place() == ConflictsLocal:
		return copyRel
	case c.action() == ConflictsDir && c.place() == ConflictsBoth:
		return env.finish(ctx, copyRel, copyRel+" (here; upload queued)", func(ctx context.Context) string {
			if err := c.upload(ctx, env, copyRel, env.abs(copyRel), true); err != nil {
				env.logf("[sync] conflict copy %s: uploading it: %v (kept here; the next sweep sends it)", copyRel, err)
				return copyRel + " (here; upload pending)"
			}
			return copyRel + " (here and on the remote)"
		})
	}
	trashIt := c.action() == ConflictsRemoteTrash
	return env.finish(ctx, copyRel, copyRel+" (here until it is sent)", func(ctx context.Context) string {
		return c.sendAway(ctx, env, copyRel, trashIt)
	})
}

// sendAway uploads the local copy at copyRel without recording it — no client
// syncs where it is going — trashes it there when trashIt, and then removes the
// local one. Any failure leaves the local copy in place and says so.
func (c *Conflicts) sendAway(ctx context.Context, env conflictEnv, copyRel string, trashIt bool) string {
	src := env.abs(copyRel)
	err := c.upload(ctx, env, copyRel, src, false)
	if err == nil && trashIt {
		err = c.trashRemote(ctx, env, copyRel)
	}
	if err != nil {
		env.logf("[sync] conflict copy %s: %v (kept here instead)", copyRel, err)
		return copyRel + " (here, because sending it failed)"
	}
	_ = os.Remove(src)
	_ = os.Remove(filepath.Dir(src)) // only if empty
	if trashIt {
		return "the remote trash, as " + copyRel
	}
	return copyRel + " (on the remote)"
}

// upload sends the local file src to the remote at copyRel. With record, the
// result becomes a baseline like any pushed file's, so the change feed's report
// of it is recognised as our own and the folder's later edits and deletions
// sync as content does.
func (c *Conflicts) upload(ctx context.Context, env conflictEnv, copyRel, src string, record bool) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	rf, err := env.store.Put(ctx, copyRel, f)
	env.observe(err)
	if err != nil {
		return err
	}
	if record && env.state != nil {
		size, mtime := state.FingerprintOf(statOf(f))
		return env.state.SetEcho(copyRel, state.Echo{
			Hash: rf.Hash, Version: rf.Version, At: time.Now(),
			LocalSize: size, LocalMTime: mtime,
		})
	}
	return nil
}

// trashRemote trashes the object at copyRel, which upload has just put there.
func (c *Conflicts) trashRemote(ctx context.Context, env conflictEnv, copyRel string) error {
	t, ok := provider.AsTrasher(env.store)
	if !ok {
		// Open refuses this combination; reaching it means a store whose
		// capabilities changed. The copy is still kept, locally.
		return fmt.Errorf("conflicts = %s, but this backend has no trash", ConflictsRemoteTrash)
	}
	if err := t.Trash(ctx, copyRel); err != nil {
		return fmt.Errorf("trashing it: %w", err)
	}
	return nil
}

// origin is the path a trash entry records: under the mountpoint, so Restore
// writes through the mount and the file syncs, or under the backing tree when
// no mountpoint is known.
func (c *Conflicts) origin(rel string) string {
	if c.MountPath != "" {
		return filepath.Join(c.MountPath, filepath.FromSlash(rel))
	}
	return "/" + rel
}

func (c *Conflicts) alreadyKept(env conflictEnv, rel, identity string) bool {
	if c != nil {
		c.mu.Lock()
		hit := c.kept[rel] == identity
		c.mu.Unlock()
		if hit {
			return true
		}
	}
	if env.state == nil {
		return false
	}
	have, ok, err := env.state.Kept(rel)
	return err == nil && ok && have == identity
}

// remember records that identity has been kept at rel, in memory and in the
// state store, so that a restart between the pull's keep and the push's does
// not keep it twice. A record that cannot be written costs at most that
// duplicate, so it is logged and not an error.
func (c *Conflicts) remember(env conflictEnv, rel, identity string) {
	if c != nil {
		c.mu.Lock()
		if c.kept == nil {
			c.kept = map[string]string{}
		}
		c.kept[rel] = identity
		c.mu.Unlock()
	}
	if env.state != nil {
		if err := env.state.SetKept(rel, identity); err != nil {
			env.logf("[sync] conflict %s: recording the kept version: %v", rel, err)
		}
	}
}

// uniqueCopy returns copyRel, or copyRel with a counter before the extension if
// a file of that name is already in the folder: two local edits that lost in
// the same second are two copies, never one.
func uniqueCopy(env conflictEnv, copyRel string) string {
	if _, err := os.Lstat(env.abs(copyRel)); err != nil {
		return copyRel
	}
	ext := path.Ext(copyRel)
	stem := strings.TrimSuffix(copyRel, ext)
	for n := 2; ; n++ {
		candidate := fmt.Sprintf("%s %d%s", stem, n, ext)
		if _, err := os.Lstat(env.abs(candidate)); err != nil {
			return candidate
		}
	}
}
