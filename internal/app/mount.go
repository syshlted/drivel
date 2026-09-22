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

	"github.com/zishmusic/drivel/internal/fsevent"
	"github.com/zishmusic/drivel/internal/hydrate"
	"github.com/zishmusic/drivel/internal/mount"
	"github.com/zishmusic/drivel/internal/state"
	"github.com/zishmusic/drivel/internal/syncengine"
	"github.com/zishmusic/drivel/internal/vfs"
	"github.com/zishmusic/drivel/provider"
)

// eventBuffer is how many mount events may be in flight before the FUSE handler
// blocks on the sync engine. Buffered so brief FS bursts don't stall on the
// consumer; per mount, since one busy mount must not stall another.
const eventBuffer = 1024

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

	// closeStore is the provider's release hook, if it has one. provider.Store
	// says nothing about closing — most backends have nothing to release — so a
	// store that does announces it by implementing io.Closer.
	closeStore func() error

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

	m := &Mount{spec: spec, lg: spec.Logger, events: make(chan fsevent.Event, eventBuffer)}
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
	if backing.InPlace {
		m.logf("in-place mode: %s is its own backing store (via %s)", spec.Mountpoint, backing.Path)
	} else {
		m.logf("backing store: %s", backing.Path)
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
		st, err := state.Open(spec.StateDB)
		if err != nil {
			return nil, fmt.Errorf("open state db: %w", err)
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
		if !m.hyd.XattrsUsable() {
			// Without the xattr there is no placeholder record at all: IsPlaceholder
			// reads the marker and nothing else, so an unmarked placeholder is an
			// ordinary empty file to every guard in the tree and the uploader will
			// push its zeros over good remote content. The state DB does not stand in
			// — its hydration entries cache present ranges, not placeholder-ness — so
			// the honest advice is not "keep the DB" but "do not run -lazy here".
			m.logf("WARNING: %s cannot hold user xattrs natively, so %s cannot be recorded and -lazy is UNSAFE on this backing store: an un-fetched placeholder is indistinguishable from an empty file and may be uploaded over the remote copy. Use eager mode, or move -data to a filesystem with user extended attributes.", backing.Path, hydrate.XattrName)
		}
		if spec.Xattr {
			// Passthrough publishes the placeholder marker at the mountpoint, where
			// stripping it from an unhydrated file makes the uploader push that file's
			// zeros over the remote copy. Nothing below the mount needs the
			// passthrough — the hydrator uses the backing path — so this pairing is
			// always a deliberate choice, and worth naming when it is made.
			m.logf("WARNING: xattrs are served through %s while lazy hydration is on; %s is readable and writable there, and removing it from a placeholder loses that file's remote content", spec.Mountpoint, hydrate.XattrName)
		}
		m.logf("lazy hydration enabled (ranged reads: %t)", m.hyd.SupportsRanges())
	}

	m.engine = syncengine.New(syncengine.Config{
		Store:    m.store,
		DataDir:  backing.Path,
		State:    m.state,
		Holes:    holesOf(m.hyd),
		Logger:   m.lg,
		Workers:  spec.UploadWorkers,
		Debounce: spec.PushDelay,
	})

	// Inbound sync. A change feed (M3) is one way in and the M7b sweep is the
	// other, and a store needs only one of them: Drive has both, while every
	// filesystem backend in the M17–M21 group has only the sweep, which is then
	// the whole inbound path rather than a backstop under a feed. src stays nil in
	// that case and the downloader runs sweep-only.
	src, _ := provider.AsChangeSource(m.store)
	_, canEnum := provider.AsEnumerator(m.store)
	if src != nil || canEnum {
		dl := syncengine.NewDownloader(src, m.store, backing.Path, m.state, syncengine.DefaultCadence)
		if m.hyd != nil {
			dl = dl.Lazy(m.hyd)
		}
		// Initial enumeration & reconcile (M7b). The downloader owns it because it
		// owns the cursor, and the ordering rule that makes a sweep safe — take the
		// start token before the sweep, poll from it only after — is a statement
		// about the cursor. It runs off the FUSE path, so the mount comes up and
		// stays usable while a large remote is swept.
		m.down = dl.Logger(m.lg).Reconcile(syncengine.ReconcileOptions{
			Push:       m.engine,
			Fetch:      spec.Materialize,
			Force:      spec.Resync,
			MaxDeletes: spec.MaxDeletes,
			Interval:   spec.SweepInterval,
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
	engineDone := make(chan struct{})
	//nolint:gosec // G118: detaching from ctx is the entire point, per the note above.
	go func() {
		m.engine.Run(context.Background(), m.events)
		close(engineDone)
	}()

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
		go m.down.Run(ctx)
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
		Logger:         m.lg,
	})
	if err != nil {
		// Deliberately not fatal here. Serve returns after unmount as well as on
		// a mount failure, and in the first case the engine may still be holding
		// buffered uploads. Report the error, drain, then let the caller exit.
		err = fmt.Errorf("serve: %w", err)
	}
	// Unmount has returned, so no more events will be emitted; closing the channel
	// tells the engine to drain its queue (bounded) and exit. Wait for that drain
	// so buffered uploads complete before the process exits (DESIGN.md §7).
	close(m.events)
	<-engineDone
	m.logf("sync engine drained")
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
