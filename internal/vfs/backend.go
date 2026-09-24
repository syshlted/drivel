// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package vfs

import (
	"context"
	"fmt"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/syshlted/drivel/internal/mount"
)

// backend is the go-fuse implementation of mount.Backend.
type backend struct{}

// NewBackend returns the go-fuse mount backend (Linux/macOS/FreeBSD).
func NewBackend() mount.Backend { return backend{} }

func (backend) Name() string { return "go-fuse" }

// Serve mounts the loopback filesystem and blocks until ctx is cancelled, which
// triggers an unmount and makes the server's Wait return.
func (backend) Serve(ctx context.Context, opts mount.Options) error {
	root, err := NewRoot(opts)
	if err != nil {
		return fmt.Errorf("building root from %s: %w", opts.Backing, err)
	}
	server, err := fs.Mount(opts.Mountpoint, root, &fs.Options{
		MountOptions: fuse.MountOptions{
			Debug:  opts.Debug,
			FsName: opts.FsName,
			Name:   "drivel",
			// Xattrs are off unless the mount asked for them (mount.Options.Xattr).
			// go-fuse answers ENOSYS to the first getxattr, after which the kernel
			// stops issuing xattr operations for this mount at all — so the cost of
			// the default is one syscall, not one per file. See the field comment
			// for why the passthrough is the dangerous direction.
			DisableXAttrs: !opts.Xattr,
			AllowOther:    opts.AllowOther,
			// Caller options, then the compulsory ones — the order is the
			// guarantee, not a detail. go-fuse turns nodev/nosuid/noexec into
			// mount(2) flags and passes the rest to the mount helper, which rejects
			// what it does not know, so an unsupported option fails the mount rather
			// than quietly weakening it.
			Options: withCompulsory(opts.BackendOptions),
		},
	})
	if err != nil {
		return fmt.Errorf("mount %s: %w", opts.Mountpoint, err)
	}

	// The filesystem is live from here: fs.Mount returns after the mount syscall
	// has completed, so anything waiting to hear "mounted" can be told now. Before
	// Wait, which does not return until unmount.
	if opts.Ready != nil {
		opts.Ready()
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
