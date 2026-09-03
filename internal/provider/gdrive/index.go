package gdrive

import (
	"context"
	"fmt"
	"log"
	"path"
	"strings"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"

	"github.com/zishmusic/drivel/internal/pathindex"
	"github.com/zishmusic/drivel/internal/provider"
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
			log.Printf("[drive] path index: %d entries carried over from a previous run", n)
		} else {
			log.Printf("[drive] path index: new (different account or root folder); starting empty")
		}
	}
	return d.idx
}

func (d *Drive) warnIndexOnce(format string, args ...any) {
	if d.idxWarned {
		return
	}
	d.idxWarned = true
	log.Printf("[drive] path index: "+format, args...)
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
		log.Printf("[drive] path index: reading %s: %v", p, err)
		return "", false
	}
	if !ok {
		return "", false
	}
	f, ok := d.verifyLocked(ctx, p, id)
	if !ok {
		if err := idx.Forget(p); err != nil {
			log.Printf("[drive] path index: dropping stale %s: %v", p, err)
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
		log.Printf("[drive] path index: resolving root folder: %v", err)
		return nil, false
	}
	got, err := d.svc.Files.Get(id).Fields(fileFields).Context(ctx).Do()
	if err != nil {
		if !isNotFound(err) {
			log.Printf("[drive] path index: verifying %s: %v", p, err)
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
		log.Printf("[drive] lookup %q: %v", name, err)
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
	log.Printf("[drive] %d remote files share the name %q in one folder; using the most recently modified (%s). The others are not visible at this path.",
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
	// Evict the stale halves a rewrite leaves behind, so the two maps stay exact
	// inverses: the ID this path used to name (the object was replaced) and the
	// path this ID used to live at (it moved). A leftover reverse entry would
	// resolve a change-feed ID to a path it no longer occupies.
	if old, ok := d.idByPath[p]; ok && old != f.Id {
		delete(d.pathByID, old)
	}
	if oldPath, ok := d.pathByID[f.Id]; ok && oldPath != p {
		delete(d.idByPath, oldPath)
	}
	d.idByPath[p] = f.Id
	d.pathByID[f.Id] = p

	if idx := d.indexLocked(ctx); idx != nil {
		if err := idx.Set(p, f.Id); err != nil {
			log.Printf("[drive] path index: recording %s: %v", p, err)
		}
	}
	return toRemoteFile(p, f)
}

// forgetLocked drops p (and, for a directory, all descendants) from both indexes.
func (d *Drive) forgetLocked(ctx context.Context, p string) {
	prefix := p + "/"
	for q, id := range d.idByPath {
		if q == p || strings.HasPrefix(q, prefix) {
			delete(d.idByPath, q)
			delete(d.pathByID, id)
		}
	}
	if idx := d.indexLocked(ctx); idx != nil {
		if err := idx.Forget(p); err != nil {
			log.Printf("[drive] path index: forgetting %s: %v", p, err)
		}
	}
}

// reindexLocked rewrites index keys after moving oldPath -> newPath, including any
// descendants of a moved directory.
func (d *Drive) reindexLocked(ctx context.Context, oldPath, newPath string) {
	oldPrefix := oldPath + "/"
	moves := map[string]string{oldPath: newPath}
	for q := range d.idByPath {
		if strings.HasPrefix(q, oldPrefix) {
			moves[q] = newPath + "/" + strings.TrimPrefix(q, oldPrefix)
		}
	}
	for from, to := range moves {
		id := d.idByPath[from]
		delete(d.idByPath, from)
		d.idByPath[to] = id
		d.pathByID[id] = to
	}
	if idx := d.indexLocked(ctx); idx != nil {
		if err := idx.Rename(oldPath, newPath); err != nil {
			log.Printf("[drive] path index: renaming %s -> %s: %v", oldPath, newPath, err)
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
		log.Printf("[drive] path index: reverse lookup %s: %v", id, err)
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
		log.Printf("[drive] resolving root folder: %v", err)
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
			log.Printf("[drive] path index: reverse lookup %s: %v", id, err)
		case ok:
			if f, vok := d.verifyLocked(ctx, p, id); vok {
				d.rememberLocked(ctx, p, f)
				return p, true
			}
			if err := idx.Forget(p); err != nil {
				log.Printf("[drive] path index: dropping stale %s: %v", p, err)
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
