// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package syncengine

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/syshlted/drivel/internal/claim"
)

// Another drivel's tree nested inside this mount's backing store (MC-47) is that
// mount's to sync, and until 2026-10-20 only the sweep's walk knew it: the walk
// prunes a subdirectory carrying a .drivel-backing marker or holding a live
// fuse.drivel mount, and every other pass went on regardless. A write through
// this mount into the nested tree was pushed to this mount's remote, a remote
// change under that path was written into the other mount's tree — which then
// sent it to the other mount's remote, another account's — and a remote
// deletion here deleted files there. foreignTrees is the one answer every pass
// now asks, so a subtree the walk prunes is one nothing else touches either.

// mountsRefresh is how long a reading of the mount table stands. mountinfo is a
// snapshot of the whole namespace and parsing it per push would cost more than
// the push's own bookkeeping; a nested mount that appears is not missed for
// longer than this, and the walk reads its own fresh copy.
const mountsRefresh = 5 * time.Second

// foreignTrees reports, for one mount's backing root, whether a root-relative
// path lies in another drivel's tree. A nil *foreignTrees reports nothing.
type foreignTrees struct {
	root     string
	now      func() time.Time
	mountsOf func(root string) map[string]bool // claim.MountsUnder; a test's in its place

	mu     sync.Mutex
	mounts map[string]bool
	read   time.Time
}

func newForeignTrees(root string) *foreignTrees {
	if root == "" {
		return nil
	}
	return &foreignTrees{root: root, now: time.Now, mountsOf: claim.MountsUnder}
}

// owner names the nested tree rel lies in — or is — and what kind it is, and
// reports false when rel is this mount's own. A live mountpoint is checked
// before the marker, so that an lstat never has to go through another
// drivel's FUSE mount to answer.
func (t *foreignTrees) owner(rel string) (dir, what string, ok bool) {
	if t == nil || rel == "" || rel == "." {
		return "", "", false
	}
	mounts := t.currentMounts()
	if len(mounts) > 0 {
		parts := strings.Split(rel, "/")
		for i := range parts {
			if q := strings.Join(parts[:i+1], "/"); mounts[q] {
				return q, "mountpoint", true
			}
		}
	}
	if q, ok := claim.NestedUnder(t.root, rel); ok {
		return q, "backing store", true
	}
	return "", "", false
}

// why is owner as the clause a log line ends with, or "" for a path of ours.
func (t *foreignTrees) why(rel string) string {
	dir, what, ok := t.owner(rel)
	if !ok {
		return ""
	}
	return fmt.Sprintf("inside another drivel mount's %s at %s/: that mount syncs it", what, dir)
}

func (t *foreignTrees) currentMounts() map[string]bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if now := t.now(); t.read.IsZero() || now.Sub(t.read) >= mountsRefresh {
		t.mounts = t.mountsOf(t.root)
		t.read = now
	}
	return t.mounts
}

// notSynced is the one answer to "does this mount sync p?", with the reason when
// it does not: a conflicts folder kept to itself (Conflicts.Syncs), or another
// drivel's nested tree. "" means it does.
func notSynced(c *Conflicts, t *foreignTrees, p string) string {
	if !c.Syncs(p) {
		return "in a conflicts folder this mount keeps to itself"
	}
	return t.why(p)
}

// nestedClaim is the marker half of foreignTrees.owner, for the status reader:
// the nested backing root rel lies in, or "". It reads no mount table, so a
// status request never parses mountinfo, and costs one Lstat per component.
func nestedClaim(root, rel string) string {
	if root == "" {
		return ""
	}
	q, _ := claim.NestedUnder(root, rel)
	return q
}
