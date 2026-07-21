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
	"github.com/zishmusic/drivel/internal/syncengine"
	"github.com/zishmusic/drivel/internal/vfs"
)

func runMount(args []string) {
	fset := flag.NewFlagSet("mount", flag.ExitOnError)
	mountpoint := fset.String("mount", "", "path to mount the filesystem (required)")
	dataDir := fset.String("data", "", "backing directory (source of truth). If omitted, in-place mode uses the mount dir as its own backing (Linux only)")
	credentials := fset.String("credentials", "", "OAuth client secret JSON; enables Drive sync (else log-only)")
	token := fset.String("token", "token.json", "path to the cached OAuth token (from 'drivel login')")
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
	var prov provider.Provider
	if *credentials != "" {
		d, err := gdrive.Open(ctx, *credentials, *token)
		if err != nil {
			log.Fatalf("google drive auth: %v", err)
		}
		defer d.Close()
		prov = d
		log.Printf("google drive sync enabled (root folder %s)", *driveRoot)
	} else {
		log.Print("no -credentials: running in log-only mode (no cloud sync)")
	}

	engine := syncengine.New(syncengine.Config{
		Provider: prov,
		DataDir:  backing.Path,
		RootID:   *driveRoot,
	})
	go engine.Run(ctx, events)

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
	close(events)
}
