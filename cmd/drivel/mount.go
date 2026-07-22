package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/zishmusic/drivel/internal/fsevent"
	"github.com/zishmusic/drivel/internal/mount"
	"github.com/zishmusic/drivel/internal/provider"
	"github.com/zishmusic/drivel/internal/provider/gdrive"
	"github.com/zishmusic/drivel/internal/state"
	"github.com/zishmusic/drivel/internal/syncengine"
	"github.com/zishmusic/drivel/internal/vfs"
)

func runMount(args []string) {
	fset := flag.NewFlagSet("mount", flag.ExitOnError)
	mountpoint := fset.String("mount", "", "path to mount the filesystem (required)")
	dataDir := fset.String("data", "", "backing directory (source of truth). If omitted, in-place mode uses the mount dir as its own backing (Linux only)")
	credentials := fset.String("credentials", "", "OAuth client secret JSON; enables Drive sync (else log-only)")
	token := fset.String("token", "token.json", "path to the cached OAuth token (from 'drivel login')")
	stateDB := fset.String("state", "drivel-state.db", "path to the sync-state DB (cursor + echo records); kept outside the backing tree")
	driveRoot := fset.String("drive-root", "root", "Drive folder ID mapped to the mount root")
	debug := fset.Bool("debug", false, "enable FUSE debug logging")
	_ = fset.Parse(args)

	if *mountpoint == "" {
		fset.Usage()
		log.Fatal("-mount is required")
	}
	if err := os.MkdirAll(*mountpoint, 0o755); err != nil {
		log.Fatalf("creating %s: %v", *mountpoint, err)
	}
	if *dataDir != "" {
		if err := os.MkdirAll(*dataDir, 0o755); err != nil {
			log.Fatalf("creating %s: %v", *dataDir, err)
		}
	}

	// Resolve the backing store. In in-place mode this opens a dirfd to the
	// mountpoint BEFORE we mount over it, so both the FUSE backend and the sync
	// engine reach the underlying directory (via /proc/self/fd/N) instead of
	// recursing through the overlay. Held open until after unmount.
	backing, err := mount.ResolveBacking(*mountpoint, *dataDir)
	if err != nil {
		log.Fatal(err)
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
		d, err := gdrive.Open(ctx, *credentials, *token, *driveRoot)
		if err != nil {
			log.Fatalf("google drive auth: %v", err)
		}
		defer d.Close()
		store = d
		log.Printf("google drive sync enabled (root folder %s)", *driveRoot)

		// Engine-level sync state (cursor + echo records) lives in a control-plane
		// DB outside the backing tree so it isn't itself synced to Drive.
		st, err = state.Open(*stateDB)
		if err != nil {
			log.Fatalf("open state db: %v", err)
		}
		defer st.Close()
	} else {
		log.Print("no -credentials: running in log-only mode (no cloud sync)")
	}

	engine := syncengine.New(syncengine.Config{
		Store:   store,
		DataDir: backing.Path,
		State:   st,
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
	})
	if err != nil {
		log.Fatalf("serve: %v", err)
	}
	// Unmount has returned, so no more events will be emitted; closing the channel
	// tells the engine to drain its queue (bounded) and exit. Wait for that drain
	// so buffered uploads complete before the process exits (DESIGN.md §7).
	close(events)
	<-engineDone
	log.Print("sync engine drained; exiting")
}
