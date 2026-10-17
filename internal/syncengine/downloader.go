// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package syncengine

import (
	"context"
	"crypto/md5" //nolint:gosec // G501: Drive addresses content by MD5; see fileMD5
	"encoding/hex"
	"errors"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

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

	// openedAt is when this mount's pull loop was constructed, which is before
	// the filesystem is served and therefore before any write can reach the
	// engine (app.Mount.Open builds both; Run mounts afterwards).
	//
	// It is the boundary the sweep's lost-push row uses: a local file modified
	// since this instant is one the running engine is already responsible for,
	// and one modified before it is not. See repushDiverged.
	openedAt time.Time
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
		repairBase: repairDelay, openedAt: time.Now(),
	}
}

// Run polls until ctx is cancelled. It resumes from the persisted cursor, or
// obtains a fresh start token on first run so it only ever sees changes from
// "now" forward.
func (d *Downloader) Run(ctx context.Context) {
	if !d.hasFeed() {
		d.runWithoutFeed(ctx)
		return
	}
	cursor, ok := d.start(ctx)
	if !ok {
		return
	}
	d.nextSweep = d.sweepDeadline()

	wait := d.cad.Fast
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}

		// Before polling, not after: a sweep replaces the cursor, so applying a page
		// fetched with the old one and then sweeping would advance past changes the
		// sweep's older token was about to replay.
		if fresh, swept := d.dueSweep(ctx); swept {
			cursor, wait = fresh, d.cad.Fast
		}

		changes, next, err := d.src.Changes(ctx, cursor)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
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
			}
		}
		if err := d.flushForget(forget); err != nil {
			d.logf("[pull] clearing %d removed record(s): %v", len(forget), err)
			continue // leave the cursor where it is; the page replays idempotently
		}

		if next != "" && next != cursor {
			if err := d.state.SetCursor(next); err != nil {
				d.logf("[pull] persist cursor: %v", err)
			}
			cursor = next
		}

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
		if !d.nextSweep.IsZero() {
			if w := time.Until(d.nextSweep); w < wait {
				wait = w
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
		// dueSweep reschedules before it runs, so a failure backs off by a whole
		// interval rather than spinning.
		d.dueSweep(ctx)
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
	dst := filepath.Join(d.dataDir, filepath.FromSlash(ch.Path))

	// A move, if the provider said so and this mount can act on it, happens before
	// anything below reads a record or touches the tree: it is what makes the rest
	// of this function find the content already present and the baseline already
	// correct. Declining it is always safe — what follows is then exactly what it
	// was before providers could report a move at all.
	d.moveLocally(ch)

	if ch.Removed || ch.File == nil {
		// Idempotent: if we (or a prior run) already removed it, RemoveAll is a
		// no-op. Covers our own delete echo without a dedicated record.
		if err := os.RemoveAll(dst); err != nil {
			return err
		}
		d.logf("[pull] delete  %s", ch.Path)
		forget[ch.Path] = struct{}{}
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
		return nil
	}
	if err := d.mat.CreatePlaceholder(rel, *f); err != nil {
		return err
	}
	d.logf("[pull] placeholder %s (%d bytes, hydrates on read)", rel, f.Size)
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
	return true, nil
}

// resolveConflict applies the §6 policy when a remote edit collides with a
// divergent local edit at rel (backing path dst): last-writer-wins by mtime, and
// the losing side is preserved as a sibling conflict copy. Conflict copies are
// local-only in v1 — they are not synced back up.
func (d *Downloader) resolveConflict(ctx context.Context, rel, dst string, f *provider.RemoteFile) error {
	info, err := os.Stat(dst)
	if err != nil {
		return err
	}
	localMtime := info.ModTime()

	if f.Modified.After(localMtime) {
		// Remote is newer: it wins the real path; the local edit becomes the copy.
		copyRel := conflictName(rel, localMtime)
		copyDst := filepath.Join(d.dataDir, filepath.FromSlash(copyRel))
		if err := os.Rename(dst, copyDst); err != nil {
			return err
		}
		// rel still exists remotely, so the winner may be a placeholder; the copy
		// keeps the real local bytes it was renamed from.
		if err := d.materialize(ctx, rel, dst, f); err != nil {
			return err
		}
		d.logf("[pull] conflict %s: remote newer, local kept as %s", rel, copyRel)
		return d.rememberApplied(rel, dst, f)
	}

	// Local is newer (or same mtime): it keeps the real path; the remote version is
	// saved alongside as the copy. Leave dst and its echo untouched — the uploader
	// still holds the local edit to push. The copy is downloaded in full even in
	// lazy mode: copyRel exists only locally, so it could never be hydrated later.
	copyRel := conflictName(rel, f.Modified)
	copyDst := filepath.Join(d.dataDir, filepath.FromSlash(copyRel))
	if err := d.download(ctx, rel, copyDst, f.Modified); err != nil {
		return err
	}
	d.logf("[pull] conflict %s: local newer, remote saved as %s", rel, copyRel)
	return nil
}

// download streams the object at path into dst atomically (temp file + rename in
// the destination directory) and stamps its mtime to the remote modified time.
func (d *Downloader) download(ctx context.Context, path, dst string, modified time.Time) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	rc, err := d.store.Get(ctx, path)
	if err != nil {
		return err
	}
	defer rc.Close()

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".drivel-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename

	if _, err := io.Copy(tmp, rc); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if !modified.IsZero() {
		_ = os.Chtimes(tmpName, modified, modified)
	}
	return os.Rename(tmpName, dst)
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
