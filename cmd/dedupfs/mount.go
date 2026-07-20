package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/zishmusic/dedupfs/internal/provider"
	"github.com/zishmusic/dedupfs/internal/provider/gdrive"
	"github.com/zishmusic/dedupfs/internal/syncengine"
	"github.com/zishmusic/dedupfs/internal/vfs"
)

func runMount(args []string) {
	fset := flag.NewFlagSet("mount", flag.ExitOnError)
	mountpoint := fset.String("mount", "", "path to mount the filesystem (required)")
	dataDir := fset.String("data", "", "underlying directory: source of truth / cache (required)")
	credentials := fset.String("credentials", "", "OAuth client secret JSON; enables Drive sync (else log-only)")
	token := fset.String("token", "token.json", "path to the cached OAuth token (from 'dedupfs login')")
	driveRoot := fset.String("drive-root", "root", "Drive folder ID mapped to the mount root")
	debug := fset.Bool("debug", false, "enable FUSE debug logging")
	_ = fset.Parse(args)

	if *mountpoint == "" || *dataDir == "" {
		fset.Usage()
		log.Fatal("both -mount and -data are required")
	}
	for _, d := range []string{*dataDir, *mountpoint} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			log.Fatalf("creating %s: %v", d, err)
		}
	}

	// Mutations observed at the mount flow through this channel to the sync
	// engine. Buffered so brief FS bursts don't stall on the consumer.
	events := make(chan vfs.Event, 1024)

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
		DataDir:  *dataDir,
		RootID:   *driveRoot,
	})
	go engine.Run(ctx, events)

	root, err := vfs.NewRoot(*dataDir, events)
	if err != nil {
		log.Fatalf("building root: %v", err)
	}

	server, err := fs.Mount(*mountpoint, root, &fs.Options{
		MountOptions: fuse.MountOptions{
			Debug:  *debug,
			FsName: *dataDir,
			Name:   "dedupfs",
		},
	})
	if err != nil {
		log.Fatalf("mount %s: %v", *mountpoint, err)
	}
	log.Printf("mounted %s -> %s (Ctrl-C to unmount)", *mountpoint, *dataDir)

	// Unmount cleanly on signal, which makes server.Wait() return.
	go func() {
		<-ctx.Done()
		log.Println("unmounting...")
		if err := server.Unmount(); err != nil {
			log.Printf("unmount failed: %v (try: fusermount3 -u %s)", err, *mountpoint)
		}
	}()

	server.Wait()
	close(events)
}
