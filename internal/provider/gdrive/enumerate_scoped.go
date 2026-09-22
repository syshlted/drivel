// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package gdrive

import (
	"context"
	"fmt"
	"path"
	"sync"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"

	"github.com/zishmusic/drivel/provider"
)

// Scoped enumeration (DESIGN.md §9, M7c).
//
// M7b's sweep lists the whole account and filters to the mount root afterwards,
// which makes its cost a function of the Drive rather than of the folder being
// mounted: one request per 1000 objects *in the account*, fetched strictly in
// sequence because a page token cannot be prefetched, and paid again in full by
// every other mount of the same account. On a Drive holding 10^6 files that is
// minutes per mount per sweep.
//
// Drive offers nothing cheaper to ask for. There is no recursive "everything
// below this folder" query; `'ID' in parents` returns direct children only. So a
// scoped sweep is a breadth-first descent — one listing per folder, with the
// frontier fanned out over enumFanout concurrent requests per call.
//
// Two things fall out of the descent that are worth more than the speed.
//
// It is **parent-first by construction**, so none of M7b's parking machinery
// applies: a child is only ever discovered by listing its parent, so its path is
// known the moment it appears. sweepState.waiting, the parked count and the
// "still parked ⇒ outside the mount" rule belong to the flat path alone.
//
// And **resume stops mattering**. M7b persists a sweep cursor because losing a
// full-account sweep is expensive; a descent of the folder actually mounted is
// short enough to restart, which is what a cursor arriving with no frontier
// behind it does.
//
// What this is not is a free win. A descent costs about one request per folder
// against flat's one per 1000 account objects, so a subtree holding more folders
// than the account holds thousands of objects is cheaper to sweep flat. That is
// why the mode is selectable, why auto keeps flat for whole-drive mounts, and
// why a descent that is spending far more requests than objects says so.

// enumFanout is how many folder listings one Enumerate call issues at once. The
// requests are independent, which is exactly what the flat sweep's page tokens
// never are, and that independence is where the wall-clock win comes from. Eight
// sits well inside Drive's per-user rate limit while turning a sequential minute
// into seconds.
const enumFanout = 8

// enumScopedWarn is the request count past which a descent reports its own cost.
// A folder-dense subtree can need more requests than listing the account would,
// and a cost the user cannot see is one they report as a hang.
const enumScopedWarn = 2000

// scopedCursor is what Enumerate returns while a descent is in progress. The
// real state is the frontier, which lives in memory: unlike the flat sweep's
// Drive page token there is nothing here worth persisting, because restarting a
// scoped sweep costs one walk of the folder that was mounted.
const scopedCursor = "scoped"

// folderTask is one unit of a descent: list this folder's children, resuming
// from pageToken when the folder holds more than one page of them.
type folderTask struct {
	id        string
	path      string // the folder's path under the mount root; "" is the root itself
	pageToken string
}

// scopedSweep reports whether this mount descends its own subtree rather than
// listing the account. sweepMode is fixed at open time, so this needs no lock.
//
// auto keys off the *configured* root rather than the resolved one: "" and
// "root" name the whole Drive, where a descent has no subtree to save and would
// pay a request per folder for the privilege.
func (d *Drive) scopedSweep() bool {
	switch d.sweepMode {
	case SweepFlat:
		return false
	case SweepScoped:
		return true
	default:
		return d.root != "" && d.root != "root"
	}
}

// enumFanoutOrDefault is how many listings this Drive issues at once.
func (d *Drive) enumFanoutOrDefault() int {
	if d.fanout > 0 {
		return d.fanout
	}
	return enumFanout
}

// enumerateScoped implements one call of the descent. It returns everything the
// batch of folders held, and scopedCursor for as long as the frontier is not
// empty.
func (d *Drive) enumerateScoped(ctx context.Context, cursor string) ([]provider.RemoteFile, string, error) {
	d.mu.Lock()
	if err := d.resolveRootLocked(ctx); err != nil {
		d.mu.Unlock()
		// The flat sweep's rule, for the same reason: a sweep that cannot find its
		// root must fail rather than report an empty tree, because every
		// previously-synced path would then look remotely deleted.
		return nil, "", fmt.Errorf("enumerate: resolving the mount root: %w", err)
	}
	sw := d.beginScopedSweepLocked(cursor)
	batch := sw.take(sw.fanout)
	d.mu.Unlock()

	if len(batch) == 0 {
		d.mu.Lock()
		d.logf("[drive] scoped enumeration complete: %d folder listing(s), %d object(s) under the mount root",
			sw.requests, sw.objects)
		d.sweep = nil
		d.mu.Unlock()
		return nil, "", nil
	}

	lists, errs := d.listBatch(ctx, batch)

	d.mu.Lock()
	defer d.mu.Unlock()

	var page sweepPage
	var retry []folderTask
	var firstErr error
	for i, t := range batch {
		if errs[i] != nil {
			// Back on the frontier, at the front, so a failed folder is retried
			// before the sweep goes deeper. Dropping it would silently omit a
			// subtree, which reconcile reads as "deleted remotely".
			retry = append(retry, t)
			if firstErr == nil {
				firstErr = errs[i]
			}
			continue
		}
		d.scopedResolveLocked(sw, t, lists[i], &page)
	}
	if len(retry) > 0 {
		sw.frontier = append(retry, sw.frontier...)
	}
	d.adaptFanoutLocked(sw, len(retry))
	d.warmIndexLocked(ctx, &page)

	if len(retry) == len(batch) && firstErr != nil {
		// Nothing at all got through. Report it so the caller backs off rather than
		// spinning against a provider that is already saying no.
		return nil, "", firstErr
	}

	if len(sw.frontier) == 0 {
		d.logf("[drive] scoped enumeration complete: %d folder listing(s), %d object(s) under the mount root",
			sw.requests, sw.objects)
		d.sweep = nil
		return page.files, "", nil
	}
	if sw.requests > enumScopedWarn && !sw.warned {
		sw.warned = true
		d.logf("[drive] scoped enumeration has spent %d request(s) on %d object(s): this subtree is folder-dense, "+
			"and sweep-mode \"flat\" may cost fewer requests against a Drive this size",
			sw.requests, sw.objects)
	}
	d.logf("[drive] scoped enumeration: %d folder listing(s), %d object(s), %d folder(s) queued",
		sw.requests, sw.objects, len(sw.frontier))
	return page.files, scopedCursor, nil
}

// beginScopedSweepLocked returns the descent this call belongs to, seeding a new
// one from the mount root when there is none.
func (d *Drive) beginScopedSweepLocked(cursor string) *sweepState {
	if d.sweep != nil {
		return d.sweep
	}
	if cursor != "" {
		// A cursor from a previous process, whose frontier died with it. The flat
		// sweep leans on the persistent index to resume; a descent has nothing to
		// lean on and needs nothing, because starting over costs one walk of the
		// folder that was actually mounted.
		d.logf("[drive] enumeration: restarting the scoped sweep (a descent does not resume across a restart)")
	}
	d.sweep = &sweepState{
		scoped:   true,
		frontier: []folderTask{{id: d.rootID}},
		fanout:   d.enumFanoutOrDefault(),
	}
	return d.sweep
}

// adaptFanoutLocked moves the descent's concurrency toward what the provider is
// actually tolerating: halve it on any throttled listing, grow it back by one on
// a clean batch, never below 1 or above the configured ceiling.
//
// This is the standard answer to rate limiting and it is what makes the ceiling
// safe to pick without knowing the account. Drive's throttle is per user, so the
// number that works depends on what else the user is doing, and a fixed eight is
// a guess that cannot be right for everyone. Halving on failure and growing by
// one on success backs off fast and recovers slowly, which is the asymmetry that
// keeps a throttled sweep from oscillating.
func (d *Drive) adaptFanoutLocked(sw *sweepState, throttled int) {
	ceiling := d.enumFanoutOrDefault()
	switch {
	case throttled > 0:
		if sw.fanout > 1 {
			sw.fanout /= 2
			d.logf("[drive] scoped enumeration: %d listing(s) throttled; concurrency now %d",
				throttled, sw.fanout)
		}
	case sw.fanout < ceiling:
		sw.fanout++
	}
}

// take removes up to n folders from the frontier and returns them.
func (sw *sweepState) take(n int) []folderTask {
	if n > len(sw.frontier) {
		n = len(sw.frontier)
	}
	batch := make([]folderTask, n)
	copy(batch, sw.frontier)
	sw.frontier = sw.frontier[n:]
	return batch
}

// listBatch lists every folder in the batch concurrently, holding no lock: a
// listing is a network round trip, and every other Drive operation would
// otherwise stall behind the whole sweep.
//
// Results are returned per task rather than collapsed to the first error, because
// the folders are independent and the caller must be able to keep the ones that
// worked. Discarding a whole batch for one failure is not merely wasteful under
// throttling, it is unstable: at fanout 8 a 20% failure rate spoils ~83% of
// batches, and re-issuing all eight each time turns a throttle into a storm. It
// was measured at 97x before this returned anything but the first error.
//
// Each error is classified here so the caller — and the engine above the seam —
// can tell "try again" from "give up" without importing this package.
func (d *Drive) listBatch(ctx context.Context, batch []folderTask) ([]*drive.FileList, []error) {
	lists := make([]*drive.FileList, len(batch))
	errs := make([]error, len(batch))

	var wg sync.WaitGroup
	for i, t := range batch {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lists[i], errs[i] = d.listChildren(ctx, t)
			errs[i] = classify(errs[i])
		}()
	}
	wg.Wait()
	return lists, errs
}

// listChildren fetches one page of one folder's direct children, retrying from
// the folder's first page if the token it was given has aged out.
//
// A listing token cannot be renewed, so re-listing is the only way to finish the
// folder. It re-emits children the sweep already reported, which costs a repeat
// listing and nothing else: pages are applied independently above the seam, and
// a path resolved twice to the same ID is the same write twice.
func (d *Drive) listChildren(ctx context.Context, t folderTask) (*drive.FileList, error) {
	res, err := d.listChildrenPage(ctx, t)
	if err != nil && t.pageToken != "" && isPageTokenExpired(err) {
		d.logf("[drive] scoped enumeration: the page token for %q expired; re-listing that folder from the start",
			t.path)
		t.pageToken = ""
		return d.listChildrenPage(ctx, t)
	}
	return res, err
}

// listChildrenPage is one request: this folder's direct children. Drive has no
// recursive form of this query, which is the whole reason the descent exists.
func (d *Drive) listChildrenPage(ctx context.Context, t folderTask) (*drive.FileList, error) {
	call := d.svc.Files.List().
		Q(fmt.Sprintf("'%s' in parents and trashed = false", escapeQuery(t.id))).
		Spaces("drive").
		PageSize(enumPageSize).
		Fields(googleapi.Field("nextPageToken,files(" + fileFields + ")"))
	if t.pageToken != "" {
		call = call.PageToken(t.pageToken)
	}
	return call.Context(ctx).Do()
}

// scopedResolveLocked places one folder's page of children. Every path is known
// immediately — this folder's path plus the child's name — so unlike the flat
// sweep there is nothing to park and nothing left over to drop at the end.
func (d *Drive) scopedResolveLocked(sw *sweepState, t folderTask, res *drive.FileList, page *sweepPage) {
	sw.requests++
	sw.objects += len(res.Files)
	sw.emitted += len(res.Files)

	for _, f := range res.Files {
		p := path.Join(t.path, f.Name)
		d.rememberIDLocked(p, f.Id)
		page.files = append(page.files, toRemoteFile(p, f))
		page.ids = append(page.ids, f.Id)
		if f.MimeType == folderMIME {
			sw.frontier = append(sw.frontier, folderTask{id: f.Id, path: p})
		}
	}
	if res.NextPageToken != "" {
		// More children than one page holds. The folder goes back on the frontier
		// rather than being drained here, so one enormous directory cannot make a
		// single Enumerate call unbounded — MC-52 watches exactly that.
		sw.frontier = append(sw.frontier, folderTask{id: t.id, path: t.path, pageToken: res.NextPageToken})
	}
}
