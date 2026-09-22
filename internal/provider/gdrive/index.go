// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package gdrive

import (
	"context"
	"fmt"
	"path"
	"strings"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"

	"github.com/zishmusic/drivel/internal/pathindex"
	"github.com/zishmusic/drivel/provider"
)

// Path↔fileID translation (DESIGN.md §2.5, §9 M7).
//
// Drive is ID-addressed and the seam above us is path-addressed, so every
// operation begins by answering "which fileID is this path?". There are three
// sources for that answer and resolveLocked tries them in increasing order of
// cost:
//
//  1. the in-memory maps — everything this process has already learned;
//  2. the persistent index — what a previous process learned, verified against
//     the remote before it is believed;
//  3. Drive itself — a name lookup, one component at a time.
//
// Only (3) is a source of truth. (1) is derived from the API within this
// session, and the pull loop keeps it current. (2) is a cache written by a
// process that may have exited long ago, so it is a *hint*: verified on first
// use, dropped when it no longer matches, and never required for a correct
// answer. Deleting the index DB must cost latency and quota, nothing else.
//
// Before M7 there was no (2) and no (3), which made a cold index quietly
// destructive rather than merely slow: an unknown path meant "does not exist
// remotely", so a restart followed by one edit created a *second* file beside the
// real one — under a second copy of every parent folder, since Drive allows
// same-name siblings — while Stat's false "absent" also disabled M6's
// unchanged-content check and re-uploaded whole files. (3) is what makes the
// index genuinely self-rebuilding, and therefore what makes (2) safe to keep.

// indexState tracks how far the persistent index has got.
type indexState int

const (
	indexUnbound indexState = iota // opened, identity not yet established
	indexReady                     // bound to this account+root; usable
	indexOff                       // unusable this session; memory-only
)

// indexLocked returns the persistent index if it is bound and usable, else nil.
//
// Binding is deferred to first use rather than done at Open for two reasons: it
// needs a network round trip, and mount must not depend on the network to come
// up. It happens on the same code path that would use the data, so nothing on
// disk is ever read before we know whose it is.
func (d *Drive) indexLocked(ctx context.Context) *pathindex.Store {
	if d.idx == nil || d.idxState == indexOff {
		return nil
	}
	if d.idxState == indexReady {
		return d.idx
	}

	identity, err := d.identityLocked(ctx)
	if err != nil {
		// A transient failure leaves the index unbound so the next call retries;
		// a permanent one (revoked scope, wrong root) turns it off for good. Either
		// way we carry on memory-only, because this is a cache.
		if isTransient(err) {
			d.warnIndexOnce("cannot identify account yet: %v (running memory-only for now)", err)
			return nil
		}
		d.warnIndexOnce("disabled: cannot identify account: %v", err)
		d.idxState = indexOff
		return nil
	}
	reused, err := d.idx.Bind(identity)
	if err != nil {
		d.warnIndexOnce("disabled: %v", err)
		d.idxState = indexOff
		return nil
	}
	d.idxState = indexReady
	if n, err := d.idx.Len(); err == nil {
		if reused {
			d.logf("[drive] path index: %d entries carried over from a previous run", n)
		} else {
			d.logf("[drive] path index: new (different account or root folder); starting empty")
		}
	}
	return d.idx
}

func (d *Drive) warnIndexOnce(format string, args ...any) {
	if d.idxWarned {
		return
	}
	d.idxWarned = true
	d.logf("[drive] path index: "+format, args...)
}

// identityLocked names the (account, root folder) pair this index describes, so
// reusing a DB against a second account or a different -drive-root wipes it
// instead of resolving one account's paths to another's IDs.
//
// The account half is the permissionId — an opaque per-user ID — rather than the
// email address, which keeps an identifiable address out of a file the user did
// not ask to hold one.
func (d *Drive) identityLocked(ctx context.Context) (string, error) {
	if err := d.resolveRootLocked(ctx); err != nil {
		return "", err
	}
	about, err := d.svc.About.Get().Fields("user(permissionId)").Context(ctx).Do()
	if err != nil {
		return "", classify(err)
	}
	if about.User == nil || about.User.PermissionId == "" {
		return "", fmt.Errorf("drive reported no account identity")
	}
	return about.User.PermissionId + "\x00" + d.rootID, nil
}

// resolveRootLocked pins the concrete folder ID of our root. The configured root
// is usually the alias "root", which never appears in a file's parents, so
// verification needs the real ID to compare against.
func (d *Drive) resolveRootLocked(ctx context.Context) error {
	if d.rootID != "" {
		return nil
	}
	if d.root != "" && d.root != "root" {
		d.rootID = d.root
		return nil
	}
	got, err := d.svc.Files.Get("root").Fields("id").Context(ctx).Do()
	if err != nil {
		return classify(err)
	}
	d.rootID = got.Id
	return nil
}

// isRootID reports whether id names our mount root, under either spelling.
func (d *Drive) isRootID(id string) bool {
	if id == "" {
		return d.root == ""
	}
	return id == d.root || id == d.rootID
}

// sameFolder compares two folder IDs, treating the "root" alias and the concrete
// root ID as the same folder.
func (d *Drive) sameFolder(a, b string) bool {
	return a == b || (d.isRootID(a) && d.isRootID(b))
}

// --- resolution -------------------------------------------------------------

// resolveLocked maps a root-relative path to its Drive fileID, ok=false if no
// such object exists remotely. See the package-level note above for the three
// sources it consults.
func (d *Drive) resolveLocked(ctx context.Context, p string) (string, bool) {
	if p == "" || p == "." || p == "/" {
		return d.root, true
	}
	if id, ok := d.idByPath[p]; ok {
		return id, true
	}
	if id, ok := d.fromIndexLocked(ctx, p); ok {
		return id, true
	}
	return d.lookupLocked(ctx, p)
}

// fromIndexLocked answers from the persistent index, but only after confirming
// the stored mapping still describes the remote.
//
// Skipping that check is the one way persistence could lose data that the
// in-memory index never could. While drivel was down, someone else may have
// moved, renamed, replaced or deleted that object; the ID then still resolves,
// just to a different file in a different place. Handing it to Files.Update
// would overwrite a file the user never touched, with no conflict copy and no
// event to notice it by. So an unverified entry is worth exactly nothing, and a
// failed check drops the whole subtree — if a directory is not where we left it,
// nothing recorded beneath it can be trusted either.
func (d *Drive) fromIndexLocked(ctx context.Context, p string) (string, bool) {
	idx := d.indexLocked(ctx)
	if idx == nil {
		return "", false
	}
	id, ok, err := idx.Lookup(p)
	if err != nil {
		d.logf("[drive] path index: reading %s: %v", p, err)
		return "", false
	}
	if !ok {
		return "", false
	}
	f, ok := d.verifyLocked(ctx, p, id)
	if !ok {
		if err := idx.Forget(p); err != nil {
			d.logf("[drive] path index: dropping stale %s: %v", p, err)
		}
		return "", false
	}
	d.rememberLocked(ctx, p, f)
	return id, true
}

// verifyLocked checks that id is still the object at path p: it exists, is not
// trashed, still carries that name, and still sits under the folder p names as
// its parent. The parent side recurses through resolveLocked, so verifying a
// deep path also verifies its ancestors — each of which is then cached in memory
// and costs nothing for the rest of the session.
//
// It fetches the full projection because the caller wants the metadata anyway;
// a cheaper field list would only force a second request.
func (d *Drive) verifyLocked(ctx context.Context, p, id string) (*drive.File, bool) {
	// The parent comparison below is against real folder IDs, and the configured
	// root is usually the "root" alias, which never appears in a file's parents.
	// Unable to name our own root, we cannot verify anything.
	if err := d.resolveRootLocked(ctx); err != nil {
		d.logf("[drive] path index: resolving root folder: %v", err)
		return nil, false
	}
	got, err := d.svc.Files.Get(id).Fields(fileFields).Context(ctx).Do()
	if err != nil {
		if !isNotFound(err) {
			d.logf("[drive] path index: verifying %s: %v", p, err)
		}
		return nil, false
	}
	if got.Trashed || got.Name != path.Base(p) {
		return nil, false
	}
	parentID, ok := d.resolveLocked(ctx, path.Dir(p))
	if !ok {
		return nil, false
	}
	for _, par := range got.Parents {
		if d.sameFolder(par, parentID) {
			return got, true
		}
	}
	return nil, false
}

// lookupLocked asks Drive for the path directly, resolving the parent first and
// then matching one name inside it. This is the self-rebuilding path: it is how
// a lost, wiped or invalidated index recovers, and how we find objects another
// client created while we were not running.
func (d *Drive) lookupLocked(ctx context.Context, p string) (string, bool) {
	parentID, ok := d.resolveLocked(ctx, path.Dir(p))
	if !ok {
		return "", false // no parent folder remotely => no child either
	}
	f, ok := d.lookupChildLocked(ctx, parentID, path.Base(p))
	if !ok {
		return "", false
	}
	d.rememberLocked(ctx, p, f)
	return f.Id, true
}

// lookupChildLocked finds the non-trashed child of parentID named name.
func (d *Drive) lookupChildLocked(ctx context.Context, parentID, name string) (*drive.File, bool) {
	q := fmt.Sprintf("name = '%s' and '%s' in parents and trashed = false",
		escapeQuery(name), escapeQuery(parentID))
	res, err := d.svc.Files.List().
		Q(q).
		Spaces("drive").
		Fields(googleapi.Field("files(" + fileFields + ")")).
		// One page is enough: the answer is normally a single file, and the
		// tie-break below only has to be defensible, not exhaustive.
		PageSize(100).
		Context(ctx).Do()
	if err != nil {
		d.logf("[drive] lookup %q: %v", name, err)
		return nil, false
	}
	switch len(res.Files) {
	case 0:
		return nil, false
	case 1:
		return res.Files[0], true
	}
	// Drive allows same-name siblings; a POSIX directory cannot represent them, so
	// one of them has to stand for the path. Pick the most recently modified — the
	// one a user would call "the current file" — and say so, because whichever we
	// pick, the others are now invisible to this mount. (Drive stamps RFC3339 in
	// UTC, so lexical order is chronological order.)
	best := res.Files[0]
	for _, f := range res.Files[1:] {
		if f.ModifiedTime > best.ModifiedTime {
			best = f
		}
	}
	d.logf("[drive] %d remote files share the name %q in one folder; using the most recently modified (%s). The others are not visible at this path.",
		len(res.Files), name, best.Id)
	return best, true
}

// escapeQuery escapes a literal for a Drive query string, where values are
// single-quoted and backslash is the escape character.
func escapeQuery(s string) string {
	return strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s)
}

// --- index maintenance ------------------------------------------------------

// rememberLocked records the path↔ID mapping in memory and in the persistent
// index, and returns the RemoteFile view.
func (d *Drive) rememberLocked(ctx context.Context, p string, f *drive.File) provider.RemoteFile {
	d.linkLocked(p, f.Id)
	if idx := d.indexLocked(ctx); idx != nil {
		if err := idx.Set(p, f.Id); err != nil {
			d.logf("[drive] path index: recording %s: %v", p, err)
		}
	}
	return toRemoteFile(p, f)
}

// linkLocked records p↔id in the in-memory maps and the child index.
//
// It evicts the stale halves a rewrite leaves behind, so the two maps stay exact
// inverses: the ID this path used to name (the object was replaced) and the path
// this ID used to live at (it moved). A leftover reverse entry would resolve a
// change-feed ID to a path it no longer occupies.
//
// Every in-memory mutation goes through here and unlinkLocked, so the child index
// below cannot drift from the maps it describes.
func (d *Drive) linkLocked(p, id string) {
	if old, ok := d.idByPath[p]; ok && old != id {
		delete(d.pathByID, old)
	}
	if oldPath, ok := d.pathByID[id]; ok && oldPath != p {
		d.unlinkLocked(oldPath)
	}
	d.idByPath[p] = id
	d.pathByID[id] = p
	d.linkKidLocked(p)
}

// unlinkLocked drops exactly one path, leaving any descendants in place. It is
// the eviction half of linkLocked, not the delete a caller wants — see
// forgetLocked for that.
func (d *Drive) unlinkLocked(p string) {
	id, ok := d.idByPath[p]
	if !ok {
		return
	}
	delete(d.idByPath, p)
	delete(d.pathByID, id)
	d.unlinkKidLocked(p)
}

// forgetLocked drops p (and, for a directory, all descendants) from both indexes.
//
// Forgetting the root ("") therefore empties the whole in-memory index. That is a
// change from the full-scan version, which matched descendants by the "p/" prefix
// and so dropped only the root's own entry while leaving every path beneath it —
// even though pathindex.Forget("") has always wiped the persistent half. The two
// halves now agree, in the safe direction: a dropped cache entry costs a lookup,
// a stale one can resolve an ID to the wrong object.
func (d *Drive) forgetLocked(ctx context.Context, p string) {
	for _, q := range d.subtreeLocked(p) {
		if id, ok := d.idByPath[q]; ok {
			delete(d.idByPath, q)
			delete(d.pathByID, id)
		}
		delete(d.kids, q)
	}
	d.unlinkKidLocked(p)
	if idx := d.indexLocked(ctx); idx != nil {
		if err := idx.Forget(p); err != nil {
			d.logf("[drive] path index: forgetting %s: %v", p, err)
		}
	}
}

// --- child index ------------------------------------------------------------
//
// kids maps a directory path to the set of paths recorded directly beneath it,
// which is the whole reason forgetLocked and reindexLocked are not quadratic.
//
// Before it existed both scanned the entire idByPath map to find descendants, on
// every single call. Under `rm -rf` that is one full-map scan per unlinked file
// with the mount's whole tree in the map, and it runs with d.mu held — the one
// lock every Drive operation takes. Measured, it was clean N^2: 5k deletes over a
// 5k-entry map took 224ms, 10k took 968ms, 20k took 3.9s, which extrapolates to
// hours for a large Drive. A leaf is now O(1) and a directory costs its own
// subtree.
//
// The invariant is that a path reachable in idByPath is reachable by walking kids
// from the root, so the two never disagree about what lies beneath a path. That
// is why linkKidLocked links the whole ancestor chain (a path may be recorded
// before its parents are) and unlinkKidLocked cascades upward (an interior node
// that holds no mapping and has no children left must not keep its own parent's
// set alive).

// linkKidLocked adds p to its parent's child set, and its parent to its
// grandparent's, up to the first ancestor already linked.
func (d *Drive) linkKidLocked(p string) {
	if d.kids == nil {
		d.kids = map[string]map[string]struct{}{}
	}
	for p != "" {
		parent := parentOf(p)
		set := d.kids[parent]
		if set == nil {
			set = map[string]struct{}{}
			d.kids[parent] = set
		}
		if _, ok := set[p]; ok {
			return // this chain is already linked all the way up
		}
		set[p] = struct{}{}
		p = parent
	}
}

// unlinkKidLocked removes p from its parent's child set once nothing beneath p
// is recorded any more, cascading upward through ancestors that become empty.
func (d *Drive) unlinkKidLocked(p string) {
	for {
		if len(d.kids[p]) > 0 {
			return // descendants still recorded; the chain has to stay reachable
		}
		delete(d.kids, p)
		if p == "" {
			return // the root has no parent to be removed from
		}
		if _, ok := d.idByPath[p]; ok {
			return // p holds a mapping of its own; its parent must still point at it
		}
		parent := parentOf(p)
		set := d.kids[parent]
		if set == nil {
			return
		}
		delete(set, p)
		p = parent
	}
}

// subtreeLocked returns p followed by every path recorded beneath it.
func (d *Drive) subtreeLocked(p string) []string {
	out := []string{p}
	for i := 0; i < len(out); i++ {
		for q := range d.kids[out[i]] {
			out = append(out, q)
		}
	}
	return out
}

// parentOf returns the containing directory of a root-relative path. The root's
// parent is itself the empty string, which is why callers stop at "".
func parentOf(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return ""
}

// reindexLocked rewrites index keys after moving oldPath -> newPath, including any
// descendants of a moved directory.
func (d *Drive) reindexLocked(ctx context.Context, oldPath, newPath string) {
	moved := d.subtreeLocked(oldPath)
	ids := make([]string, len(moved))
	for i, from := range moved {
		ids[i] = d.idByPath[from]
	}
	// Drop every old key before writing any new one: the two key sets can overlap
	// (a sibling rename like a -> ab), and unlinking after linking would remove
	// what was just written. Same reasoning as pathindex.Rename.
	for _, from := range moved {
		d.unlinkLocked(from)
		delete(d.kids, from)
	}
	d.unlinkKidLocked(oldPath)
	for i, from := range moved {
		if ids[i] == "" {
			continue // no mapping of its own; it was only an interior node
		}
		d.linkLocked(newPath+strings.TrimPrefix(from, oldPath), ids[i])
	}
	if idx := d.indexLocked(ctx); idx != nil {
		if err := idx.Rename(oldPath, newPath); err != nil {
			d.logf("[drive] path index: renaming %s -> %s: %v", oldPath, newPath, err)
		}
	}
}

// knownPathForIDLocked resolves a fileID to a path from the indexes alone, with
// no request and no verification.
//
// It exists for removals, which are the one case where verification is not
// available: the change feed reports a deleted file as a bare ID with no record
// attached, so there is nothing left on the remote to check the mapping against.
// The stored path is therefore used as-is — the same thing the in-memory index
// has always done, and safe for the same reason: changes arrive in order, so a
// move that happened before the delete has already been applied and has already
// corrected the mapping. An ID we have never heard of is outside our subtree and
// the caller skips it.
func (d *Drive) knownPathForIDLocked(ctx context.Context, id string) (string, bool) {
	if p, ok := d.pathByID[id]; ok {
		return p, true
	}
	idx := d.indexLocked(ctx)
	if idx == nil {
		return "", false
	}
	p, ok, err := idx.PathFor(id)
	if err != nil {
		d.logf("[drive] path index: reverse lookup %s: %v", id, err)
		return "", false
	}
	return p, ok
}

// pathForIDLocked resolves a fileID to its root-relative path. ok is false if the
// chain does not reach our root (i.e. the object is outside the mounted subtree).
//
// This is the change feed's direction, and the one where the persistent index
// earns the most: without it, the first mention of any ID after a restart costs
// one request per level of nesting to walk back to the root. With it, a hit
// costs one verification.
func (d *Drive) pathForIDLocked(ctx context.Context, id string) (string, bool) {
	// Pin the concrete root ID first. A file's parents carry the real ID, never the
	// "root" alias that -drive-root defaults to, so without this the walk never
	// recognises the root it is walking toward: it climbs to My Drive, finds a
	// folder with no parents, and reports the object as outside our subtree. Every
	// change to a top-level file was dropped that way.
	if err := d.resolveRootLocked(ctx); err != nil {
		d.logf("[drive] resolving root folder: %v", err)
	}
	if id == "" || d.isRootID(id) {
		return "", true
	}
	if p, ok := d.pathByID[id]; ok {
		return p, true
	}
	if idx := d.indexLocked(ctx); idx != nil {
		p, ok, err := idx.PathFor(id)
		switch {
		case err != nil:
			d.logf("[drive] path index: reverse lookup %s: %v", id, err)
		case ok:
			if f, vok := d.verifyLocked(ctx, p, id); vok {
				d.rememberLocked(ctx, p, f)
				return p, true
			}
			if err := idx.Forget(p); err != nil {
				d.logf("[drive] path index: dropping stale %s: %v", p, err)
			}
		}
	}
	got, err := d.svc.Files.Get(id).Fields(fileFields).Context(ctx).Do()
	if err != nil || len(got.Parents) == 0 {
		return "", false
	}
	parent, ok := d.pathForIDLocked(ctx, got.Parents[0])
	if !ok {
		return "", false
	}
	p := path.Join(parent, got.Name)
	d.rememberLocked(ctx, p, got)
	return p, true
}
