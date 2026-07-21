package vfs

import (
	"context"
	"fmt"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/zishmusic/drivel/internal/mount"
)

// backend is the go-fuse implementation of mount.Backend.
type backend struct{}

// NewBackend returns the go-fuse mount backend (Linux/macOS/FreeBSD).
func NewBackend() mount.Backend { return backend{} }

func (backend) Name() string { return "go-fuse" }

// Serve mounts the loopback filesystem and blocks until ctx is cancelled, which
// triggers an unmount and makes the server's Wait return.
func (backend) Serve(ctx context.Context, opts mount.Options) error {
	root, err := NewRoot(opts.Backing, opts.Events)
	if err != nil {
		return fmt.Errorf("building root from %s: %w", opts.Backing, err)
	}
	server, err := fs.Mount(opts.Mountpoint, root, &fs.Options{
		MountOptions: fuse.MountOptions{
			Debug:  opts.Debug,
			FsName: opts.FsName,
			Name:   "drivel",
		},
	})
	if err != nil {
		return fmt.Errorf("mount %s: %w", opts.Mountpoint, err)
	}

	go func() {
		<-ctx.Done()
		if err := server.Unmount(); err != nil {
			// Unmount can fail if the mount is busy; the process is shutting down
			// anyway. Surface a hint for a manual cleanup.
			fmt.Printf("unmount %s failed: %v (try: fusermount3 -u %s)\n", opts.Mountpoint, err, opts.Mountpoint)
		}
	}()

	server.Wait()
	return nil
}
