package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/zishmusic/drivel/internal/fsevent"
	"github.com/zishmusic/drivel/internal/hydrate"
	"github.com/zishmusic/drivel/internal/mount"
	"github.com/zishmusic/drivel/internal/provider"
	"github.com/zishmusic/drivel/internal/provider/gdrive"
	"github.com/zishmusic/drivel/internal/state"
	"github.com/zishmusic/drivel/internal/syncengine"
	"github.com/zishmusic/drivel/internal/vfs"
)

func runMount(args []string) error {
	fset := flag.NewFlagSet("mount", flag.ExitOnError)
	mountpoint := fset.String("mount", "", "path to mount the filesystem (required)")
	dataDir := fset.String("data", "", "backing directory (source of truth). If omitted, in-place mode uses the mount dir as its own backing (Linux only)")
	credentials := fset.String("credentials", "", "OAuth client secret JSON; enables Drive sync (else log-only)")
	token := fset.String("token", "token.json", "path to the cached OAuth token (from 'drivel login')")
	stateDB := fset.String("state", "drivel-state.db", "path to the sync-state DB (cursor + echo records); kept outside the backing tree")
	indexDB := fset.String("index", "drivel-index.db", "path to the provider's path↔ID index (a cache; safe to delete); \"\" disables it")
	driveRoot := fset.String("drive-root", "root", "Drive folder ID mapped to the mount root")
	lazy := fset.Bool("lazy", false, "lazy hydration (M5): materialise remote files as placeholders and fetch content on first read (requires -credentials)")
	resync := fset.Bool("resync", false, "enumerate the whole remote tree and reconcile it against the backing dir at startup, even if a baseline already exists")
	materialize := fset.Bool("materialize", false, "during a reconcile in eager mode, download remote files that have no local copy (implied by -lazy, where it costs only a placeholder)")
	maxDeletes := fset.Int("max-deletes", syncengine.DefaultMaxDeletes, "cap on deletions one reconcile may infer, in either direction; 0 for no limit")
	sweepInterval := fset.Duration("sweep-interval", syncengine.DefaultSweepInterval, "re-enumerate and reconcile the remote tree this often, timed from the last completed sweep; 0 disables it")
	debug := fset.Bool("debug", false, "enable FUSE debug logging")
	_ = fset.Parse(args)

	if *mountpoint == "" {
		fset.Usage()
		return errors.New("-mount is required")
	}
	if *lazy && *credentials == "" {
		// A placeholder is a promise that the bytes can be fetched later; without a
		// provider there is nothing to redeem it against.
		return errors.New("-lazy requires -credentials (there is nothing to hydrate from)")
	}
	if err := os.MkdirAll(*mountpoint, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", *mountpoint, err)
	}
	if *dataDir != "" {
		if err := os.MkdirAll(*dataDir, 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", *dataDir, err)
		}
	}

	// Resolve the backing store. In in-place mode this opens a dirfd to the
	// mountpoint BEFORE we mount over it, so both the FUSE backend and the sync
	// engine reach the underlying directory (via /proc/self/fd/N) instead of
	// recursing through the overlay. Held open until after unmount.
	backing, err := mount.ResolveBacking(*mountpoint, *dataDir)
	if err != nil {
		return err
	}
	defer backing.Close()
	if backing.InPlace {
		log.Printf("in-place mode: %s is its own backing store (via %s)", *mountpoint, backing.Path)
	} else {
		log.Printf("backing store: %s", backing.Path)
	}

	// Mutations observed at the mount flow through this channel to the sync
	// engine. Buffered so brief FS bursts don't stall on the consumer.
	events := make(chan fsevent.Event, 1024)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Wire the Drive provider if credentials were supplied; otherwise run
	// log-only (M1 behaviour), which needs no network or auth.
	var (
		store provider.Store
		st    *state.Store
	)
	if *credentials != "" {
		d, err := gdrive.Open(ctx, gdrive.Config{
			Credentials: *credentials,
			Token:       *token,
			RootID:      *driveRoot,
			IndexPath:   *indexDB,
		})
		if err != nil {
			return fmt.Errorf("google drive auth: %w", err)
		}
		defer d.Close()
		store = d
		log.Printf("google drive sync enabled (root folder %s)", *driveRoot)

		// Engine-level sync state (cursor + echo records) lives in a control-plane
		// DB outside the backing tree so it isn't itself synced to Drive.
		st, err = state.Open(*stateDB)
		if err != nil {
			return fmt.Errorf("open state db: %w", err)
		}
		defer st.Close()
	} else {
		log.Print("no -credentials: running in log-only mode (no cloud sync)")
	}

	// Lazy hydration (M5). The hydrator is shared by all three consumers: the mount
	// backend faults content in on open, the downloader writes placeholders instead
	// of content, and the uploader consults it to avoid pushing a placeholder's
	// zeros over the real remote file.
	var hyd *hydrate.Hydrator
	if *lazy {
		hyd = hydrate.New(backing.Path, store, st)
		if !hyd.XattrsUsable() {
			// Without xattrs the placeholder marker lives only in the state DB, so
			// losing that DB makes placeholders look like empty files — which the
			// uploader would then push over good remote content.
			log.Printf("WARNING: %s cannot store user xattrs; placeholder marks rely on the state DB alone (%s). Do not delete it while placeholders exist.", backing.Path, *stateDB)
		}
		log.Printf("lazy hydration enabled (ranged reads: %t)", hyd.SupportsRanges())
	}

	engine := syncengine.New(syncengine.Config{
		Store:   store,
		DataDir: backing.Path,
		State:   st,
		Holes:   holesOf(hyd),
	})
	// The engine's lifecycle is bounded by close(events), NOT by ctx: on SIGINT the
	// mount unmounts first (below), which flushes every pending FUSE event, and only
	// then do we close(events). Running the engine on a background context lets it
	// drain those buffered writes and its in-flight uploads (bounded internally)
	// instead of aborting the moment Ctrl-C cancels ctx. A second Ctrl-C hard-exits.
	engineDone := make(chan struct{})
	go func() {
		engine.Run(context.Background(), events)
		close(engineDone)
	}()

	// Inbound pull loop (M3), only if the store offers a change feed.
	if src, ok := store.(provider.ChangeSource); ok {
		dl := syncengine.NewDownloader(src, store, backing.Path, st, syncengine.DefaultCadence)
		if hyd != nil {
			dl = dl.Lazy(hyd)
		}
		// Initial enumeration & reconcile (M7b). The downloader owns it because it
		// owns the cursor, and the ordering rule that makes a sweep safe — take the
		// start token before the sweep, poll from it only after — is a statement
		// about the cursor. It runs in this goroutine, off the FUSE path, so the
		// mount below comes up and stays usable while a large Drive is swept.
		dl = dl.Reconcile(syncengine.ReconcileOptions{
			Push:       engine,
			Fetch:      *materialize,
			Force:      *resync,
			MaxDeletes: *maxDeletes,
			Interval:   *sweepInterval,
		})
		log.Print("inbound sync enabled (changes.list pull loop)")
		go dl.Run(ctx)
	}

	backend := vfs.NewBackend()
	log.Printf("mounting %s (%s backend, Ctrl-C to unmount)", *mountpoint, backend.Name())
	err = backend.Serve(ctx, mount.Options{
		Mountpoint: *mountpoint,
		Backing:    backing.Path,
		Events:     events,
		FsName:     "drivel",
		Debug:      *debug,
		Hydrator:   hydratorOf(hyd),
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
	close(events)
	<-engineDone
	log.Print("sync engine drained; exiting")
	return err
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
