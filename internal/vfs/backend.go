// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package vfs

import (
	"context"
	"fmt"
	"log"
	"time"

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
			// flock(2) only, never POSIX locks: a BSD lock taken through the mount
			// is forwarded to the backing file (fileHandle.Setlk), so a process
			// holding one — bbolt does, on every database it has open for writing —
			// is seen by the engine's claim.HeldByAnother and is not uploaded
			// mid-transaction, which a lock the kernel kept to itself never was.
			// flock is per open file description and so is a FUSE file handle, so
			// forwarding it changes nothing about its semantics. POSIX locks are
			// per process and released by any close; go-fuse's loopback answers
			// them with OFD locks on one descriptor, which gets both wrong, so the
			// kernel goes on keeping those itself (EnableLocks would forward both).
			// Only a kernel that offers the capability gets it.
			ExtraCapabilities: fuse.CAP_FLOCK_LOCKS,
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

	// Unmount on cancellation — but only while the mount is still ours to
	// unmount. Waiting on ctx alone meant a mount ended from outside (umount(8),
	// `systemctl stop`, which is how every fstab mount ends) left this goroutine
	// behind until the process exited, and then had it unmount a filesystem that
	// was already gone: "unmount … failed: Invalid argument", on stdout, after a
	// shutdown that had gone perfectly well.
	//
	// And retried while it is still ours, because one failure is not final: the
	// commonest EBUSY is a FUSE request still in flight — a write blocked in emit
	// while the engine sheds its queues at the shutdown deadline — which clears
	// by itself a moment later. Trying once left server.Wait waiting forever on a
	// mount nothing would unmount. A mountpoint some process is sitting in stays
	// busy until it leaves; retrying then costs one fusermount a second, said
	// once, and unmounts the moment it can.
	served := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
		case <-served:
			return
		}
		lg := opts.Logger
		if lg == nil {
			lg = log.Default()
		}
		for attempt := 1; ; attempt++ {
			err := server.Unmount()
			if err == nil {
				if attempt > 1 {
					lg.Printf("unmounted %s on attempt %d", opts.Mountpoint, attempt)
				}
				return
			}
			if attempt == 1 {
				lg.Printf("unmount %s failed: %v (retrying every %s; if it stays busy, find what is using it: fuser -vm %s)",
					opts.Mountpoint, err, unmountRetry, opts.Mountpoint)
			}
			select {
			case <-served:
				return // unmounted from outside meanwhile
			case <-time.After(unmountRetry):
			}
		}
	}()

	server.Wait()
	close(served)
	return nil
}

// unmountRetry is how often Serve retries an unmount that failed at shutdown.
const unmountRetry = time.Second
