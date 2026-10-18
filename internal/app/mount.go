// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

// Package app is drivel's composition root below main: it turns a description of
// one or more mounts into running mounts and owns their lifecycle.
//
// It exists as a package rather than as the body of runMount for two reasons that
// turned out to be the same refactor. One process must serve N mounts (M8), which
// a function that owns the signal context and the process's defers cannot do. And
// the wiring — flag plumbing, provider selection, the shutdown ordering — was the
// one part of drivel no test could reach, so a flag that silently stopped being
// read was invisible to every other test in the tree (DESIGN.md §9, M0).
//
// The layering rule is unchanged: nothing here knows what a Drive folder ID is.
// A mount names a provider *kind* and carries that provider's configuration
// undecoded, and internal/provider's registry does the rest.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/syshlted/drivel/internal/claim"
	"github.com/syshlted/drivel/internal/fsevent"
	"github.com/syshlted/drivel/internal/hydrate"
	"github.com/syshlted/drivel/internal/mount"
	"github.com/syshlted/drivel/internal/state"
	"github.com/syshlted/drivel/internal/syncengine"
	"github.com/syshlted/drivel/internal/vfs"
	"github.com/syshlted/drivel/provider"
)

// eventBuffer is how many mount events may be in flight before the FUSE handler
// blocks on the sync engine. Buffered so brief FS bursts don't stall on the
// consumer; per mount, since one busy mount must not stall another.
const eventBuffer = 1024

// inboundDrainTimeout bounds how long Run waits for the inbound loop to stop
// after the mount has gone away. It is deliberately shorter than the engine's
// drain: the engine is finishing uploads the user is waiting for, while the pull
// loop and the sweep have nothing in hand that is worth delaying a shutdown for —
// a sweep that does not finish is resumed, and a download that does not finish is
// retried. What the wait buys is that neither of them is still touching the state
// DB or the backing descriptor when Close takes them away.
const inboundDrainTimeout = 10 * time.Second

// MountSpec describes one mount to run. It is plain data: whatever produced it —
// the flag set, a config file, a test — is interchangeable, which is what keeps
// the single-mount path and the N-mount path from drifting apart.
type MountSpec struct {
	// Name identifies this mount in logs and errors. With one mount it is
	// cosmetic; with several it is the only way to tell whose message you are
	// reading. Empty falls back to the mountpoint.
	Name string

	Mountpoint string // where the filesystem is mounted
	DataDir    string // backing dir; "" selects in-place mode (Linux only)
	StateDB    string // engine-level sync state (cursor + echo records)

	// Provider names a kind registered in the provider.Registry. Empty runs the
	// mount log-only (M1 behaviour): no network, no auth, no state store.
	Provider string
	// ProviderConfig carries that provider's own configuration, undecoded, as the
	// TOML table the user wrote. See provider.Config; provider.EncodeConfig builds
	// one from a settings struct in hand, which is what the flag and fstab paths
	// do.
	ProviderConfig provider.Config

	Lazy  bool // M5 lazy hydration
	Debug bool // FUSE-level tracing

	// Xattr serves extended attributes through the mountpoint by passing them to
	// the backing store. Off by default for every mount: see mount.Options.Xattr
	// for what the passthrough exposes, and note that it is not what M5 needs —
	// the hydrator reads and writes its marker on the backing path directly, which
	// is below this mount and unaffected either way.
	Xattr bool

	// Logger is where everything this mount does reports. Nil uses the log
	// package's default, which is what a single mount wants: its output is then
	// byte-for-byte what drivel printed before there could be more than one.
	// App.New fills it in with a name-prefixed logger as soon as there are two.
	Logger *log.Logger

	// M7b enumeration & reconcile.
	Resync        bool
	Materialize   bool
	MaxDeletes    int
	SweepInterval time.Duration

	// PushDelay is how long a path must go without a further content change
	// before its upload is dispatched — the engine's per-path coalescing window.
	// Zero selects syncengine.DefaultPushDelay.
	//
	// It is a tuning value rather than a safety one: every setting pushes the
	// same bytes, just sooner or later and in more or fewer requests. Raising it
	// trades promptness for quota and for a calmer outbound queue, which is what
	// keeps a bulk import from filling the event channel and stalling the FUSE
	// handler. It cannot starve a path — see syncengine's maxWaitFactor — and it
	// has no bearing on a file that is held open and never closed, which emits no
	// event at any setting (push-on-close; see internal/vfs/file.go).
	PushDelay time.Duration

	// Transfer concurrency, per mount and deliberately not per process. Two
	// mounts are two sets of credentials; a limiter they shared would let either
	// one starve the other and read its activity off the contention, which is the
	// cross-mount coupling Validate exists to prevent. Zero selects the package
	// default in each case.
	//
	// They are separate numbers because the directions are not alike: a link's
	// uplink and downlink differ, the pools bound different work (whole-file
	// pushes off the FUSE path against hydrations blocking a read on it), and a
	// provider may well cap the two differently.
	UploadWorkers  int // size of the outbound path-hashed pool (syncengine)
	HydrateWorkers int // concurrent lazy fetches (hydrate); ignored unless Lazy

	// FsName is the device name the mount reports to the kernel — the source
	// column in findmnt(8) and /proc/self/mountinfo. Empty means "drivel", which
	// is what every mount said before there was a reason to differ.
	//
	// The fstab helper sets it to the spec field of the fstab line, so that
	// mount(8) and umount(8) can match the mount by the name the admin wrote.
	FsName string

	// AllowOther and BackendOptions are passed to the mount backend unchanged;
	// see mount.Options for what they cost.
	AllowOther     bool
	BackendOptions []string

	// Ready, if non-nil, is called once this mount is live. See mount.Options.
	Ready func()
}

// label is what this mount is called in a message.
func (s MountSpec) label() string {
	if s.Name != "" {
		return s.Name
	}
	return s.Mountpoint
}

// fsName is the device name this mount reports to the kernel. It defaults to
// "drivel" rather than to the mount's own name so that an existing mount's entry
// in /proc/self/mountinfo is byte-for-byte what it was before FsName existed;
// anything matching on it keeps working.
func (s MountSpec) fsName() string {
	if s.FsName != "" {
		return s.FsName
	}
	return "drivel"
}

// controlName is what the control API calls this mount: its configured name, or
// for the flag path — which has one mount and no name — the same short form the
// log prefix uses.
func (s MountSpec) controlName() string {
	if s.Name != "" {
		return s.Name
	}
	return s.logName()
}

// logName is label shortened for a log prefix. The flag path has no name and
// its mountpoint may be an absolute path long enough to bury the message.
func (s MountSpec) logName() string {
	if s.Name != "" {
		return s.Name
	}
	return filepath.Base(s.Mountpoint)
}

// Mount is one opened mount: every resource acquired, nothing yet running.
//
// The three-phase split (Open / Run / Close) is what N mounts need and one mount
// never did. Open acquires and can fail; Run blocks until unmount and then
// drains; Close releases. Keeping acquisition separate from serving is what lets
// a process that fails to bring up mount 3 of 5 unmount and drain the two that
// already came up, instead of exiting with them still mounted.
type Mount struct {
	spec    MountSpec
	backing *mount.Backing
	store   provider.Store
	state   *state.Store
	hyd     *hydrate.Hydrator
	engine  *syncengine.Engine
	down    *syncengine.Downloader
	events  chan fsevent.Event
	writing *fsevent.Writing // new files still open, shared by the mount and the sweep

	// mon is this mount's observation surface (M14): the engine, the pull loop
	// and the sweep write to it, and the control API reads it. Built for every
	// mount, log-only included, so a status query never has to ask whether one
	// exists.
	mon *syncengine.Monitor

	// closeStore is the provider's release hook, if it has one. provider.Store
	// says nothing about closing — most backends have nothing to release — so a
	// store that does announces it by implementing io.Closer.
	closeStore func() error

	// claim marks the backing store as this process's (internal/claim), so a
	// second drivel can see it. Held from Open to Close.
	claim *claim.Claim

	lg     *log.Logger
	closed bool
}

// logf writes one line for this mount.
func (m *Mount) logf(format string, args ...any) {
	if m.lg == nil {
		log.Printf(format, args...)
		return
	}
	m.lg.Printf(format, args...)
}

// Open acquires everything one mount needs and starts nothing. The returned
// Mount must be Closed whether or not Run is ever called.
func Open(ctx context.Context, spec MountSpec, reg *provider.Registry) (*Mount, error) {
	if spec.Mountpoint == "" {
		return nil, errors.New("mount has no mountpoint")
	}
	if spec.Lazy && spec.Provider == "" {
		// A placeholder is a promise that the bytes can be fetched later; without a
		// provider there is nothing to redeem it against.
		return nil, fmt.Errorf("%s: lazy hydration needs a provider (there is nothing to hydrate from)", spec.label())
	}
	if spec.Provider != "" && spec.StateDB == "" {
		// Not merely a bad path: an engine with no state store records no echoes,
		// and §4 echo suppression is what stops every inbound change from being
		// re-uploaded — and what M7b uses as its delete baseline. Refuse rather
		// than quietly running without it.
		return nil, fmt.Errorf("%s: a provider needs a state DB (cursor + echo records)", spec.label())
	}
	if err := os.MkdirAll(spec.Mountpoint, 0o755); err != nil {
		return nil, fmt.Errorf("creating %s: %w", spec.Mountpoint, err)
	}
	if spec.DataDir != "" {
		if err := os.MkdirAll(spec.DataDir, 0o755); err != nil {
			return nil, fmt.Errorf("creating %s: %w", spec.DataDir, err)
		}
	}

	m := &Mount{
		writing: &fsevent.Writing{},
		spec:    spec, lg: spec.Logger, events: make(chan fsevent.Event, eventBuffer),
		mon: syncengine.NewMonitor(spec.controlName()),
	}
	// Anything acquired before a later failure has to be handed back, or a failed
	// startup leaks a dirfd, a bbolt lock and an HTTP/3 transport per mount.
	ok := false
	defer func() {
		if !ok {
			_ = m.Close()
		}
	}()

	// Resolve the backing store. In in-place mode this opens a dirfd to the
	// mountpoint BEFORE we mount over it, so both the FUSE backend and the sync
	// engine reach the underlying directory (via /proc/self/fd/N) instead of
	// recursing through the overlay. Held open until after unmount.
	backing, err := mount.ResolveBacking(spec.Mountpoint, spec.DataDir)
	if err != nil {
		return nil, err
	}
	m.backing = backing
	if err := m.claimBacking(spec, backing); err != nil {
		return nil, err
	}
	if backing.InPlace {
		m.logf("in-place mode: %s is its own backing store (via %s)", spec.Mountpoint, backing.Path)
	} else {
		m.logf("backing store: %s", backing.Path)
	}

	// Lazy mode needs the placeholder marker, and the marker needs a filesystem
	// that holds user xattrs. Without it there is no placeholder record at all:
	// IsPlaceholder reads the marker and nothing else, so an unmarked placeholder
	// is an ordinary empty file to every guard in the tree, and the first write
	// through it pushes zeros over good remote content. The state DB does not
	// stand in — its hydration entries cache present ranges, not placeholder-ness.
	// This used to be a warning; it is a refusal because a mount that is unsafe
	// "full stop" should not come up, and before the provider opens because there
	// is no reason to launch a backend for a mount that cannot run.
	if spec.Lazy && !xattrsUsable(backing.Path) {
		return nil, fmt.Errorf("%s: -lazy needs a backing store that holds user extended attributes, and %s cannot hold %s: "+
			"without it an unfetched placeholder is indistinguishable from an empty file and can be uploaded over the remote copy. "+
			"Use eager mode, or move the backing store to a filesystem with user xattrs",
			spec.label(), backing.Path, hydrate.XattrName)
	}

	// Wire the provider if one was named; otherwise run log-only (M1 behaviour),
	// which needs no network or auth.
	if spec.Provider != "" {
		store, err := reg.Open(ctx, spec.Provider, provider.Params{
			Config: spec.ProviderConfig,
			Log:    m.logger(),
		})
		if err != nil {
			return nil, err
		}
		m.store = store
		if c, hasClose := store.(io.Closer); hasClose {
			m.closeStore = c.Close
		}
		m.logf("%s sync enabled", spec.Provider)

		// Engine-level sync state (cursor + echo records) lives in a control-plane
		// DB outside the backing tree so it isn't itself synced to the provider.
		//
		// Its directory is created here rather than left to bbolt, which creates
		// the file and not the path to it. The config path defaults the DB under
		// $XDG_STATE_HOME, which on a first run does not exist yet, so without
		// this the very first mount of a new config fails with "no such file or
		// directory". 0700 because the echo records name every path that has
		// ever synced.
		if dir := filepath.Dir(spec.StateDB); dir != "" {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, fmt.Errorf("creating the state directory: %w", err)
			}
		}
		st, err := state.Open(spec.StateDB)
		if err != nil {
			// Unwrapped: every error state.Open returns already names the operation
			// and the file, and App prefixes the mount's name on top, so a wrap here
			// produced "mount b: open state db: open state db /path: timeout".
			return nil, err
		}
		// Loud, because the consequences are visible to the user even though none
		// of them loses data: with no echo records nothing has a §4 baseline, so
		// the next sweep enumerates the whole tree, re-pushes and re-pulls what it
		// finds, and writes a §6 conflict copy for every path whose two sides
		// genuinely differ. Silence here would leave that looking like a bug.
		if aside := st.Recovered(); aside != "" {
			m.logf("state db %s was unreadable and has been replaced; the old file is at %s. "+
				"This mount has no sync history: it will re-enumerate the remote tree, and paths that "+
				"differ on both sides get a conflict copy. Nothing is deleted on the strength of a missing baseline.",
				spec.StateDB, aside)
		}
		m.state = st
	} else {
		m.logf("no provider configured: running log-only (no cloud sync)")
	}

	// Lazy hydration (M5). The hydrator is shared by all three consumers: the mount
	// backend faults content in on open, the downloader writes placeholders instead
	// of content, and the uploader consults it to avoid pushing a placeholder's
	// zeros over the real remote file.
	if spec.Lazy {
		m.hyd = hydrate.New(backing.Path, m.store, m.state, spec.HydrateWorkers)
		if spec.Xattr {
			// Passthrough publishes the placeholder marker at the mountpoint, where
			// stripping it from an unhydrated file makes the uploader push that file's
			// zeros over the remote copy. Nothing below the mount needs the
			// passthrough — the hydrator uses the backing path — so this pairing is
			// always a deliberate choice, and worth naming when it is made.
			m.logf("WARNING: xattrs are served through %s while lazy hydration is on; %s is readable and writable there, and removing it from a placeholder loses that file's remote content", spec.Mountpoint, hydrate.XattrName)
		}
		m.logf("lazy hydration enabled (ranged reads: %t)", m.hyd.SupportsRanges())
		m.mon.WatchHydration(m.hyd)
		m.hyd.ReportOutcomes(m.mon.Observe)
	}

	m.engine = syncengine.New(syncengine.Config{
		Store:    m.store,
		DataDir:  backing.Path,
		State:    m.state,
		Holes:    holesOf(m.hyd),
		Logger:   m.lg,
		Workers:  spec.UploadWorkers,
		Debounce: spec.PushDelay,
		Monitor:  m.mon,

		DrainTimeout: engineDrainTimeout,
	})

	// Inbound sync. A change feed (M3) is one way in and the M7b sweep is the
	// other, and a store needs only one of them: Drive has both, while every
	// filesystem backend in the M17–M21 group has only the sweep, which is then
	// the whole inbound path rather than a backstop under a feed. src stays nil in
	// that case and the downloader runs sweep-only.
	src, _ := provider.AsChangeSource(m.store)
	_, canEnum := provider.AsEnumerator(m.store)
	if src != nil || canEnum {
		// A previous run that never drained may have lost a push nothing recorded;
		// see state.Running for why that has to start a sweep. A read, zero commits.
		unclean, wasRunning, err := m.state.Running()
		if err != nil {
			m.logf("reading the unclean-shutdown mark: %v (treating the last run as clean)", err)
		}
		if !wasRunning {
			unclean = time.Time{}
		}
		dl := syncengine.NewDownloader(src, m.store, backing.Path, m.state, syncengine.DefaultCadence)
		if m.hyd != nil {
			dl = dl.Lazy(m.hyd)
		}
		// Initial enumeration & reconcile (M7b). The downloader owns it because it
		// owns the cursor, and the ordering rule that makes a sweep safe — take the
		// start token before the sweep, poll from it only after — is a statement
		// about the cursor. It runs off the FUSE path, so the mount comes up and
		// stays usable while a large remote is swept.
		m.down = dl.Logger(m.lg).Monitor(m.mon).PausedBy(m.engine).Reconcile(syncengine.ReconcileOptions{
			Push:       m.engine,
			Fetch:      spec.Materialize,
			Force:      spec.Resync,
			MaxDeletes: spec.MaxDeletes,
			Interval:   spec.SweepInterval,
			Unclean:    unclean,
			Writing:    m.writing,
		})
	}

	ok = true
	return m, nil
}

// Run starts the sync loops, mounts the filesystem, and blocks until ctx is
// cancelled (which unmounts) or the mount otherwise ends. It then drains the
// engine and returns. Call Close afterwards.
func (m *Mount) Run(ctx context.Context) error {
	// The engine's lifecycle is bounded by close(events), NOT by ctx: on SIGINT the
	// mount unmounts first (below), which flushes every pending FUSE event, and only
	// then do we close(events). Running the engine on a background context lets it
	// drain those buffered writes and its in-flight uploads (bounded internally)
	// instead of aborting the moment Ctrl-C cancels ctx. A second Ctrl-C hard-exits.
	// The unclean-shutdown mark goes down before anything can emit an event and
	// comes up only after both drains finished in time, so a run killed anywhere
	// in between leaves it set and the next start sweeps for what it lost. Only a
	// mount with an inbound side has anything to run that sweep.
	marked := false
	if m.down != nil {
		if err := m.state.SetRunning(time.Now()); err != nil {
			m.logf("setting the unclean-shutdown mark: %v (a crash in this run will not trigger a recovery sweep)", err)
		} else {
			marked = true
		}
	}
	inboundStopped := true

	engineDone := make(chan struct{})
	//nolint:gosec // G118: detaching from ctx is the entire point, per the note above.
	go func() {
		m.engine.Run(context.Background(), m.events)
		close(engineDone)
	}()
	// The engine runs detached so the drain can finish, which is also why it
	// cannot see the shutdown begin: with a provider that stopped answering, its
	// run loop sits in a send to a full worker queue, FUSE requests block in emit
	// behind it, and the unmount below never completes. Arming its deadline from
	// the request rather than from close(events) is what bounds that.
	go func() {
		select {
		case <-ctx.Done():
			m.engine.BeginShutdown()
		case <-engineDone:
		}
	}()

	var (
		downDone   chan struct{}
		downCancel context.CancelFunc
	)
	if m.down != nil {
		// Which of the two inbound paths this mount actually has, in its own terms.
		// "changes.list pull loop" was Drive's name for it and was printed for every
		// provider, including one that has no feed at all — where the sweep interval
		// IS the inbound latency and is the number the operator needs to see.
		switch _, hasFeed := provider.AsChangeSource(m.store); {
		case hasFeed:
			m.logf("inbound sync enabled (change-feed poll loop)")
		case m.spec.SweepInterval > 0:
			m.logf("inbound sync enabled (no change feed on this provider: enumerating every %s)", m.spec.SweepInterval)
		default:
			m.logf("inbound sync: one enumeration at startup only (no change feed on this provider, and sweep-interval is 0)")
		}
		// Its own cancellation, derived from ctx rather than ctx itself. The normal
		// shutdown has ctx cancelled already, but Serve also returns on a mount that
		// never came up — and there ctx is still live, so a loop waiting on it alone
		// would still be polling when the caller calls Close. The failed mount is the
		// case most likely to be retried immediately, too.
		downCtx, cancel := context.WithCancel(ctx)
		downCancel = cancel
		defer cancel()
		downDone = make(chan struct{})
		go func() {
			m.down.Run(downCtx)
			close(downDone)
		}()
	}

	backend := vfs.NewBackend()
	m.logf("mounting %s (%s backend, Ctrl-C to unmount)", m.spec.Mountpoint, backend.Name())
	err := backend.Serve(ctx, mount.Options{
		Mountpoint:     m.spec.Mountpoint,
		Backing:        m.backing.Path,
		Events:         m.events,
		FsName:         m.spec.fsName(),
		Debug:          m.spec.Debug,
		Xattr:          m.spec.Xattr,
		AllowOther:     m.spec.AllowOther,
		BackendOptions: m.spec.BackendOptions,
		Ready:          m.spec.Ready,
		Hydrator:       hydratorOf(m.hyd),
		Writing:        m.writing,
		Logger:         m.lg,
	})
	if err != nil {
		// Deliberately not fatal here. Serve returns after unmount as well as on
		// a mount failure, and in the first case the engine may still be holding
		// buffered uploads. Report the error, drain, then let the caller exit.
		err = fmt.Errorf("serve: %w", err)
	}
	// The inbound loop is part of the drain too, and waiting for it is a fix rather
	// than tidiness. Run's return is what lets the caller call Close, and Close
	// shuts the state DB and the backing dirfd — so a sweep still in flight found
	// its bbolt gone ("[sweep] delete pass: database not open"), which abandoned
	// the delete pass and skipped FinishSweep, leaving a completed sweep parked as
	// interrupted. The sharper version is in-place mode, where the backing path is
	// /proc/self/fd/N: once Close has closed that descriptor the number can be
	// reused, and a download still writing through it writes somewhere else
	// entirely. It is bounded for the same reason the engine's drain is — a
	// shutdown must end — and it goes before close(events) so a sweep's last
	// pushes are still drained by the engine below.
	if downDone != nil {
		downCancel()
		select {
		case <-downDone:
		case <-time.After(inboundDrainTimeout):
			m.logf("inbound sync did not stop within %s; continuing shutdown without it", inboundDrainTimeout)
			inboundStopped = false
		}
	}
	// Unmount has returned, so no more events will be emitted; closing the channel
	// tells the engine to drain its queue (bounded) and exit. Wait for that drain
	// so buffered uploads complete before the process exits (DESIGN.md §7).
	close(m.events)
	<-engineDone
	m.logf("sync engine drained")
	if marked && inboundStopped && m.engine.DrainedCleanly() {
		if cErr := m.state.ClearRunning(); cErr != nil {
			m.logf("clearing the unclean-shutdown mark: %v (the next start will run a recovery sweep it does not need)", cErr)
		}
	}
	return err
}

// Close releases what Open acquired, in reverse order, and is safe to call
// whether or not Run ever ran. It must not be called before Run returns: the
// backing dirfd is what the mount reads through.
func (m *Mount) Close() error {
	if m.closed {
		return nil
	}
	m.closed = true
	var errs []error
	if m.state != nil {
		errs = append(errs, m.state.Close())
	}
	if m.claim != nil {
		errs = append(errs, m.claim.Release())
	}
	if m.closeStore != nil {
		errs = append(errs, m.closeStore())
	}
	if m.backing != nil {
		errs = append(errs, m.backing.Close())
	}
	return errors.Join(errs...)
}

// holesOf and hydratorOf convert a possibly-nil *hydrate.Hydrator into the
// consumer-defined interfaces without handing over a non-nil interface wrapping a
// nil pointer — the classic Go trap that would make every "is lazy mode on?" check
// answer yes and every IsPlaceholder call panic.

func holesOf(h *hydrate.Hydrator) syncengine.Placeholders {
	if h == nil {
		return nil
	}
	return h
}

func hydratorOf(h *hydrate.Hydrator) mount.Hydrator {
	if h == nil {
		return nil
	}
	return h
}

// logger is m.lg with a non-nil guarantee, for the seams that require one.
func (m *Mount) logger() *log.Logger {
	if m.lg == nil {
		return log.Default()
	}
	return m.lg
}

// xattrsUsable is the lazy-mode probe, a variable so a test can stand in a
// backing store with no user xattrs — a filesystem the test machine may not have.
var xattrsUsable = hydrate.XattrsUsableIn

// engineDrainTimeout bounds the engine's shutdown drain, a variable so a test of
// a stuck shutdown need not wait out the production 30 seconds.
var engineDrainTimeout = syncengine.DefaultDrainTimeout

// claimBacking is the cross-process half of Validate's overlap rules (MC-47). It
// refuses a mount whose backing store, mountpoint or state DB sits inside the
// backing store of another drivel that is running — the shapes that multiply
// content without bound, or upload one client's sync history to the shared
// remote — and then claims this mount's own backing store, which also refuses a
// second process serving the same directory. A marker whose owner is no longer
// running is only a warning: it is an old directory, not a live mount, and the
// sweep's walk keeps such a subtree out of the sync either way.
//
// What it cannot see is the parent that starts second; that is the walk's job
// (syncengine pushLocalOnly), and the reason the marker exists at all.
func (m *Mount) claimBacking(spec MountSpec, backing *mount.Backing) error {
	root := spec.DataDir
	if backing.InPlace {
		root = spec.Mountpoint
	}
	checks := []struct{ what, dir string }{
		// Strictly above the backing root: a marker AT it is this mount's own,
		// from its last run.
		{"backing store " + root, filepath.Dir(root)},
	}
	if !backing.InPlace {
		checks = append(checks, struct{ what, dir string }{"mountpoint " + spec.Mountpoint, spec.Mountpoint})
	}
	if spec.StateDB != "" {
		checks = append(checks, struct{ what, dir string }{"state DB " + spec.StateDB, filepath.Dir(spec.StateDB)})
	}
	for _, c := range checks {
		found, ok := claim.Enclosing(c.dir)
		if !ok {
			continue
		}
		if found.Live {
			return fmt.Errorf("%s: the %s is inside %s, the backing store of the running drivel mount %q; "+
				"syncing it from here would sync that mount's files a second time, nested one level deeper on every sweep",
				spec.label(), c.what, found.Dir, found.Owner)
		}
		m.logf("note: the %s is inside %s, which was the backing store of drivel mount %q (not running now); "+
			"if that mount is started again, its sweep will leave this tree alone, but the two should not overlap",
			c.what, found.Dir, found.Owner)
	}

	c, err := claim.Acquire(backing.Path, spec.controlName())
	if errors.Is(err, claim.ErrHeld) {
		return fmt.Errorf("%s: backing store %s is already being served by another drivel process: %w", spec.label(), root, err)
	}
	if err != nil {
		// Not fatal: a backing store we cannot write a marker into is one the
		// mount will have its own trouble with, and it says so itself.
		m.logf("could not mark %s as this mount's backing store (%v); another drivel will not be able to tell", root, err)
		return nil
	}
	m.claim = c
	return nil
}
