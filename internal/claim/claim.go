// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

// Package claim marks a directory as a live drivel backing store, so that a
// second drivel process can see it.
//
// app.Validate keeps one process's mounts from overlapping, and it can only
// compare the specs it was handed. Two processes pointed at nesting directories
// were therefore unguarded, and the result is unbounded content multiplication
// with no component misbehaving (MC-47, docs/dev/multiclient-test-plan.md §4.18):
// A's sweep finds B's backing tree "local only" and pushes it, B materialises it
// one level deeper inside A's tree, and the paths grow by one level per sweep in
// each direction.
//
// A claim is a marker file at a backing root, held under an exclusive flock for
// as long as the mount is up. Three things read it. A mount opening refuses when
// its backing store, mountpoint or state DB lies under a *live* claim, which
// catches the nested child that starts second (Enclosing); the lock itself
// refuses a second process on the same backing directory (Acquire); and the
// sweep's walk prunes any subdirectory carrying a marker, which catches the
// parent that starts second, since a parent cannot find a marker below it
// without walking — and the walk is already visiting every file (Marked).
//
// The lock, not the file, is what "live" means: a marker left by a mount that is
// no longer running is stale, and is a warning rather than a refusal, because
// refusing would turn an old directory into a permanent veto.
//
// Leaf and dependency-free above golang.org/x/sys, so the sync engine can import
// it without importing the mount layer.
package claim

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Name is the marker's file name. It starts with ".drivel-" on purpose: the
// sweep's walk already skips that prefix, so the marker itself is never pushed.
const Name = ".drivel-backing"

// ErrHeld is returned by Acquire when another process holds the claim.
var ErrHeld = errors.New("held by another drivel process")

// Claim is a held marker. Release it when the mount is closed.
type Claim struct {
	f *os.File
}

// Release drops the lock. The marker file stays: it is what makes a stale
// backing store recognisable to the next sweep that meets it.
func (c *Claim) Release() error {
	if c == nil || c.f == nil {
		return nil
	}
	err := c.f.Close() // closing the descriptor releases the flock
	c.f = nil
	return err
}

// Acquire marks root as the backing store of the mount named owner and locks the
// marker for as long as the Claim is held. ErrHeld, wrapped with the holder's
// name, means another live process already serves this directory.
func Acquire(root, owner string) (*Claim, error) {
	p := filepath.Join(root, Name)
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := lockExclusive(f); err != nil {
		holder := readOwner(p)
		f.Close()
		if errors.Is(err, errWouldBlock) {
			return nil, fmt.Errorf("%s: %w (mount %q)", root, ErrHeld, holder)
		}
		return nil, err
	}
	body := []byte(markerText(owner))
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt(body, 0)
	}
	return &Claim{f: f}, nil
}

// Found describes a marker met above a path.
type Found struct {
	Dir   string // the directory holding the marker
	Owner string // the mount name recorded in it
	Live  bool   // another process holds its lock right now
}

// Enclosing walks up from dir (inclusive) to the filesystem root and reports the
// nearest marker it finds. ok is false when there is none.
func Enclosing(dir string) (found Found, ok bool) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Found{}, false
	}
	for d := filepath.Clean(abs); ; d = filepath.Dir(d) {
		p := filepath.Join(d, Name)
		if f, err := os.Open(p); err == nil {
			live := false
			if err := lockExclusive(f); err != nil {
				live = errors.Is(err, errWouldBlock)
			}
			f.Close() // also releases the probe's lock if it took one
			return Found{Dir: d, Owner: readOwner(p), Live: live}, true
		}
		if parent := filepath.Dir(d); parent == d {
			return Found{}, false
		}
	}
}

// Marked reports whether dir carries a marker: whether it is, or was, some
// drivel mount's backing store. One Lstat; the walk calls it per directory.
func Marked(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, Name))
	return err == nil
}

// NestedUnder reports the shallowest directory along rel — a root-relative slash
// path, rel itself included — that carries a marker strictly below root: the
// root of another drivel's backing store that rel lies in, or is. root's own
// marker is not consulted, since it is this mount's. One Lstat per component.
//
// It is the per-path form of the question the sweep's walk asks per directory
// (Marked), for the passes that meet one path at a time — the push, the pull,
// the delete rows — and must give the walk's answer, or a subtree the walk
// prunes is still written into by a pull and uploaded by a push.
func NestedUnder(root, rel string) (string, bool) {
	if rel == "" || rel == "." {
		return "", false
	}
	parts := strings.Split(rel, "/")
	for i := range parts {
		q := strings.Join(parts[:i+1], "/")
		if Marked(filepath.Join(root, filepath.FromSlash(q))) {
			return q, true
		}
	}
	return "", false
}

func markerText(owner string) string {
	return "drivel backing store for mount " + owner + "\n" +
		"This directory is synced by a running drivel; its contents are not to be synced again from a directory above it.\n"
}

// readOwner recovers the mount name from a marker, or "" if it cannot.
func readOwner(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	line, _, _ := bytes.Cut(b, []byte("\n"))
	return strings.TrimPrefix(string(line), "drivel backing store for mount ")
}

// HeldByAnother reports whether some other open file description holds an
// exclusive flock on f. bbolt holds one on every database it has open for
// writing — drivel's state DB and index among them — so this is how a mount
// recognises another process's live database inside its own backing tree,
// which no up-walk from either side can see when that process started first:
// the database is an ordinary file with a configurable name. Uploading one
// would publish a file caught mid-transaction, again on every commit.
//
// A flock taken through this mount reaches the backing file too, since
// 2026-10-20: the mount asks the kernel to forward flock(2) (vfs backend.go)
// and applies it to the backing descriptor, so a database opened through the
// mount is recognised as well. The mount announces the file again once such a
// lock goes (vfs flock.go), because a push declined here is not retried. A
// POSIX (fcntl) lock is still kept by the kernel and is invisible here.
func HeldByAnother(f *os.File) bool { return heldExclusively(f) }
