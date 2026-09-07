package gdrive

import (
	"context"
	"fmt"
	"path"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"

	"github.com/zishmusic/drivel/internal/provider"
)

// Initial enumeration (DESIGN.md §9, M7b).
//
// The change feed only ever reports what changed after a cursor was taken, so a
// Drive that existed before the first mount is invisible to M3–M7: nothing walks
// it. Enumerate is the one sweep that does — a flat files.list of the account,
// roughly one request per 1000 objects, metadata only.
//
// Three things make it more than a paginated listing:
//
//  1. **A flat listing has no parent-before-child guarantee.** An object whose
//     parent has not been seen yet cannot be given a path, so it is parked on
//     that parent's ID and resolved the moment the parent arrives. Anything still
//     parked when the sweep ends never reached our root and is dropped — the same
//     rule pathForIDLocked applies to the change feed, and the reason a sweep of a
//     subfolder mount stays correct while listing the whole account.
//  2. **It doubles as index warm-up.** Every resolved object is written to the
//     in-memory maps and the persistent index (M7) in one batched transaction per
//     page — Set per object would mean one fsync per file.
//  3. **Resolution during the sweep is local.** A complete sweep sees every
//     non-trashed object, so a parent that is missing from it is genuinely absent
//     (trashed, or not ours) rather than merely unseen. That is what makes "still
//     parked ⇒ outside the mount" sound, and it is why a *resumed* sweep needs the
//     persistent index to stand in for the pages it did not see (see beginSweep).
const enumPageSize = 1000

// sweepState is the in-flight state of one enumeration sweep. It lives on the
// Drive value rather than in the cursor because it is derived data — a restart
// rebuilds it from the persistent index, and beginSweep restarts the whole sweep
// when it cannot.
type sweepState struct {
	folders map[string]*drive.File   // id -> folder seen so far, for chain assembly
	waiting map[string][]*drive.File // parent id -> objects parked on it
	parked  int                      // how many are currently parked

	// Scoped descent (M7c): folders not yet listed, and whether this sweep is one.
	// The two halves are exclusive — a flat sweep never fills frontier and a
	// descent never parks — because the mode is fixed when the Drive is opened.
	scoped   bool
	frontier []folderTask
	fanout   int  // current concurrency, moved by adaptFanoutLocked toward what Drive allows
	requests int  // folder listings issued, which is a descent's whole cost
	warned   bool // whether this descent has already reported being folder-dense

	pages   int
	objects int
	emitted int
}

// sweepPage accumulates one page's resolved objects: the seam-facing view and the
// fileIDs behind them, which never cross the seam but do go into the index.
type sweepPage struct {
	files []provider.RemoteFile
	ids   []string
}

// Enumerate implements provider.Enumerator: one page of a flat sweep, resolved to
// paths under the mount root. next == "" means the sweep is complete.
func (d *Drive) Enumerate(ctx context.Context, cursor string) ([]provider.RemoteFile, string, error) {
	if d.scopedSweep() {
		return d.enumerateScoped(ctx, cursor)
	}
	d.mu.Lock()
	// Pin the concrete root ID before anything is placed. A file's parents carry
	// the real folder ID, never the "root" alias -drive-root defaults to, so
	// without this every object in the account looks like it sits outside the
	// mount — and a sweep that reports an empty tree is the one shape that is
	// genuinely dangerous above the seam, because every previously-synced path
	// then looks remotely deleted. Failing is the only safe answer.
	rootErr := d.resolveRootLocked(ctx)
	sw, cursor := d.beginSweepLocked(ctx, cursor)
	d.mu.Unlock()
	if rootErr != nil {
		return nil, "", fmt.Errorf("enumerate: resolving the mount root: %w", rootErr)
	}

	res, err := d.listPage(ctx, cursor)
	if err != nil {
		if !isPageTokenExpired(err) || cursor == "" {
			return nil, "", classify(err)
		}
		// The listing token died mid-sweep (they are not durable across a long
		// pause). Nothing is lost by starting over — the sweep is idempotent and
		// its consumer applies pages independently — so restart rather than
		// leaving enumeration permanently stuck on a dead token.
		d.logf("[drive] enumeration: page token expired; restarting the sweep")
		d.mu.Lock()
		d.sweep = nil
		sw, _ = d.beginSweepLocked(ctx, "")
		d.mu.Unlock()
		if res, err = d.listPage(ctx, ""); err != nil {
			return nil, "", classify(err)
		}
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	// Register this page's folders before resolving anything, so a page holding
	// both a folder and its children resolves in one pass instead of parking them
	// until the next one.
	for _, f := range res.Files {
		if f.MimeType == folderMIME {
			sw.folders[f.Id] = f
		}
	}
	var page sweepPage
	for _, f := range res.Files {
		d.sweepResolveLocked(ctx, sw, f, &page)
	}
	sw.pages++
	sw.objects += len(res.Files)
	sw.emitted += len(page.files)

	d.warmIndexLocked(ctx, &page)

	if res.NextPageToken == "" {
		d.logf("[drive] enumeration complete: %d page(s), %d object(s) listed, %d under the mount root, %d outside it",
			sw.pages, sw.objects, sw.emitted, sw.parked)
		d.sweep = nil
	} else {
		d.logf("[drive] enumeration: page %d, %d object(s) so far (%d under the mount root)",
			sw.pages, sw.objects, sw.emitted)
	}
	return page.files, res.NextPageToken, nil
}

// listPage fetches one listing page. It deliberately holds no lock: a page is a
// network round trip, and every other Drive operation would otherwise stall
// behind the whole sweep.
func (d *Drive) listPage(ctx context.Context, cursor string) (*drive.FileList, error) {
	call := d.svc.Files.List().
		Q("trashed = false").
		Spaces("drive").
		PageSize(enumPageSize).
		Fields(googleapi.Field("nextPageToken,files(" + fileFields + ")"))
	if cursor != "" {
		call = call.PageToken(cursor)
	}
	return call.Context(ctx).Do()
}

// beginSweepLocked returns the sweep this call belongs to, and the cursor to
// actually use.
//
// The interesting case is a resume into a fresh process: the pages a previous run
// consumed are gone from memory, so the parents they contained can only come from
// the persistent index. With the index unusable there is no way to place those
// children, and a sweep that silently omits a subtree is worse than a slow one —
// the reconcile above would read the gap as "deleted remotely". So we restart the
// sweep instead.
func (d *Drive) beginSweepLocked(ctx context.Context, cursor string) (*sweepState, string) {
	if d.sweep == nil {
		if cursor != "" && d.indexLocked(ctx) == nil {
			d.logf("[drive] enumeration: cannot resume without a usable path index; restarting the sweep")
			cursor = ""
		}
		d.sweep = &sweepState{
			folders: map[string]*drive.File{},
			waiting: map[string][]*drive.File{},
		}
	}
	return d.sweep, cursor
}

// sweepResolveLocked places one object, emitting it if its chain reaches our root
// and parking it on its parent's ID if the parent is not known yet.
//
// Resolving a folder releases whatever was parked on it, recursively — so a page
// that arrives child-first still costs one pass, and the release cannot loop
// because each object is parked at most once and removed when released.
func (d *Drive) sweepResolveLocked(ctx context.Context, sw *sweepState, f *drive.File, page *sweepPage) {
	parentID := ""
	if len(f.Parents) > 0 {
		parentID = f.Parents[0]
	}
	dir, ok := d.sweepParentLocked(ctx, sw, parentID)
	if !ok {
		if parentID == "" {
			return // no parent and not our root: nothing to hang it from
		}
		sw.waiting[parentID] = append(sw.waiting[parentID], f)
		sw.parked++
		return
	}

	p := path.Join(dir, f.Name)
	d.rememberIDLocked(p, f.Id)
	page.files = append(page.files, toRemoteFile(p, f))
	page.ids = append(page.ids, f.Id)

	if f.MimeType != folderMIME {
		return
	}
	parked := sw.waiting[f.Id]
	if len(parked) == 0 {
		return
	}
	delete(sw.waiting, f.Id)
	sw.parked -= len(parked)
	for _, child := range parked {
		d.sweepResolveLocked(ctx, sw, child, page)
	}
}

// sweepParentLocked answers "where does this parent folder live?" from local
// knowledge only — our root, what this sweep and this session have already
// placed, or a verified entry from the persistent index.
//
// It never falls back to walking the parent chain by ID over the network, and the
// omission is deliberate: a sweep lists the *whole account*, so most of what it
// cannot place is simply outside the mount root, and a per-object request for each
// would turn a cheap sweep into a quota disaster.
func (d *Drive) sweepParentLocked(ctx context.Context, sw *sweepState, parentID string) (string, bool) {
	if parentID == "" || d.isRootID(parentID) {
		return "", true
	}
	if p, ok := d.pathByID[parentID]; ok {
		return p, true
	}
	// A folder listed earlier in this sweep whose own chain we can now place.
	if f, ok := sw.folders[parentID]; ok {
		grandID := ""
		if len(f.Parents) > 0 {
			grandID = f.Parents[0]
		}
		if up, ok := d.sweepParentLocked(ctx, sw, grandID); ok {
			p := path.Join(up, f.Name)
			d.rememberIDLocked(p, f.Id)
			return p, true
		}
		return "", false
	}
	// Only reachable on a resumed sweep, for folders listed before the restart.
	// The M7 rule still applies: a stored entry is a hint until it is checked.
	if idx := d.indexLocked(ctx); idx != nil {
		if p, ok, err := idx.PathFor(parentID); err == nil && ok {
			if got, vok := d.verifyLocked(ctx, p, parentID); vok {
				d.rememberLocked(ctx, p, got)
				return p, true
			}
		}
	}
	return "", false
}

// rememberIDLocked records a path↔ID mapping in the in-memory maps only. The
// persistent half is written per page in one batch (see Enumerate), which is the
// difference between one commit per page and one per object.
func (d *Drive) rememberIDLocked(p, id string) { d.linkLocked(p, id) }

// warmIndexLocked records everything one sweep call resolved, in a single
// transaction. Set per object would mean one fsync per file, which on a large
// tree costs more than the listing did. Shared by both sweep modes.
func (d *Drive) warmIndexLocked(ctx context.Context, page *sweepPage) {
	idx := d.indexLocked(ctx)
	if idx == nil || len(page.ids) == 0 {
		return
	}
	paths := make([]string, len(page.files))
	for i := range page.files {
		paths[i] = page.files[i].Path
	}
	if err := idx.SetMany(paths, page.ids); err != nil {
		d.logf("[drive] path index: recording enumeration page: %v", err)
	}
}
