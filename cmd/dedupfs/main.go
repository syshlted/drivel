// Command dedupfs mounts a loopback FUSE filesystem that proxies operations to
// an underlying directory and (from M2 onward) syncs that directory with a
// cloud-storage provider. See DESIGN.md for the architecture.
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

	"github.com/zishmusic/dedupfs/internal/syncengine"
	"github.com/zishmusic/dedupfs/internal/vfs"
)

func main() {
	mountpoint := flag.String("mount", "", "path to mount the filesystem (required)")
	dataDir := flag.String("data", "", "underlying directory: source of truth / cache (required)")
	debug := flag.Bool("debug", false, "enable FUSE debug logging")
	flag.Parse()

	if *mountpoint == "" || *dataDir == "" {
		flag.Usage()
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

	engine := syncengine.New()
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
			log.Printf("unmount failed: %v (try: fusermount -u %s)", err, *mountpoint)
		}
	}()

	server.Wait()
	close(events)
}
