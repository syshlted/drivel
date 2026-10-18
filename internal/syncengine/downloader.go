// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package syncengine

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // G501: Drive addresses content by MD5; see fileMD5
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/syshlted/drivel/internal/fsevent"
	"github.com/syshlted/drivel/internal/state"
	"github.com/syshlted/drivel/provider"
)

// Cadence bounds the adaptive poll interval (DESIGN.md §3.4): poll at Fast while
// the feed is active, backing off geometrically toward Slow once it goes quiet.
type Cadence struct {
	Fast time.Duration
	Slow time.Duration
}

// DefaultCadence is the ~2s-active / ~30s-idle default.
var DefaultCadence = Cadence{Fast: 2 * time.Second, Slow: 30 * time.Second}

// tombstoneWindow is how long after deleting an object we still recognise the
// provider's report of it as a stale view rather than as news (see
// state.Tombstone and suppressDeleted).
//
// It has to exceed the lag between a deletion and the provider's feed agreeing
// that it happened. On Drive that lag is the trash flag reaching a trashed
// folder's children, which was single-digit seconds in the run that found this,
// and which Drive does not specify — so the window is an order of magnitude
// above the only number anyone has measured.
//
// What a window that is too long costs is bounded and heals: an object another
// client re-creates at this exact path, with byte-identical content, inside the
// window is not applied until the next sweep. What one that is too short costs
// does not heal, which is why the asymmetry points this way. It is not a Config
// knob for the same reason repairDelay is not: it describes a provider's
// behaviour, not a preference.
const tombstoneWindow = 5 * time.Minute

// Downloader runs the inbound pull loop: it polls a provider.ChangeSource and
// applies remote deltas to the backing directory, using the echo-suppression
// store so it never re-applies the uploader's own writes (DESIGN.md §3–§4).
//
// A provider with no change feed is a supported shape, not a degraded one: the
// M7b sweep then IS the inbound path rather than a safety net beneath one, and
// -sweep-interval stops being a long-period backstop and becomes the poll
// interval (DESIGN.md §9, the M17–M21 preamble). Run branches on that at the top
// and everything below the branch is shared.
//
// It writes directly into the backing dir (never through the mountpoint), so its
// writes don't re-enter the FUSE layer and never generate local events — which is
// why §4.3 ("suppress downloader-originated local events") needs no extra
// plumbing here. When a remote edit collides with a divergent local edit it
// applies the §6 conflict policy (last-writer-wins by mtime, loser kept as a
// conflict copy); see resolveConflict.
type Downloader struct {
	// src is the incremental change feed, or nil for a provider that has none —
	// every filesystem backend in the M17–M21 group. hasFeed is the only thing
	// that may read it for presence; the rest of the file assumes it is there.
	src     provider.ChangeSource
	store   provider.Store
	dataDir string
	state   *state.Store
	cad     Cadence
	lg      *log.Logger  // nil => the log package's default
	mat     Materializer // nil => eager mode: apply content immediately

	// Initial enumeration & reconcile (M7b). enum is nil unless the store can
	// enumerate, in which case the downloader owns the sweep as well as the feed —
	// one owner for the cursor means one place where "snapshot, then tail" can be
	// got wrong. See reconcile.go.
	enum provider.Enumerator
	rec  ReconcileOptions

	// nextSweep is when the periodic sweep next falls due. It is read and written
	// only by the Run goroutine (and by runSweep, which that goroutine drives), so
	// it needs no lock. Zero means never.
	nextSweep time.Time

	// repairDue is when a sweep owed to an abandoned push falls due, and
	// repairFails counts the consecutive repair sweeps that left the debt
	// standing. Same goroutine, same absence of a lock. See armRepairSweep.
	//
	// repairBase is repairDelay, as a field only so that a test can ask for the
	// behaviour without waiting five minutes for it. It is not a Config knob: see
	// repairDelay for why the delay is not the user's to set.
	repairDue   time.Time
	repairFails int
	repairBase  time.Duration

	// refetchDue is when the paths whose inbound apply failed are next retried,
	// and refetchFails counts the consecutive retry passes that left one
	// standing. Same goroutine, same absence of a lock; refetchBase is
	// refetchDelay, a field only for tests. See refetch.go.
	refetchDue   time.Time
	refetchFails int
	refetchBase  time.Duration

	// openedAt is when this mount's pull loop was constructed, which is before
	// the filesystem is served and therefore before any write can reach the
	// engine (app.Mount.Open builds both; Run mounts afterwards).
	//
	// It is the boundary the sweep's lost-push row uses: a local file modified
	// since this instant is one the running engine is already responsible for,
	// and one modified before it is not. See repushDiverged.
	openedAt time.Time

	// conflicts is the mount's conflict policy, shared with its engine; never nil
	// (NewDownloader installs the default, and Conflicts replaces it).
	conflicts *Conflicts

	// M14. mon is written to and never read (see Monitor); hold is what a pause
	// makes this loop wait at, between polls and between sweep pages.
	mon  *Monitor
	hold Holder

	// sending counts conflict copies on their way to the remote, at most
	// conflictSenders at once (sendConflictCopy).
	sending sync.WaitGroup
	senders chan struct{}
}

// Holder is satisfied by *Engine: WaitUnpaused blocks while the mount is paused.
type Holder interface {
	WaitUnpaused(ctx context.Context) bool
}

// Monitor sets where this pull loop reports, and returns d for chaining.
func (d *Downloader) Monitor(m *Monitor) *Downloader {
	d.mon = m
	return d
}

// PausedBy makes this pull loop, and the sweep it owns, stop while h is paused.
func (d *Downloader) PausedBy(h Holder) *Downloader {
	d.hold = h
	return d
}

// waitUnpaused blocks while the mount is paused, reporting false if ctx ended.
func (d *Downloader) waitUnpaused(ctx context.Context) bool {
	if d.hold == nil {
		return ctx.Err() == nil
	}
	return d.hold.WaitUnpaused(ctx)
}

// Materializer is the OPTIONAL lazy-hydration hook (M5), satisfied by
// *hydrate.Hydrator. With one installed the pull loop writes placeholders — right
// name, size and mtime, no bytes — instead of downloading content the user has not
// asked for. Nil restores the eager M3/M4 behaviour.
type Materializer interface {
	// CreatePlaceholder materialises rel with f's metadata and no content.
	CreatePlaceholder(rel string, f provider.RemoteFile) error
	// IsPlaceholder reports whether rel currently lacks its content.
	IsPlaceholder(rel string) bool
}

// Lazy enables M5 lazy hydration on the pull loop and returns d for chaining:
//
//	NewDownloader(src, store, dir, st, cad).Lazy(hydrator)
func (d *Downloader) Lazy(m Materializer) *Downloader {
	d.mat = m
	return d
}

// Logger sets where this pull loop writes and returns d for chaining. Per
// downloader for the same reason as the engine: with several mounts running,
// an unattributed line does not say whose remote changed (M8).
func (d *Downloader) Logger(lg *log.Logger) *Downloader {
	d.lg = lg
	return d
}

// logf writes one line for this pull loop.
func (d *Downloader) logf(format string, args ...any) {
	if d.lg == nil {
		log.Printf(format, args...)
		return
	}
	d.lg.Printf(format, args...)
}

// NewDownloader constructs the pull loop. cad zero-values fall back to
// DefaultCadence.
//
// src may be nil, for a store that offers no incremental feed. Such a downloader
// does no polling at all and exists solely to run the sweep, which is then the
// only inbound path — so pairing a nil src with a store that cannot enumerate
// builds a downloader with nothing to do, and Run returns immediately.
func NewDownloader(src provider.ChangeSource, store provider.Store, dataDir string, st *state.Store, cad Cadence) *Downloader {
	if cad.Fast <= 0 {
		cad.Fast = DefaultCadence.Fast
	}
	if cad.Slow < cad.Fast {
		cad.Slow = DefaultCadence.Slow
	}
	return &Downloader{
		src: src, store: store, dataDir: dataDir, state: st, cad: cad,
		repairBase: repairDelay, refetchBase: refetchDelay, openedAt: time.Now(),
		conflicts: &Conflicts{}, senders: make(chan struct{}, conflictSenders),
	}
}

// Conflicts sets the mount's conflict policy — the same value its engine is
// given, so the pull and the push agree on where a loser goes and on which
// versions have already been kept. Nil keeps the default.
func (d *Downloader) Conflicts(c *Conflicts) *Downloader {
	if c != nil {
		d.conflicts = c
	}
	return d
}

// conflictEnv is what the shared conflict policy needs from the pull loop. The
// remote half of keeping a loser runs on its own goroutine (sendConflictCopy),
// so a large copy's upload does not hold up the next inbound change.
func (d *Downloader) conflictEnv(ctx context.Context) conflictEnv {
	return conflictEnv{
		store: d.store, state: d.state, dataDir: d.dataDir, logf: d.logf, observe: d.mon.observe,
		later: func(copyRel string, job func(context.Context) string) { d.sendConflictCopy(ctx, copyRel, job) },
	}
}

// conflictSenders bounds how many conflict copies one pull loop sends at once.
// A page of a thousand conflicts would otherwise be a thousand uploads at once.
const conflictSenders = 2

// sendConflictCopy runs job — uploading, or uploading and trashing, a copy
// already safe in the local conflicts folder — once the mount is not paused,
// since a pause promises to stop every transfer and this is one. Run waits for
// it before returning, so it never outlives the state store it records in. Cut
// short by shutdown, the copy stays in the local folder, which preserves it;
// with the folder on both sides the walk sends it later.
func (d *Downloader) sendConflictCopy(ctx context.Context, copyRel string, job func(context.Context) string) {
	d.sending.Add(1)
	go func() {
		defer d.sending.Done()
		if !d.waitUnpaused(ctx) {
			d.logf("[pull] conflict copy %s: not sent before shutdown (kept in the local folder)", copyRel)
			return
		}
		select {
		case d.senders <- struct{}{}:
		case <-ctx.Done():
			d.logf("[pull] conflict copy %s: not sent before shutdown (kept in the local folder)", copyRel)
			return
		}
		defer func() { <-d.senders }()
		d.logf("[pull] conflict copy %s: %s", copyRel, job(ctx))
	}()
}

// Run polls until ctx is cancelled. It resumes from the persisted cursor, or
// obtains a fresh start token on first run so it only ever sees changes from
// "now" forward.
func (d *Downloader) Run(ctx context.Context) {
	defer d.sending.Wait()
	if !d.hasFeed() {
		d.runWithoutFeed(ctx)
		return
	}
	cursor, ok := d.start(ctx)
	if !ok {
		return
	}
	d.nextSweep = d.sweepDeadline()
	d.resumeRefetches()

	wait := d.cad.Fast
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if !d.waitUnpaused(ctx) {
			return
		}

		// Before polling, not after: a sweep replaces the cursor, so applying a page
		// fetched with the old one and then sweeping would advance past changes the
		// sweep's older token was about to replay.
		if fresh, swept := d.dueSweep(ctx); swept {
			cursor, wait = fresh, d.cad.Fast
		}
		d.dueRefetch(ctx)

		changes, next, err := d.src.Changes(ctx, cursor)
		d.mon.observe(err)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			d.mon.count(cPollErrors, 1)
			// An expired cursor is not a transient failure: the changes it covered
			// no longer exist to be fetched, so retrying it never recovers. Before
			// M7b there was no case for it and inbound sync simply stopped, forever
			// and quietly, at the slow cadence. The recovery is a resync (M7b).
			if errors.Is(err, provider.ErrCursorExpired) {
				fresh, rErr := d.recoverCursor(ctx)
				if rErr != nil {
					if ctx.Err() != nil {
						return
					}
					d.logf("[pull] resync after cursor expiry failed: %v", rErr)
					wait = d.cad.Slow
					continue
				}
				cursor, wait = fresh, d.cad.Fast
				continue
			}
			d.logf("[pull] changes: %v", err)
			wait = d.cad.Slow
			continue
		}

		// Records for paths removed in this page are dropped in ONE commit at the
		// end of it rather than two per path, which is what a mass delete arriving
		// over the feed actually costs. The page is the right boundary because the
		// cursor below defines it: flush first, so we never advance past a page
		// whose records we failed to drop.
		forget := map[string]struct{}{}
		for _, ch := range changes {
			if err := d.applyTo(ctx, ch, forget); err != nil {
				if ctx.Err() != nil {
					return
				}
				d.logf("[pull] apply %s: %v", ch.Path, err)
				// The cursor still advances past this page, so the feed will
				// not offer the object again: record the path, or only a sweep
				// would ever notice (MC-46).
				d.oweRefetch(ch)
			}
		}
		if err := d.flushForget(forget); err != nil {
			d.logf("[pull] clearing %d removed record(s): %v", len(forget), err)
			continue // leave the cursor where it is; the page replays idempotently
		}

		advanced := next != "" && next != cursor
		if advanced {
			if err := d.state.SetCursor(next); err != nil {
				d.logf("[pull] persist cursor: %v", err)
			}
			cursor = next
		}
		d.mon.count(cPolls, 1)
		d.mon.polled(advanced)

		// Adaptive cadence: stay fast while the feed is active, else back off
		// geometrically toward Slow.
		if len(changes) > 0 {
			wait = d.cad.Fast
		} else if wait *= 2; wait > d.cad.Slow {
			wait = d.cad.Slow
		}
	}
}

// hasFeed reports whether this provider offers an incremental change feed. It is
// the single place that tests src for presence, so "no feed" is one concept
// rather than a nil check repeated down the file.
func (d *Downloader) hasFeed() bool { return d.src != nil }

// runWithoutFeed is the inbound loop for a provider with no change feed: an
// initial sweep, then one sweep per -sweep-interval, forever.
//
// Nothing here polls, because there is nothing to poll. That makes the interval
// the whole of the inbound latency — a remote edit is invisible until the next
// sweep — which is why every backend in this group has to document a sensible
// value for it rather than inheriting DefaultSweepInterval's 24h, tuned as that
// is for a backend whose feed already covers the live case.
//
// The three sweep triggers are unchanged and still live in startFeed, so a
// resumed sweep, a first run and -resync behave here exactly as they do under a
// feed. What differs is only what happens between sweeps.
func (d *Downloader) runWithoutFeed(ctx context.Context) {
	if !d.canSweep() {
		// No feed and nothing to enumerate: this downloader has no way to observe
		// the remote at all. Outbound sync still runs — that is the engine, not us.
		d.logf("[pull] provider offers neither a change feed nor enumeration: inbound sync is off")
		return
	}

	if _, err := d.startFeed(ctx); err != nil {
		if ctx.Err() != nil {
			return
		}
		// Same judgement as start's: a failed sweep must not take the loop down,
		// because the periodic schedule below is the retry.
		d.logf("[sweep] initial enumeration failed: %v", err)
		d.nextSweep = time.Now().Add(d.cad.Slow)
	} else {
		d.nextSweep = d.sweepDeadline()
	}
	d.resumeRefetches()

	if d.nextSweep.IsZero() {
		// -sweep-interval 0 disables the only *scheduled* inbound path there is.
		// That is a legitimate thing to ask for (a push-only mirror), and it is
		// also the shape someone lands in by copying a Drive config, so say it
		// once. The loop below still runs, because an abandoned push can put a
		// repair sweep on the schedule from the outbound side: 0 means "do not
		// poll", not "never repair" (DESIGN.md §9, M27).
		d.logf("[sweep] no change feed and -sweep-interval is 0: nothing will be pulled unless an abandoned push asks for a repair sweep")
	}

	for {
		// Capped at the repair delay rather than sleeping straight through to
		// nextSweep, because the other thing that can make a sweep due is written
		// to the state store by the engine, in another goroutine, with no way to
		// wake us. At a 24h interval that is a timer and one read transaction
		// every few minutes, which is why this is a poll and not a notification:
		// the two halves already share the state store and nothing else, and the
		// record has to be consulted after a restart regardless.
		wait := d.repairBase
		for _, due := range []time.Time{d.nextSweep, d.refetchDue} {
			if !due.IsZero() {
				if w := time.Until(due); w < wait {
					wait = w
				}
			}
		}
		if wait < time.Second {
			wait = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if !d.waitUnpaused(ctx) {
			return
		}
		// dueSweep reschedules before it runs, so a failure backs off by a whole
		// interval rather than spinning.
		d.dueSweep(ctx)
		d.dueRefetch(ctx)
	}
}

// start establishes the cursor to poll from, retrying at the slow cadence while
// the failure is transient.
//
// It can block for a while on a first run — that is the initial sweep — which is
// exactly why the whole downloader runs off the FUSE path: the mount is already
// up and usable while this happens. Polling does not begin until it returns,
// because the token it returns was taken *before* the sweep and using it earlier
// would mean applying changes twice for no benefit.
func (d *Downloader) start(ctx context.Context) (string, bool) {
	for attempt := 1; ; attempt++ {
		cursor, err := d.startFeed(ctx)
		if err == nil {
			return cursor, true
		}
		if ctx.Err() != nil {
			return "", false
		}
		if !provider.IsRetryable(err) {
			// A permanent failure must not keep inbound sync hostage. Fall back to
			// the feed alone: any sweep already recorded stays recorded, so a later
			// run (or -resync) picks it up where this one stopped.
			d.logf("[pull] initial reconcile failed: %v; continuing with the change feed alone", err)
			cursor, ferr := d.resumeCursor(ctx)
			if ferr != nil {
				d.logf("[pull] cannot start change feed: %v", ferr)
				return "", false
			}
			return cursor, true
		}
		d.logf("[pull] cannot start change feed (attempt %d): %v", attempt, err)
		select {
		case <-ctx.Done():
			return "", false
		case <-time.After(d.cad.Slow):
		}
	}
}

// resumeCursor returns the persisted cursor, or a fresh start token (persisted)
// on first run.
func (d *Downloader) resumeCursor(ctx context.Context) (string, error) {
	if cursor, ok, err := d.state.Cursor(); err != nil {
		return "", err
	} else if ok {
		return cursor, nil
	}
	cursor, err := d.src.StartCursor(ctx)
	d.mon.observe(err)
	if err != nil {
		return "", err
	}
	if err := d.state.SetCursor(cursor); err != nil {
		return "", err
	}
	return cursor, nil
}

// apply reconciles one remote change into the backing directory, committing its
// record changes immediately. The poll loop calls applyTo instead, so a page's
// removals share one commit.
func (d *Downloader) apply(ctx context.Context, ch provider.RemoteChange) error {
	forget := map[string]struct{}{}
	err := d.applyTo(ctx, ch, forget)
	if fErr := d.flushForget(forget); err == nil {
		err = fErr
	}
	return err
}

// flushForget drops the accumulated records in one transaction.
func (d *Downloader) flushForget(forget map[string]struct{}) error {
	if len(forget) == 0 {
		return nil
	}
	paths := make([]string, 0, len(forget))
	for p := range forget {
		paths = append(paths, p)
	}
	return d.state.ForgetMany(paths)
}

// applyTo reconciles one remote change into the backing directory, recording any
// path whose state records should be dropped in forget rather than deleting them
// on the spot.
func (d *Downloader) applyTo(ctx context.Context, ch provider.RemoteChange, forget map[string]struct{}) error {
	if reservedPath(ch.Path) || reservedPath(ch.MovedFrom) {
		// Neither applied nor recorded: a remote object of this name is not this
		// mount's to materialise, and its removal is not this mount's to apply.
		d.logf("[pull] skip    %s (the name of a drivel claim marker; never synced)", ch.Path)
		return nil
	}
	if !d.conflicts.Syncs(ch.Path) {
		// A conflicts folder this mount does not sync, in either direction: not
		// applied, not recorded, and its removal is not this mount's to apply.
		return nil
	}
	if ch.MovedFrom != "" && !d.conflicts.Syncs(ch.MovedFrom) {
		// Moved out of one: there is no local source to rename, so it arrives
		// as what it now is, a new object at its new path.
		ch.MovedFrom = ""
	}
	dst := filepath.Join(d.dataDir, filepath.FromSlash(ch.Path))

	// A move, if the provider said so and this mount can act on it, happens before
	// anything below reads a record or touches the tree: it is what makes the rest
	// of this function find the content already present and the baseline already
	// correct. Declining it is always safe — what follows is then exactly what it
	// was before providers could report a move at all.
	d.moveLocally(ch)

	if ch.Removed || ch.File == nil {
		if err := d.applyRemoval(ctx, ch.Path, dst, forget); err != nil {
			return err
		}
		d.mon.count(cRemoteDeletes, 1)
		d.mon.applied(ch.Path, StatusDeleted)
		return nil
	}

	// The path exists again, so a removal earlier in this same page is cancelled:
	// flushing it would delete the echo this change is about to write, leaving the
	// next report of the file unrecognisable as one we already have.
	delete(forget, ch.Path)

	f := ch.File

	// A Google-native doc (M7b) has no byte stream: Get on it fails, its size is
	// not a byte count and it has no checksum. There is nothing to place locally,
	// so say so once instead of failing a download on every report of it.
	if f.ExportOnly {
		d.logf("[pull] skip    %s (Google-native document; no downloadable content)", ch.Path)
		return nil
	}

	echo, hasEcho, err := d.state.GetEcho(ch.Path)
	if err != nil {
		return err
	}
	// Echo / loop breaker (§4): if this is the content we last synced at this
	// path — our own upload coming back, or a re-report of an applied download —
	// drop it.
	if hasEcho && echo.Matches(f.Hash, f.Version) {
		return nil
	}
	// The same test one step further back: a path WE deleted, whose provider is
	// still reporting the content the deletion removed. Only when there is no
	// live echo, because an echo means the path has been synced since and a sync
	// clears the tombstone — so the two can never both answer.
	if !hasEcho {
		drop, sErr := d.suppressDeleted(ch.Path, f)
		if sErr != nil {
			return sErr
		}
		if drop {
			return nil
		}
	}

	if f.IsDir {
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
		d.logf("[pull] mkdir   %s", ch.Path)
		d.mon.applied(ch.Path, StatusSynced)
		return d.rememberApplied(ch.Path, dst, f)
	}

	// Lazy mode (M5): a local placeholder has no resident content, so it cannot
	// have diverged — there is nothing for the user to have edited. Hashing it would
	// compute the digest of a hole and read as a local edit, manufacturing a bogus
	// conflict copy on every remote change. Re-stamp it with the new metadata and
	// let the next read fetch the fresh bytes.
	if d.mat != nil && d.mat.IsPlaceholder(ch.Path) {
		if err := d.mat.CreatePlaceholder(ch.Path, *f); err != nil {
			return err
		}
		d.logf("[pull] restamp %s (placeholder, %d bytes pending)", ch.Path, f.Size)
		d.mon.count(cRestamps, 1)
		d.mon.applied(ch.Path, StatusPlaceholder)
		return d.rememberApplied(ch.Path, dst, f)
	}

	localHash, localExists, err := statMD5(dst)
	if err != nil {
		return err
	}

	// Loop breaker vs. on-disk content: if the backing file already holds these
	// exact bytes (common on first sync when the dir already mirrors Drive), skip
	// the download entirely and just record the echo.
	if f.Hash != "" && localExists && localHash == f.Hash {
		return d.rememberApplied(ch.Path, dst, f)
	}

	// Conflict (§6): we're past the echo check, so the remote content differs from
	// what we last synced. If the local file also diverged from that baseline (or
	// there's no baseline and it differs from the remote), both sides changed —
	// resolve last-writer-wins by mtime and keep the loser as a conflict copy.
	// Needs content hashes to compare; hashless objects (Google-native docs) fall
	// through to a plain apply.
	if f.Hash != "" && localExists {
		baseline := ""
		if hasEcho {
			baseline = echo.Hash
		}
		if localHash != baseline {
			return d.resolveConflict(ctx, ch.Path, dst, f)
		}
	}
	// The same question without a remote digest — SFTP and every M17–M21
	// backend. This used to fall through to the plain apply below, so on those
	// backends any local edit not yet pushed was overwritten by the next remote
	// change with no conflict copy at all (found by MC-31 over a real sshd,
	// 2026-10-18).
	if f.Hash == "" && localExists {
		verdict, err := d.hashlessLocal(ctx, ch.Path, dst, f, echo, hasEcho)
		if err != nil {
			return err
		}
		switch verdict {
		case localSame:
			return d.rememberApplied(ch.Path, dst, f)
		case localDiverged:
			return d.resolveConflict(ctx, ch.Path, dst, f)
		}
	}

	if err := d.materialize(ctx, ch.Path, dst, f); err != nil {
		return err
	}
	return d.rememberApplied(ch.Path, dst, f)
}

// moveLocally applies a change's MovedFrom to the backing tree: one rename of the
// subtree, and one re-key of the records that describe it. It reports whether the
// move was performed.
//
// This is the whole receiving half of a provider-reported move, and what it
// replaces is expensive rather than wrong: without it the subtree is downloaded
// a second time under its new name while the old copy becomes N orphans that
// only the delete pass can retire, and past -max-deletes that pass refuses
// itself and the client stays doubled (MC-27). One rename and one commit do
// instead.
//
// Every condition below declines rather than failing, and the fallback is not a
// degraded path but the behaviour that shipped before this existed. Two of them
// are the ones that matter:
//
//   - The paths are validated here, above the seam, because MovedFrom is the
//     first thing a provider says that drives a destructive operation on a
//     *subtree*. A plugin is code drivel launched and trusted (M9 rule 8) and
//     this is not a sandbox — but the blast radius of a bad answer is the whole
//     backing tree rather than one path, so it is checked anyway, in the same
//     spirit as sftp's structural containment.
//   - An occupied destination declines. os.Rename replaces a file silently and
//     fails on a non-empty directory, and what it would replace may be local
//     work that has never been pushed — which is §6's question to answer, with a
//     conflict copy, and not something a move may decide by overwriting.
func (d *Downloader) moveLocally(ch provider.RemoteChange) bool {
	if ch.MovedFrom == "" || ch.Removed || ch.File == nil {
		return false
	}
	if !relPathOK(ch.MovedFrom) || !relPathOK(ch.Path) {
		d.logf("[pull] ignoring a reported move %q -> %q: not a path inside the mount", ch.MovedFrom, ch.Path)
		return false
	}
	// Either path inside the other is not a move a filesystem can perform
	// (os.Rename answers EINVAL) and is the one shape where the two key ranges
	// nest. Refuse it here so it is a decision rather than an error in a log.
	if inside(ch.Path, ch.MovedFrom) || inside(ch.MovedFrom, ch.Path) {
		return false
	}
	src := filepath.Join(d.dataDir, filepath.FromSlash(ch.MovedFrom))
	dst := filepath.Join(d.dataDir, filepath.FromSlash(ch.Path))
	if _, err := os.Lstat(src); err != nil {
		// Nothing of ours at the old path: a client that never held the subtree,
		// or a re-report of a move already applied. The ordinary apply fetches
		// what it needs, which for the first case is the right thing to do.
		return false
	}
	if _, err := os.Lstat(dst); err == nil {
		d.logf("[pull] not moving %s -> %s: the destination already exists locally", ch.MovedFrom, ch.Path)
		return false
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		d.logf("[pull] move %s -> %s: %v", ch.MovedFrom, ch.Path, err)
		return false
	}
	if err := os.Rename(src, dst); err != nil {
		d.logf("[pull] move %s -> %s: %v", ch.MovedFrom, ch.Path, err)
		return false
	}
	// The bytes have moved. A failure to re-key the records leaves them naming the
	// old path, which is the state of affairs this whole mechanism improves on
	// rather than a new hazard — the sweep repairs it at the cost it always had —
	// so it is reported and the page carries on instead of being replayed.
	n, err := d.state.RenameSubtree(ch.MovedFrom, ch.Path)
	if err != nil {
		d.logf("[pull] move %s -> %s: re-keying records: %v", ch.MovedFrom, ch.Path, err)
	}
	d.logf("[pull] move    %s -> %s (%d record(s) re-keyed)", ch.MovedFrom, ch.Path, n)
	d.mon.count(cRemoteMoves, 1)
	// A subtree's worth of paths changed name at once, and itemising them would
	// be a walk; a subscriber is told to re-read instead.
	d.mon.resyncAll("a directory moved: " + ch.MovedFrom + " -> " + ch.Path)
	return true
}

// relPathOK reports whether p is usable as a root-relative path inside the
// backing tree: non-empty, already clean, not absolute and not climbing out.
//
// path.Clean leaves a leading ".." exactly where it is, so the prefix test is
// what catches an escape; requiring the path to equal its own Clean also refuses
// the sloppier forms ("a//b", "a/./b") rather than quietly accepting them,
// because a provider that sends those is a provider whose other answers are
// worth distrusting too.
func relPathOK(p string) bool {
	if p == "" || p == "." || path.IsAbs(p) || p != path.Clean(p) {
		return false
	}
	return p != ".." && !strings.HasPrefix(p, "../")
}

// inside reports whether p is q itself or a path beneath it.
func inside(p, q string) bool { return p == q || strings.HasPrefix(p, q+"/") }

// materialize brings rel's remote content into the backing store: a real download
// in eager mode, a zero-byte placeholder in lazy mode.
//
// Only paths that exist remotely may become placeholders — a placeholder is a
// promise that the content can be fetched from rel later. Conflict copies are
// therefore always downloaded in full (see resolveConflict): their paths are
// local-only inventions, so a placeholder there could never be hydrated.
func (d *Downloader) materialize(ctx context.Context, rel, dst string, f *provider.RemoteFile) error {
	if d.mat == nil {
		if err := d.download(ctx, rel, dst, f.Modified); err != nil {
			return err
		}
		d.logf("[pull] download %s", rel)
		d.mon.applied(rel, StatusSynced)
		return nil
	}
	if err := d.mat.CreatePlaceholder(rel, *f); err != nil {
		return err
	}
	d.logf("[pull] placeholder %s (%d bytes, hydrates on read)", rel, f.Size)
	d.mon.count(cPlaceholders, 1)
	d.mon.applied(rel, StatusPlaceholder)
	return nil
}

// suppressDeleted reports whether a remote report of rel is this mount's own
// deletion coming back — §4's loop breaker applied across the delete that
// cleared the baseline it normally compares against.
//
// Both inbound paths ask: the feed, where the defect was found, and the sweep,
// whose listing can be taken inside the same window. Two conditions, and
// neither is sufficient alone. The record must be live, because a provider that
// has not caught up within tombstoneWindow is not what this describes. And the
// report must carry the content the deletion removed — identity, not time:
// comparing a provider's timestamp against ours is wrong by the skew between
// two clocks, and a re-creation that is genuinely new content is applied
// whatever either clock says.
//
// A tombstone with no identity never matches (state.Echo.Matches answers false
// for an empty record), which is the safe direction and is why the engine does
// not bother writing one.
func (d *Downloader) suppressDeleted(rel string, f *provider.RemoteFile) (bool, error) {
	t, ok, err := d.state.Tombstone(rel)
	if err != nil || !ok {
		return false, err
	}
	age := time.Since(t.Deleted)
	if age > tombstoneWindow {
		return false, nil
	}
	if !t.Echo.Matches(f.Hash, f.Version) {
		return false, nil
	}
	d.logf("[pull] ignore  %s (deleted here %s ago; the remote is still reporting what we deleted)",
		rel, age.Round(time.Second))
	d.mon.count(cSuppressed, 1)
	return true, nil
}

// resolveConflict applies the §6 policy when a remote edit collides with a
// divergent local edit at rel (backing path dst): last-writer-wins by mtime, and
// the losing side goes where the mount's conflict policy sends it (conflicts.go).
// A loser that cannot be kept fails the apply, which the refetch pass retries;
// it is never replaced unkept.
func (d *Downloader) resolveConflict(ctx context.Context, rel, dst string, f *provider.RemoteFile) error {
	info, err := os.Stat(dst)
	if err != nil {
		return err
	}
	localMtime := info.ModTime()

	if f.Modified.After(localMtime) {
		// Remote is newer: it wins the real path, and the local edit is kept.
		where, err := d.conflicts.keepLocal(ctx, d.conflictEnv(ctx), rel, localMtime)
		if err != nil {
			return fmt.Errorf("keeping the local edit before applying a newer remote: %w", err)
		}
		// rel still exists remotely, so the winner may be a placeholder; the copy
		// keeps the real local bytes it was moved from.
		if err := d.materialize(ctx, rel, dst, f); err != nil {
			return err
		}
		d.logf("[pull] conflict %s: remote newer, local edit kept in %s", rel, where)
		d.conflicted(rel)
		return d.rememberApplied(rel, dst, f)
	}

	// Local is newer (or same mtime): it keeps the real path, and the remote
	// version is kept. Leave dst and its echo untouched — the uploader still holds
	// the local edit to push. The copy is downloaded in full even in lazy mode: it
	// could never be hydrated later.
	where, err := d.conflicts.keepRemote(ctx, d.conflictEnv(ctx), rel, *f)
	if err != nil {
		return fmt.Errorf("keeping the remote version: %w", err)
	}
	if where != "" {
		d.logf("[pull] conflict %s: local newer, remote version kept in %s", rel, where)
		d.conflicted(rel)
	}
	return nil
}

// conflicted reports a §6 resolution to the monitor.
func (d *Downloader) conflicted(rel string) {
	d.mon.count(cConflicts, 1)
	d.mon.applied(rel, StatusConflict)
}

// download streams the object at path into dst atomically (temp file + rename in
// the destination directory) and stamps its mtime to the remote modified time.
func (d *Downloader) download(ctx context.Context, path, dst string, modified time.Time) error {
	n, err := fetchTo(ctx, d.store, path, dst, modified, d.mon.observe)
	if err != nil {
		return err
	}
	d.mon.count(cDownloads, 1)
	d.mon.count(cDownloadBytes, n)
	return nil
}

// rememberApplied records what we just wrote so a subsequent change-feed report
// of the same content is recognised as an echo and dropped.
//
// dst is the backing file the record describes; its size and mtime become the
// baseline's local fingerprint (see state.Echo), which is the only thing a
// provider with no content checksum can later use to tell "unmodified since we
// agreed with the remote" from "edited locally". A stat that fails records no
// fingerprint, which reads as "not recorded" and is the safe direction.
func (d *Downloader) rememberApplied(path, dst string, f *provider.RemoteFile) error {
	fi, err := os.Stat(dst)
	if err != nil {
		fi = nil
	}
	size, mtime := state.FingerprintOf(fi)
	return d.state.SetEcho(path, state.Echo{
		Hash: f.Hash, Version: f.Version, At: time.Now(),
		LocalSize: size, LocalMTime: mtime,
	})
}

// hashlessVerdict is what hashlessLocal found out about a local file.
type hashlessVerdict int

const (
	localUnchanged hashlessVerdict = iota // as last synced: apply the remote over it
	localSame                             // already holds the remote's bytes: record, no download
	localDiverged                         // edited here: a §6 conflict
)

// hashlessLocal decides, for a remote object with no content digest, whether
// the local file at dst is an edit the remote change must not overwrite.
//
// With a baseline that carries a local fingerprint the answer is metadata:
// the same test the delete rows and the lost-push row use (fingerprintDiverged),
// so the three can never disagree about whether a file changed. Without one —
// no baseline, or one written before M18 stored fingerprints — the only
// evidence is the bytes: a different length is a different file, and an equal
// one is settled by fetching the remote and comparing, which costs nothing the
// plain apply would not have spent anyway.
func (d *Downloader) hashlessLocal(ctx context.Context, rel, dst string, f *provider.RemoteFile, echo state.Echo, hasEcho bool) (hashlessVerdict, error) {
	fi, err := os.Stat(dst)
	if err != nil {
		return localUnchanged, err
	}
	if hasEcho && !echo.LocalMTime.IsZero() {
		if fingerprintDiverged(fi, echo) {
			return localDiverged, nil
		}
		return localUnchanged, nil
	}
	if fi.Size() != f.Size {
		return localDiverged, nil
	}
	cmp := filepath.Join(filepath.Dir(dst), ".drivel-cmp-"+filepath.Base(dst))
	defer os.Remove(cmp)
	if _, err := fetchTo(ctx, d.store, rel, cmp, time.Time{}, d.mon.observe); err != nil {
		return localUnchanged, err
	}
	same, err := sameBytes(dst, cmp)
	if err != nil {
		return localUnchanged, err
	}
	if same {
		return localSame, nil
	}
	return localDiverged, nil
}

// sameBytes reports whether two files hold identical content.
func sameBytes(a, b string) (bool, error) {
	fa, err := os.Open(a)
	if err != nil {
		return false, err
	}
	defer fa.Close()
	fb, err := os.Open(b)
	if err != nil {
		return false, err
	}
	defer fb.Close()
	ba, bb := make([]byte, 64<<10), make([]byte, 64<<10)
	for {
		na, ea := io.ReadFull(fa, ba)
		nb, eb := io.ReadFull(fb, bb)
		if na != nb || !bytes.Equal(ba[:na], bb[:nb]) {
			return false, nil
		}
		aDone := ea == io.EOF || ea == io.ErrUnexpectedEOF
		bDone := eb == io.EOF || eb == io.ErrUnexpectedEOF
		if ea != nil && !aDone {
			return false, ea
		}
		if eb != nil && !bDone {
			return false, eb
		}
		if aDone || bDone {
			return aDone && bDone, nil
		}
	}
}

// statMD5 returns the hex md5 of the file at p, reporting exists=false (no error)
// when the file is absent.
func statMD5(p string) (hash string, exists bool, err error) {
	h, err := fileMD5(p)
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return h, true, nil
}

// conflictName inserts a " (conflict <ts>)" marker before the extension of a
// root-relative slash path, e.g. "sub/a.txt" -> "sub/a (conflict 2026-07-17 09-30-00).txt".
func conflictName(rel string, t time.Time) string {
	ext := path.Ext(rel)
	return strings.TrimSuffix(rel, ext) + " (conflict " + t.Format("2006-01-02 15-04-05") + ")" + ext
}

// fileMD5 returns the hex md5 of the file at path (Drive's md5Checksum encoding).
// The algorithm is dictated by the Drive API and is used only to compare bytes
// against a checksum Drive already reported — never as a security property.
func fileMD5(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := md5.New() //nolint:gosec // G401: dictated by Drive's md5Checksum, see above
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// applyRemoval applies a remote deletion of rel to the local tree, and is the
// feed's counterpart of the sweep's deleteGone: only what still matches its
// baseline is deleted. A file changed here since the last sync, or one never
// synced at all, is the only copy of that work, and a deletion made somewhere
// else knows nothing about it. It is kept, its stale baseline is dropped, and it
// is pushed back, so an edit beats a deletion it never saw. A directory goes the
// same way, child by child, and survives if anything in it was kept.
//
// Until 2026-10-18 this was one os.RemoveAll, which destroyed an unpushed edit
// to a file deleted remotely, and every never-synced file under a directory
// deleted remotely, with nothing in the log to say so.
func (d *Downloader) applyRemoval(ctx context.Context, rel, dst string, forget map[string]struct{}) error {
	info, err := os.Lstat(dst)
	if errors.Is(err, os.ErrNotExist) {
		// Already gone: our own deletion coming back, or a prior run's.
		forget[rel] = struct{}{}
		d.logf("[pull] delete  %s", rel)
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		if d.removeIfSynced(ctx, rel, dst, info, forget) {
			d.logf("[pull] delete  %s", rel)
		}
		return nil
	}

	// Children first, deepest first, so a directory is removed only once
	// everything in it has been decided.
	var paths []string
	err = filepath.WalkDir(dst, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // unreadable: left where it is, which is the safe direction
		}
		paths = append(paths, p)
		return nil
	})
	if err != nil {
		return err
	}
	kept := 0
	for i := len(paths) - 1; i >= 0; i-- {
		p := paths[i]
		r, rErr := filepath.Rel(d.dataDir, p)
		if rErr != nil {
			continue
		}
		r = filepath.ToSlash(r)
		fi, err := os.Lstat(p)
		if err != nil {
			continue
		}
		if fi.IsDir() {
			if os.Remove(p) == nil {
				forget[r] = struct{}{}
			}
			continue
		}
		if !d.removeIfSynced(ctx, r, p, fi, forget) {
			kept++
		}
	}
	if kept > 0 {
		d.logf("[pull] delete  %s (kept %d file(s) changed here or never synced)", rel, kept)
		return nil
	}
	d.logf("[pull] delete  %s", rel)
	return nil
}

// removeIfSynced deletes the local file at rel if it still holds what was last
// synced there, and reports whether it did. Otherwise it is kept and re-pushed.
func (d *Downloader) removeIfSynced(ctx context.Context, rel, abs string, fi os.FileInfo, forget map[string]struct{}) bool {
	if d.mat != nil && d.mat.IsPlaceholder(rel) {
		// Remote-born and holding nothing local: there is no work here to keep.
		if err := os.Remove(abs); err != nil {
			d.logf("[pull] removing %s: %v", rel, err)
			return false
		}
		forget[rel] = struct{}{}
		return true
	}
	var why string
	switch e, ok, err := d.state.GetEcho(rel); {
	case err != nil:
		why = "its sync record could not be read"
	case !fi.Mode().IsRegular():
		why = "it is a " + kindOf(fi.Mode()) + ", which never syncs"
	case !ok:
		why = "it was never synced from here"
	default:
		var same bool
		same, why = d.matchesBaseline(rel, abs, e)
		if same {
			if err := os.Remove(abs); err != nil {
				d.logf("[pull] removing %s: %v", rel, err)
				return false
			}
			forget[rel] = struct{}{}
			return true
		}
	}
	d.logf("[pull] keep    %s (deleted remotely, but %s)", rel, why)
	if !fi.Mode().IsRegular() || !d.conflicts.Syncs(rel) {
		return false
	}
	// Dropped inline, as the sweep does: the push below writes a fresh echo for
	// this path, and a batched forget flushed after it would erase it.
	delete(forget, rel)
	if err := d.state.DeleteEcho(rel); err != nil {
		d.logf("[pull] clearing the baseline for %s: %v", rel, err)
	}
	// A new file still open through the mount is pushed by its own close; sent
	// now it would go part-written (fsevent.Writing). One changed since this
	// process opened is already queued in the engine (engineOwns).
	if d.rec.Push != nil && !d.rec.Writing.Has(rel) && !d.engineOwns(fi) {
		d.rec.Push.Push(ctx, fsevent.Event{Op: fsevent.OpWrite, Path: rel})
	}
	return false
}
