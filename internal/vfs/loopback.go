// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

// Package vfs is Drivel's go-fuse mount backend: a loopback filesystem that
// proxies every operation to a backing store (a directory, or /proc/self/fd/N in
// in-place mode) and emits an fsevent.Event for each mutating operation so the
// sync engine can push those changes to the cloud provider.
//
// It implements the mount.Backend seam (see backend.go); the change-event type
// lives in internal/fsevent so it isn't tied to go-fuse.
package vfs

import (
	"context"
	"log"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/zishmusic/drivel/internal/fsevent"
	"github.com/zishmusic/drivel/internal/mount"
)

// node is a loopback node that emits a change Event after each successful
// mutating operation. It embeds fs.LoopbackNode so all non-overridden behaviour
// (reads, lookups, attrs, ...) passes straight through to the backing store.
// Xattr operations are the exception: they are refused rather than proxied unless
// the mount opted in (mount.Options.Xattr), which takes a guard here as well as a
// server option — see xattr.go.
type node struct {
	// A pointer, not a value: WrapChild receives the *fs.LoopbackNode that
	// go-fuse already built for the child and we wrap that instance.
	*fs.LoopbackNode
	events chan<- fsevent.Event
	hyd    mount.Hydrator // nil => eager mode; content is always resident
	xattr  bool           // false => xattr ops are refused, not proxied (see xattr.go)
	lg     *log.Logger    // nil => the log package's default
}

// logf writes one line for this mount.
func (n *node) logf(format string, args ...any) {
	logTo(n.lg, format, args...)
}

// logTo is the shared nil-logger fallback for the node and its file handles.
func logTo(lg *log.Logger, format string, args ...any) {
	if lg == nil {
		log.Printf(format, args...)
		return
	}
	lg.Printf(format, args...)
}

// Interface assertions: these are the node capabilities we override. If a
// go-fuse API change drops one of these, the build breaks here rather than
// silently losing interception.
var (
	_ fs.NodeCreater   = (*node)(nil)
	_ fs.NodeOpener    = (*node)(nil)
	_ fs.NodeMkdirer   = (*node)(nil)
	_ fs.NodeRmdirer   = (*node)(nil)
	_ fs.NodeUnlinker  = (*node)(nil)
	_ fs.NodeRenamer   = (*node)(nil)
	_ fs.NodeSetattrer = (*node)(nil)

	// How children come to be nodes rather than plain LoopbackNodes.
	_ fs.NodeWrapChilder = (*node)(nil)
)

// WrapChild implements fs.NodeWrapChilder. go-fuse builds a plain
// *fs.LoopbackNode for every child it discovers and passes it here; wrapping it
// is what makes the child intercept mutations too.
//
// This replaces LoopbackRoot.NewNode, which is deprecated. The substantive
// difference is that wrapping is now driven by the parent node instead of by the
// root, so it is reached from NewInode on every creation path — Lookup, Create,
// Mkdir, Mknod, Symlink and Link alike.
func (n *node) WrapChild(_ context.Context, ops fs.InodeEmbedder) fs.InodeEmbedder {
	ln, ok := ops.(*fs.LoopbackNode)
	if !ok {
		// go-fuse only ever hands us what LoopbackRoot constructed. If that ever
		// changes, pass the child through unwrapped rather than dropping it: an
		// uninstrumented node loses sync events, a nil one loses the file.
		n.logf("vfs: unexpected child type %T; passing through uninstrumented", ops)
		return ops
	}
	return &node{LoopbackNode: ln, events: n.events, hyd: n.hyd, xattr: n.xattr, lg: n.lg}
}

// NewRoot builds the root InodeEmbedder for a loopback mount backed by
// opts.Backing (which may be a /proc/self/fd/N path for in-place mounts). Every
// node created under it reports mutations on opts.Events. opts.Hydrator may be
// nil (eager mode); when set, opening a file whose content is not resident faults
// it in first (M5).
//
// It takes the whole mount.Options rather than the four fields it reads so that a
// new option reaches the nodes without another positional parameter — the mount
// point and FsName are simply not the node layer's business.
func NewRoot(opts mount.Options) (fs.InodeEmbedder, error) {
	dir := opts.Backing
	var st syscall.Stat_t
	if err := syscall.Stat(dir, &st); err != nil {
		return nil, err
	}
	root := &fs.LoopbackRoot{
		Path: dir,
		// Required: syscall.Stat_t.Dev is int32 on darwin, uint64 on linux.
		Dev: uint64(st.Dev), //nolint:unconvert // not redundant off linux
	}
	// The root is the one node go-fuse does not create for us, so build it here;
	// every descendant arrives through WrapChild above.
	rootNode := &node{
		LoopbackNode: &fs.LoopbackNode{RootData: root},
		events:       opts.Events,
		hyd:          opts.Hydrator,
		xattr:        opts.Xattr,
		lg:           opts.Logger,
	}
	// Mirrors NewLoopbackRoot: relative-path computation prefers this over
	// walking up to the FUSE mount root.
	root.RootNode = rootNode
	return rootNode, nil
}

// childPath returns the root-relative path of a child named name under n.
func (n *node) childPath(name string) string {
	p := n.Path(nil)
	if p == "" {
		return name
	}
	return p + "/" + name
}

// emit sends an Event, applying backpressure (blocking) rather than dropping:
// losing a mutation would mean losing a sync operation.
func (n *node) emit(ev fsevent.Event) {
	if n.events != nil {
		n.events <- ev
	}
}

func (n *node) Create(ctx context.Context, name string, flags, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	// Creating over an existing placeholder replaces its content; drop the mark so
	// the (now genuinely local) file is no longer treated as unhydrated.
	if n.hyd != nil {
		if p := n.childPath(name); n.hyd.IsPlaceholder(p) {
			if err := n.hyd.Discard(p); err != nil {
				n.logf("[hydrate] discard %s: %v", p, err)
				return nil, nil, 0, syscall.EIO
			}
		}
	}
	inode, fh, ff, errno := n.LoopbackNode.Create(ctx, name, flags, mode, out)
	if errno == 0 {
		path := n.childPath(name)
		n.emit(fsevent.Event{Op: fsevent.OpCreate, Path: path})
		// The file was just created or truncated, so its content is resident by
		// definition — mark the handle so no read on it tries to fault anything in.
		h := &fileHandle{wrapped: fh, path: path, events: n.events, hyd: n.hyd, lg: n.lg}
		h.resident.Store(true)
		fh = h
	}
	return inode, fh, ff, errno
}

// Open wraps the loopback handle so content writes to an existing file surface as
// an OpWrite on close (see file.go). Reads pass straight through.
//
// In lazy mode (M5) opening does NOT hydrate — the handle faults content in on its
// first Read or Write instead (see file.go). Deferring that far is what makes
// "open, truncate, rewrite" free: nothing is fetched for content that is about to
// be discarded. Opening a whole directory tree therefore costs no downloads.
//
// The one thing that must happen here is dropping the mark on a truncating open.
// The kernel usually delivers O_TRUNC as a separate Setattr, but when
// atomic_o_trunc is negotiated it arrives on the open itself — and the loopback
// Open below would then zero the backing file while it is still marked a
// placeholder, so a later read would helpfully restore the content the caller just
// truncated away.
func (n *node) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	path := n.Path(nil)
	if n.hyd != nil && flags&syscall.O_TRUNC != 0 && n.hyd.IsPlaceholder(path) {
		if err := n.hyd.Discard(path); err != nil {
			n.logf("[hydrate] discard %s: %v", path, err)
			return nil, 0, syscall.EIO
		}
	}
	// The backing file is opened readable even when the client asked for
	// write-only, because a kernel may read through a write handle. FreeBSD's
	// fusefs fills a buffer-cache block before writing part of it, and
	// fuse_io_strategy deliberately falls back to the write filehandle for that
	// read-modify-write when no read handle is open — so a READ arrives on a handle
	// opened O_WRONLY. Passing the flags straight through leaves the backing fd
	// write-only, go-fuse preads it to serialise the fd-backed ReadResult in its
	// !linux reply path (server_unix.go in the pinned v2.10.1), and the EBADF that
	// pread returns comes back to the caller as the *write* failing — after a
	// -debug trace has already logged the READ as OK, since the read happens when
	// the reply is written rather than when it is built. Linux does not RMW through
	// the write handle, so nothing there depends on this; it is one body rather
	// than a build-tagged FreeBSD delta so that a Linux CI run covers the path
	// FreeBSD needs, the same argument hydrate/xattr_unix.go makes.
	//
	// The fallback is not decoration: read permission is not implied by write
	// permission, so a backing file this process may write and not read (mode 0222,
	// or an ACL) must still open exactly as the caller asked.
	openFlags := flags
	if flags&uint32(syscall.O_ACCMODE) == uint32(syscall.O_WRONLY) {
		openFlags = flags&^uint32(syscall.O_ACCMODE) | uint32(syscall.O_RDWR)
	}
	fh, fuseFlags, errno := n.LoopbackNode.Open(ctx, openFlags)
	if errno != 0 && openFlags != flags {
		fh, fuseFlags, errno = n.LoopbackNode.Open(ctx, flags)
	}
	if errno == 0 {
		fh = &fileHandle{wrapped: fh, path: path, events: n.events, hyd: n.hyd, lg: n.lg}
	}
	return fh, fuseFlags, errno
}

func (n *node) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	inode, errno := n.LoopbackNode.Mkdir(ctx, name, mode, out)
	if errno == 0 {
		n.emit(fsevent.Event{Op: fsevent.OpMkdir, Path: n.childPath(name)})
	}
	return inode, errno
}

func (n *node) Rmdir(ctx context.Context, name string) syscall.Errno {
	errno := n.LoopbackNode.Rmdir(ctx, name)
	if errno == 0 {
		n.emit(fsevent.Event{Op: fsevent.OpRmdir, Path: n.childPath(name)})
	}
	return errno
}

func (n *node) Unlink(ctx context.Context, name string) syscall.Errno {
	errno := n.LoopbackNode.Unlink(ctx, name)
	if errno == 0 {
		n.emit(fsevent.Event{Op: fsevent.OpUnlink, Path: n.childPath(name)})
	}
	return errno
}

func (n *node) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	errno := n.LoopbackNode.Rename(ctx, name, newParent, newName, flags)
	if errno == 0 {
		newPath := newName
		if p, ok := newParent.(*node); ok {
			newPath = p.childPath(newName)
		}
		n.emit(fsevent.Event{Op: fsevent.OpRename, Path: n.childPath(name), NewPath: newPath})
	}
	return errno
}

func (n *node) Setattr(ctx context.Context, fh fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	// A truncate on a placeholder needs care: shrinking to a non-zero length must
	// keep the real prefix, so the content has to be resident first. Truncating to
	// zero discards everything anyway, so it only needs the mark dropped.
	path := n.Path(nil)
	if n.hyd != nil && n.hyd.IsPlaceholder(path) {
		if sz, ok := in.GetSize(); ok {
			var err error
			if sz == 0 {
				err = n.hyd.Discard(path)
			} else {
				err = n.hyd.Hydrate(ctx, path)
			}
			if err != nil {
				n.logf("[hydrate] setattr %s: %v", path, err)
				return syscall.EIO
			}
		}
	}
	errno := n.LoopbackNode.Setattr(ctx, fh, in, out)
	if errno == 0 {
		n.emit(fsevent.Event{Op: fsevent.OpSetattr, Path: path})
	}
	return errno
}
