// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package syncengine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/syshlted/drivel/internal/fsevent"
	"github.com/syshlted/drivel/internal/state"
	"github.com/syshlted/drivel/provider"
)

// Initial enumeration & reconcile (DESIGN.md §9, M7b).
//
// The pull loop starts from a "now" cursor, so everything that existed on the
// remote before the first mount is invisible to it: nothing enumerates the tree,
// and the backing dir only ever learns about objects that change while we are
// running. A sweep (provider.Enumerator) is what makes that tree present.
//
// Two rules carry the whole thing, and both are about ordering or evidence:
//
//   - **Snapshot, then tail.** The change-feed start token is taken BEFORE the
//     sweep and handed to the pull loop only after it completes. The overlap
//     replays some changes, which is harmless — they are idempotent and §4 echo
//     suppression drops them. The other order loses every change made while the
//     sweep was running, permanently and silently.
//   - **A delete is inferred only from a baseline, never from absence alone.**
//     Without a last-known record, "created remotely" and "deleted locally" are
//     the same observation. The baseline is the §4 echo store: a path with an echo
//     is one we have synced, so its disappearance means something. A path without
//     one is simply new, whichever side it is on — which is why the first-ever run
//     performs no deletions at all.
//
// Everything ambiguous keeps data: unsure means materialise, not delete.

// Pusher applies one local-origin event through the outbound path. It is
// satisfied by *Engine, and the reconciler goes through it rather than calling
// the store directly so that a push it triggers gets exactly what a push from the
// mount gets — the M5 placeholder guard, the M6 gates, echo recording, and the
// retry policy.
type Pusher interface {
	Push(ctx context.Context, ev fsevent.Event)
}

// ReconcileOptions configures the sweep. The zero value is inert: without an
// Enumerator behind the store, and without state, nothing here runs and the
// downloader behaves exactly as it did in M3–M7.
type ReconcileOptions struct {
	// Push applies the local-origin half of the reconcile (uploading a file that
	// only exists locally, removing a remote object whose local copy is gone).
	// Nil disables both, leaving the sweep read-only from the remote's point of view.
	Push Pusher

	// Fetch downloads remote objects that have no local counterpart in EAGER mode.
	// It is off by default because there it means pulling down the entire remote
	// tree, which must be an explicit request rather than a side effect of
	// mounting. In lazy mode it is irrelevant: materialising is a placeholder,
	// costs nothing, and always happens.
	Fetch bool

	// Force runs a sweep even when a baseline already exists (drivel mount -resync).
	// Without it a sweep runs on the first run, when resuming an interrupted one,
	// and when the change cursor has expired.
	Force bool

	// Interval re-runs the sweep this often; 0 disables it.
	//
	// The change feed only reports what happens while we are watching it, so
	// everything that happened while drivel was NOT running — a file deleted
	// offline, a push that exhausted its retries — stays unreconciled until
	// something enumerates the tree again. Without an interval that is a first run,
	// a resume, a dead cursor or -resync, which for a long-lived mount can be
	// never. The baselines for those paths stay in the state DB too, describing
	// content that exists on neither side; the sweep is the only pass that can
	// establish that and drop them.
	//
	// The schedule is measured from the last COMPLETED sweep, persisted in the
	// state store, not from process start — see state.SweepDone.
	Interval time.Duration

	// MaxDeletes caps how many deletions ONE sweep may infer, across both
	// directions; 0 means unlimited. Exceeding it abandons the delete pass
	// entirely rather than trimming it, because the shapes that produce a huge
	// count are the accidents — a state DB reused against a different -drive-root,
	// a fresh empty -data dir, a mount that came up pointing somewhere else — and
	// in those the whole inference is wrong, not just its tail.
	MaxDeletes int
}

// DefaultMaxDeletes is the out-of-the-box cap on reconcile-inferred deletions.
// Routine offline activity produces a handful; hundreds means the premise is
// broken. Deletions applied to the remote go through provider.Store.Remove, and
// what that costs is the provider's to decide: Drive trashes by default, so a
// pass this cap fails to catch is recoverable for 30 days, and permanent when the
// operator asked for permanent. Neither weakens the case for the cap — the shapes
// that produce a huge count are broken premises, and a thousand files in someone's
// trash is still a thousand files they did not mean to delete.
const DefaultMaxDeletes = 100

// DefaultSweepInterval is how often a running mount re-enumerates the remote.
//
// Daily rather than hourly because a sweep is the expensive operation here — one
// files.list page per thousand objects, then a local stat and two state reads per
// file — and what it recovers is by definition offline activity, which is not
// urgent. Everything that happens while the mount is up already arrives through
// the change feed within seconds.
const DefaultSweepInterval = 24 * time.Hour

// Reconcile enables the M7b sweep on this downloader and returns it for chaining.
// It is a no-op unless the store also implements provider.Enumerator.
func (d *Downloader) Reconcile(opts ReconcileOptions) *Downloader {
	if e, ok := provider.AsEnumerator(d.store); ok {
		d.enum = e
	}
	d.rec = opts
	return d
}

// canSweep reports whether a sweep is possible at all: something to enumerate,
// and somewhere to keep the token, the sweep cursor and the seen-set.
func (d *Downloader) canSweep() bool { return d.enum != nil && d.state != nil }

// startFeed returns the cursor the pull loop should poll from, running the
// initial sweep first when one is due.
//
// The three ways a sweep becomes due are all "we cannot trust the feed alone":
// an interrupted sweep to finish, a first run with no cursor at all (the remote
// tree has never been looked at), or an explicit -resync. Cursor expiry is the
// fourth and arrives later, through recoverCursor.
func (d *Downloader) startFeed(ctx context.Context) (string, error) {
	if !d.canSweep() {
		return d.resumeCursor(ctx)
	}

	if sw, ok, err := d.state.Sweep(); err != nil {
		d.logf("[sweep] reading sweep state: %v", err)
	} else if ok {
		d.logf("[sweep] resuming the enumeration interrupted at %s", sw.Started.Format(time.RFC3339))
		return d.runSweep(ctx, sw)
	}

	cursor, ok, err := d.state.Cursor()
	if err != nil {
		return "", err
	}
	switch {
	case !ok && !d.hasFeed():
		// Every start, by design: with no feed this sweep is the only thing that
		// will ever notice what changed remotely while the mount was down.
		d.logf("[sweep] no change feed: enumerating the remote tree")
	case !ok:
		d.logf("[sweep] no change cursor yet: enumerating the remote tree before tailing it")
	case d.rec.Force:
		d.logf("[sweep] -resync: re-enumerating the remote tree")
	case d.sweepOverdue():
		d.logf("[sweep] no enumeration within -sweep-interval %s: re-enumerating the remote tree", d.rec.Interval)
	default:
		return cursor, nil
	}
	return d.beginSweep(ctx)
}

// sweepOverdue reports whether the periodic schedule is due, from the last sweep
// that actually completed.
//
// No stamp at all counts as overdue. That is a state DB written before the stamp
// existed, or one whose every sweep was interrupted; either way nothing has ever
// finished reconciling this tree, which is the case the interval exists for. The
// cost is one sweep on the first mount after enabling it.
func (d *Downloader) sweepOverdue() bool {
	if d.rec.Interval <= 0 || !d.canSweep() {
		return false
	}
	last, ok, err := d.state.SweepDone()
	if err != nil {
		d.logf("[sweep] reading the last sweep time: %v", err)
		return false // unreadable state is not evidence; leave the schedule alone
	}
	return !ok || time.Since(last) >= d.rec.Interval
}

// sweepDeadline is when the next periodic sweep falls due. The zero time means
// never, which is what a disabled interval and a provider that cannot enumerate
// both amount to.
func (d *Downloader) sweepDeadline() time.Time {
	if d.rec.Interval <= 0 || !d.canSweep() {
		return time.Time{}
	}
	last, ok, err := d.state.SweepDone()
	if err != nil || !ok {
		// startFeed has already run whatever sweep was due, so a missing stamp here
		// means only that recording it failed. Counting from now costs at most one
		// late sweep; counting from zero would sweep on every poll.
		return time.Now().Add(d.rec.Interval)
	}
	return last.Add(d.rec.Interval)
}

// dueSweep runs the periodic sweep if the deadline has passed, returning the
// cursor the feed continues from. It exists for the long-lived mount that never
// restarts, which is exactly the process startFeed's checks cannot help.
//
// It goes through beginSweep, so the start token is taken before the listing just
// as on a first run. The cursor it replaces is older than the one in hand, which
// replays changes the feed already delivered — idempotent, and dropped by §4.
func (d *Downloader) dueSweep(ctx context.Context) (string, bool) {
	if d.nextSweep.IsZero() || time.Now().Before(d.nextSweep) {
		return "", false
	}
	// Reschedule before running, not after. A sweep that fails must not retry in a
	// tight poll loop, and one that takes an hour must not be due again on return.
	d.nextSweep = time.Now().Add(d.rec.Interval)
	d.logf("[sweep] -sweep-interval %s elapsed: re-enumerating the remote tree", d.rec.Interval)
	fresh, err := d.beginSweep(ctx)
	if err != nil {
		if ctx.Err() == nil {
			d.logf("[sweep] periodic re-enumeration failed: %v", err)
		}
		return "", false
	}
	return fresh, true
}

// beginSweep takes the start token FIRST, records it with the sweep, and only
// then starts listing. The token is what the pull loop resumes from once the
// sweep finishes, so it has to predate everything the sweep observes.
func (d *Downloader) beginSweep(ctx context.Context) (string, error) {
	var token string
	if d.hasFeed() {
		var err error
		if token, err = d.src.StartCursor(ctx); err != nil {
			return "", err
		}
	}
	// With no feed there is no token to take and nothing to tail afterwards, so
	// the sweep records an empty one. Everything downstream already treats "" as
	// "no cursor"; what it must not do is *persist* it — see runSweep.
	sw := state.Sweep{
		Gen:     fmt.Sprintf("%d", time.Now().UnixNano()),
		Token:   token,
		Started: time.Now(),
	}
	if err := d.state.SetSweep(sw); err != nil {
		return "", err
	}
	return d.runSweep(ctx, sw)
}

// recoverCursor handles an expired change cursor: the feed can no longer tell us
// what we missed, so the only honest recovery is to look at the whole tree again.
// A fresh token is taken before that sweep, exactly as on a first run.
func (d *Downloader) recoverCursor(ctx context.Context) (string, error) {
	if !d.canSweep() {
		// Nothing to enumerate with. Restarting from "now" at least gets inbound
		// sync moving again; what happened during the gap is lost either way, and a
		// dead cursor retried forever is strictly worse.
		d.logf("[pull] cursor expired and this provider cannot enumerate; restarting the feed from now (changes during the gap are lost)")
		token, err := d.src.StartCursor(ctx)
		if err != nil {
			return "", err
		}
		return token, d.state.SetCursor(token)
	}
	d.logf("[pull] cursor expired: re-enumerating the remote tree to resync")
	return d.beginSweep(ctx)
}

// runSweep drives one sweep to completion: pages in, reconcile per page, then the
// passes that need the whole picture. It returns the cursor the pull loop should
// use.
//
// Pages are applied as they arrive rather than accumulated into one transaction,
// so an interruption leaves a partially populated but consistent tree — and the
// sweep cursor is persisted after each one, so resuming costs at most a page.
func (d *Downloader) runSweep(ctx context.Context, sw state.Sweep) (string, error) {
	started := time.Now()
	var st sweepStats
	for {
		files, next, err := d.enum.Enumerate(ctx, sw.Cursor)
		if err != nil {
			return "", fmt.Errorf("enumerate: %w", err)
		}
		if err := d.applyPage(ctx, sw, files, &st); err != nil {
			return "", err
		}
		if next != "" && next == sw.Cursor {
			// A provider that hands back the cursor it was given would loop us over
			// one page forever. Treat it as the end of the sweep: the pages already
			// applied stand, and the delete passes below are the only thing that
			// needs completeness — so say so rather than pretending it completed.
			d.logf("[sweep] enumeration stalled on cursor %q; ending the sweep here", next)
			next = ""
		}
		sw.Cursor = next
		if err := d.state.SetSweep(sw); err != nil {
			return "", err
		}
		if next == "" {
			break
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
	}

	// Both passes below need the complete picture, so neither can run per page:
	// "not seen anywhere in the sweep" is only knowable once the sweep has ended.
	// They also run to completion before the pull loop starts polling, which on a
	// large local-only tree can mean a slow first run. That is latency, not loss:
	// the token was taken before the sweep, so the changes are still waiting when
	// polling begins. Serial and deterministic is worth more here than concurrent
	// and racing the very paths it is reconciling.
	d.pushLocalOnly(ctx, sw, &st)
	if err := d.applyDeletes(ctx, sw, &st); err != nil {
		d.logf("[sweep] delete pass: %v", err)
	}

	// Only with a feed. A provider without one has no cursor, and persisting the
	// empty token would make state.Cursor report ok=true on the next mount — which
	// startFeed reads as "we are already tailing this remote" and would skip the
	// startup sweep, i.e. skip the entire inbound path.
	if d.hasFeed() {
		if err := d.state.SetCursor(sw.Token); err != nil {
			return "", err
		}
	}
	if err := d.state.FinishSweep(time.Now()); err != nil {
		// Only the schedule suffers: the next mount reads no completion stamp and
		// treats a sweep as overdue, which costs one extra sweep, not correctness.
		d.logf("[sweep] recording sweep completion: %v", err)
	}
	d.nextSweep = d.sweepDeadline()
	d.logf("[sweep] reconcile complete in %s: %s", time.Since(started).Round(time.Millisecond), st)
	return sw.Token, nil
}

// sweepStats is the running tally, logged at the end. A cost the user cannot see
// is a cost they will assume is a hang.
type sweepStats struct {
	objects     int
	materialize int
	deferred    int // remote-only, eager mode, no -materialize
	exportOnly  int
	pushed      int
	localDel    int
	remoteDel   int
	kept        int // would have been deleted locally, but the local copy diverged
	orphaned    int // baselines dropped without acting: gone locally, unlisted remotely
	special     int // local non-regular files stepped over: no byte stream to sync (M15)
}

// kindOf names a file type for the one job of telling a user which of their files
// is not being synced. The mount reports the same skip from the other side and has
// its own copy (vfs.specialKind) — see the comment there for why the engine must
// not import the mount backend to share one. Keep the wording identical.
func kindOf(m fs.FileMode) string {
	switch {
	case m&fs.ModeSymlink != 0:
		return "symbolic link"
	case m&fs.ModeNamedPipe != 0:
		return "named pipe"
	case m&fs.ModeSocket != 0:
		return "socket"
	case m&fs.ModeCharDevice != 0:
		return "character device"
	case m&fs.ModeDevice != 0:
		return "block device"
	}
	return "special file"
}

func (s sweepStats) String() string {
	out := fmt.Sprintf("%d remote object(s) listed, %d reconciled locally, %d pushed", s.objects, s.materialize, s.pushed)
	if s.deferred > 0 {
		out += fmt.Sprintf(", %d not fetched (add -materialize to download them)", s.deferred)
	}
	if s.exportOnly > 0 {
		out += fmt.Sprintf(", %d Google-native doc(s) skipped", s.exportOnly)
	}
	if s.localDel > 0 || s.remoteDel > 0 {
		out += fmt.Sprintf(", %d deleted locally, %d deleted remotely", s.localDel, s.remoteDel)
	}
	if s.kept > 0 {
		out += fmt.Sprintf(", %d kept (locally modified after a remote delete)", s.kept)
	}
	if s.orphaned > 0 {
		out += fmt.Sprintf(", %d stale record(s) dropped", s.orphaned)
	}
	if s.special > 0 {
		out += fmt.Sprintf(", %d local special file(s) skipped", s.special)
	}
	return out
}

// applyPage reconciles one page of remote objects.
func (d *Downloader) applyPage(ctx context.Context, sw state.Sweep, files []provider.RemoteFile, st *sweepStats) error {
	if len(files) == 0 {
		return nil
	}
	paths := make([]string, len(files))
	for i := range files {
		paths[i] = files[i].Path
	}
	// Mark before acting, and mark everything — including objects we then decline
	// to materialise. The marks answer "does the remote still have this?", which is
	// a different question from "did we copy it", and conflating them would let the
	// delete pass remove a local file whose remote counterpart we merely skipped.
	if err := d.state.MarkSeen(sw.Gen, paths); err != nil {
		return err
	}
	st.objects += len(files)

	for i := range files {
		f := files[i]
		if err := d.reconcileRemote(ctx, f, st); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			d.logf("[sweep] %s: %v", f.Path, err)
		}
	}
	return nil
}

// reconcileRemote applies one remote object to the local tree.
//
// The remote-present rows of the three-way table are exactly what the pull loop's
// apply already does — echo match ⇒ nothing, identical local bytes ⇒ nothing,
// divergent local ⇒ §6 conflict copy, absent local ⇒ materialise — with the echo
// record serving as the baseline in both. So this adds only what a sweep needs on
// top: the two kinds of object it must NOT materialise.
func (d *Downloader) reconcileRemote(ctx context.Context, f provider.RemoteFile, st *sweepStats) error {
	if f.ExportOnly {
		// A Google-native doc has no byte stream, no size and no checksum: nothing
		// to place a placeholder against and nothing to download. It is still marked
		// seen, so it is not mistaken for remotely deleted; it simply has no local
		// representation. Deliberately no echo record either — recording one would
		// claim we hold content we do not.
		st.exportOnly++
		return nil
	}
	if !d.wouldMaterialize(f) {
		st.deferred++
		return nil
	}
	if err := d.apply(ctx, provider.RemoteChange{Path: f.Path, File: &f}); err != nil {
		return err
	}
	st.materialize++
	return nil
}

// wouldMaterialize reports whether a remote object with no local counterpart
// should be created locally now.
//
// In lazy mode it always should: a placeholder is metadata, so the whole remote
// tree becomes visible for the price of the sweep. In eager mode the same
// operation is a full download of everything, which is a decision the user makes
// (-materialize), not a side effect of mounting. Objects that already exist
// locally are unaffected either way — those are conflicts or no-ops, not
// materialisation, and always get handled.
func (d *Downloader) wouldMaterialize(f provider.RemoteFile) bool {
	if d.mat != nil || d.rec.Fetch {
		return true
	}
	_, err := os.Lstat(filepath.Join(d.dataDir, filepath.FromSlash(f.Path)))
	return err == nil
}

// pushLocalOnly walks the backing tree and pushes files that exist only there.
//
// This is the "new locally" row, and it needs a local walk because nothing else
// knows about a file the mount never saw created — an edit made while drivel was
// down, or in in-place mode, a file dropped into the directory between runs. The
// walk is local I/O and its only action is a push, so unlike the delete passes it
// needs no cap and no flag: the worst case is uploading a file that belongs in a
// directory the user is syncing anyway.
func (d *Downloader) pushLocalOnly(ctx context.Context, sw state.Sweep, st *sweepStats) {
	if d.rec.Push == nil {
		return
	}
	err := filepath.WalkDir(d.dataDir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // unreadable entry: skip it, never abandon the walk
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rel, relErr := filepath.Rel(d.dataDir, p)
		if relErr != nil || rel == "." {
			// A path we cannot express relative to the root is not ours to push.
			return nil //nolint:nilerr // see above
		}
		rel = filepath.ToSlash(rel)
		if skipLocal(filepath.Base(p)) {
			if e.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if e.IsDir() {
			return nil // Put creates ancestors; an empty dir is not worth a request
		}
		if !e.Type().IsRegular() {
			// A symlink, fifo, socket or device node has no byte stream to upload, so
			// it stays local — the behaviour this walk always had. What M15 adds is
			// saying so: silence is what turns "unsupported" into a support question,
			// and this is one of the two sites that make the decision (the other is
			// the mount, in vfs/special.go, when it declines to emit).
			//
			// One line per path per sweep, not one ever: deduplicating across sweeps
			// would take persistent state to record that a user has been told, and a
			// tree full of sockets is a fact worth repeating at the interval the
			// sweep already logs everything else at.
			d.logf("[sweep] skip    %s (%s: no remote representation, stays local)", rel, kindOf(e.Type()))
			st.special++
			return nil
		}
		// A placeholder is remote-born by definition — never a local-only file —
		// and pushing one is the M5 catastrophe. The uploader would refuse it
		// anyway; not asking is cheaper and clearer.
		if d.mat != nil && d.mat.IsPlaceholder(rel) {
			return nil
		}
		if _, ok, err := d.state.GetEcho(rel); err != nil || ok {
			return nil //nolint:nilerr // has a baseline (or the store is unreadable, which means the same here): handled by the rows that need one
		}
		if seen, err := d.state.SeenPath(sw.Gen, rel); err != nil || seen {
			return nil //nolint:nilerr // seen remotely (or the store is unreadable, which means the same here): apply already reconciled it
		}
		d.logf("[sweep] push    %s (local only)", rel)
		d.rec.Push.Push(ctx, fsevent.Event{Op: fsevent.OpWrite, Path: rel})
		st.pushed++
		return nil
	})
	if err != nil && ctx.Err() == nil {
		d.logf("[sweep] walking the backing tree: %v", err)
	}
}

// applyDeletes runs the two rows of the table that remove something. It is the
// only part of a reconcile that can destroy data, and it runs last, only after a
// sweep that completed, only on paths with a baseline older than the sweep, and
// only within MaxDeletes.
func (d *Downloader) applyDeletes(ctx context.Context, sw state.Sweep, st *sweepStats) error {
	// The cap is applied DURING the walk, not after it. The shape that most needs
	// refusing — a state DB whose paths are all absent from this remote — is also
	// the biggest, so materialising every candidate before deciding would spend the
	// memory on exactly the pass about to be thrown away. Past the limit we stop
	// retaining and only keep counting: the count costs nothing inside a scan we
	// are already performing, and it is what makes the refusal actionable, since
	// "4231 deletions" reads very differently to an operator than "more than 100".
	var cands []candidate
	total := 0
	err := d.state.EachUnseenEcho(sw.Gen, func(p string, e state.Echo) error {
		// The baseline has to be older than the sweep. An echo written *during* the
		// sweep describes a file that appeared after its page was listed — a local
		// create pushed while we swept — and it is absent from the seen-set for that
		// reason alone. Deleting it would destroy a file the user just made.
		if !e.At.Before(sw.Started) {
			return nil
		}
		total++
		if d.rec.MaxDeletes > 0 && total > d.rec.MaxDeletes {
			cands = nil
			return nil
		}
		cands = append(cands, candidate{path: p, echo: e})
		return nil
	})
	if err != nil {
		return err
	}
	if total == 0 {
		return nil
	}
	if d.rec.MaxDeletes > 0 && total > d.rec.MaxDeletes {
		d.logf("[sweep] REFUSING to delete: the sweep says %d previously-synced path(s) are gone from the remote, over the -max-deletes limit of %d. "+
			"That many at once usually means the state DB, the backing dir or -drive-root do not match each other, rather than %d real deletions. "+
			"Nothing was deleted. If the deletions are genuine, re-run with -resync AND a higher -max-deletes (or 0 for no limit): "+
			"this sweep still counts as complete, so raising the limit alone changes nothing until the next -sweep-interval falls due.",
			total, d.rec.MaxDeletes, total)
		return nil
	}

	// Deepest first, so a directory is only considered once its own children have
	// been dealt with and it can be removed by an empty-dir rmdir. The name
	// tie-break is only there to keep the log order stable between runs.
	sort.Slice(cands, func(i, j int) bool {
		if len(cands[i].path) != len(cands[j].path) {
			return len(cands[i].path) > len(cands[j].path)
		}
		return cands[i].path < cands[j].path
	})
	// The records for paths this pass actually removes are dropped in one commit
	// rather than two per path. Deferring them is safe because nothing re-reads a
	// baseline within the pass, and a crash before the flush is self-correcting:
	// the path is gone on both sides, so the next sweep sees it as already-deleted
	// and the leftover record costs one no-op push.
	forget := map[string]struct{}{}
	defer func() {
		paths := make([]string, 0, len(forget))
		for p := range forget {
			paths = append(paths, p)
		}
		if err := d.state.ForgetMany(paths); err != nil {
			d.logf("[sweep] clearing %d record(s): %v", len(paths), err)
		}
	}()
	for _, c := range cands {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		d.deleteGone(ctx, c.path, c.echo, st, forget)
	}
	return nil
}

// candidate is one path with a baseline that the sweep did not observe remotely.
// The echo travels with the path because deleteGone needs it to decide whether
// the local copy is still the one the baseline describes.
type candidate struct {
	path string
	echo state.Echo
}

// deleteGone handles one path that has a baseline but is no longer on the remote.
//
//	local absent  → the user deleted it here while we were down  ⇒ delete remotely
//	local present → the remote lost it while we were down        ⇒ delete locally,
//	                but only if the local copy is still the one the baseline
//	                describes. A local copy that has changed since is the only
//	                remaining version of that work, and no inference is worth
//	                losing it: keep it and push it back instead.
func (d *Downloader) deleteGone(ctx context.Context, rel string, e state.Echo, st *sweepStats, forget map[string]struct{}) {
	dst := filepath.Join(d.dataDir, filepath.FromSlash(rel))
	info, err := os.Lstat(dst)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if d.rec.Push == nil {
			// Nothing wired to act on the remote, so whatever is left there stays.
			// The baseline still has to go: it describes content that is absent
			// locally and unlisted remotely, so keeping it makes this path a delete
			// candidate again on every future sweep — forever, since no pass will
			// ever resolve it. Dropping it is the safe direction: the path becomes
			// new to the next sweep, and a path without a baseline is never deleted.
			forget[rel] = struct{}{}
			st.orphaned++
			return
		}
		d.logf("[sweep] delete  %s remotely (gone locally)", rel)
		d.rec.Push.Push(ctx, fsevent.Event{Op: fsevent.OpUnlink, Path: rel})
		st.remoteDel++
		return
	case err != nil:
		d.logf("[sweep] %s: %v (keeping both sides)", rel, err)
		return
	}

	if info.IsDir() {
		// Never RemoveAll: the children are candidates in their own right, and a
		// directory holding anything we did not account for must survive. An
		// ENOTEMPTY here is the correct outcome, not an error.
		if err := os.Remove(dst); err != nil {
			d.logf("[sweep] keep    %s (remote directory gone, but the local one is not empty)", rel)
			return
		}
		forget[rel] = struct{}{}
		d.logf("[sweep] rmdir   %s (gone remotely)", rel)
		st.localDel++
		return
	}

	unchanged, why := d.matchesBaseline(rel, dst, e)
	if !unchanged {
		// The local bytes are now the only copy of that work. Keep them, drop the
		// baseline that no longer describes anything, and put them back on the
		// remote if there is anywhere to put them.
		d.logf("[sweep] keep    %s (gone remotely, but %s)", rel, why)
		// Dropped inline, not batched: the push below writes a fresh echo for this
		// same path, and a deferred delete would land on top of it and erase it.
		if err := d.state.DeleteEcho(rel); err != nil {
			d.logf("[sweep] clearing the baseline for %s: %v", rel, err)
		}
		if d.rec.Push != nil {
			d.rec.Push.Push(ctx, fsevent.Event{Op: fsevent.OpWrite, Path: rel})
		}
		st.kept++
		return
	}
	if err := os.Remove(dst); err != nil {
		d.logf("[sweep] removing %s: %v", rel, err)
		return
	}
	forget[rel] = struct{}{}
	d.logf("[sweep] delete  %s locally (gone remotely)", rel)
	st.localDel++
}

// matchesBaseline reports whether the local file still holds exactly the content
// the baseline recorded — the only state in which deleting it loses nothing,
// because that content also existed remotely.
//
// It fails toward keeping the file: a placeholder is content-free and therefore
// always safe to drop, but a baseline with no checksum to compare against, or a
// file we cannot read, is not something to delete on a guess.
func (d *Downloader) matchesBaseline(rel, dst string, e state.Echo) (bool, string) {
	if d.mat != nil && d.mat.IsPlaceholder(rel) {
		return true, ""
	}
	if e.Hash == "" {
		return matchesFingerprint(dst, e)
	}
	local, err := fileMD5(dst)
	if err != nil {
		return false, fmt.Sprintf("it could not be read (%v)", err)
	}
	if local != e.Hash {
		return false, "it was modified locally since"
	}
	return true, ""
}

// matchesFingerprint is the baseline check for a provider that publishes no
// content checksum — every filesystem backend in the M17–M21 group, where
// RemoteFile.Hash is always empty.
//
// It compares the backing file against the size and mtime recorded when the
// baseline was written (state.Echo). Without it a hashless provider answered "the
// baseline has no checksum to compare against" for every file, forever, so a
// deletion made on the remote was never applied locally — the file was kept and
// pushed straight back up on the same sweep, permanently undoing the deletion.
// Verified against a live OpenSSH server, where it is exactly what happened.
//
// Size and mtime are weaker than a digest and strong enough here, for a reason
// that does not hold on the wire: this is the *local* file, whose mtime the
// kernel keeps to nanoseconds, so any write moves it. The remote's own
// second-resolution mtime never enters into it.
//
// No fingerprint recorded means no answer, not a match — an echo written before
// the field existed, a directory, or a stat that failed. Guessing "unmodified"
// there would delete a file somebody edited.
func matchesFingerprint(dst string, e state.Echo) (bool, string) {
	if e.LocalMTime.IsZero() {
		return false, "the baseline has no checksum or fingerprint to compare against"
	}
	fi, err := os.Stat(dst)
	if err != nil {
		return false, fmt.Sprintf("it could not be read (%v)", err)
	}
	if fi.Size() != e.LocalSize || !fi.ModTime().Equal(e.LocalMTime) {
		return false, "it was modified locally since"
	}
	return true, ""
}

// conflictRe matches the names conflictName generates. Conflict copies are
// local-only by §6 policy, so the local walk must not discover them and "helpfully"
// upload them — which would publish the losing side of every conflict drivel has
// ever resolved.
var conflictRe = regexp.MustCompile(` \(conflict \d{4}-\d{2}-\d{2} \d{2}-\d{2}-\d{2}\)(\.[^./]*)?$`)

// skipLocal reports names the local walk must not treat as user content: drivel's
// own temporary files and probes, and §6 conflict copies.
func skipLocal(base string) bool {
	return strings.HasPrefix(base, ".drivel-") || conflictRe.MatchString(base)
}
