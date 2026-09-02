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
// (reads, lookups, attrs, xattrs, ...) passes straight through to the backing store.
type node struct {
	fs.LoopbackNode
	events chan<- fsevent.Event
	hyd    mount.Hydrator // nil => eager mode; content is always resident
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
)

// NewRoot builds the root InodeEmbedder for a loopback mount backed by dir (which
// may be a /proc/self/fd/N path for in-place mounts). Every node created under it
// reports mutations on events. hyd may be nil (eager mode); when set, opening a
// file whose content is not resident faults it in first (M5).
func NewRoot(dir string, events chan<- fsevent.Event, hyd mount.Hydrator) (fs.InodeEmbedder, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(dir, &st); err != nil {
		return nil, err
	}
	root := &fs.LoopbackRoot{
		Path: dir,
		Dev:  uint64(st.Dev),
	}
	root.NewNode = func(rootData *fs.LoopbackRoot, _ *fs.Inode, _ string, _ *syscall.Stat_t) fs.InodeEmbedder {
		return &node{
			LoopbackNode: fs.LoopbackNode{RootData: rootData},
			events:       events,
			hyd:          hyd,
		}
	}
	return root.NewNode(root, nil, "", &st), nil
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
				log.Printf("[hydrate] discard %s: %v", p, err)
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
		h := &fileHandle{wrapped: fh, path: path, events: n.events, hyd: n.hyd}
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
			log.Printf("[hydrate] discard %s: %v", path, err)
			return nil, 0, syscall.EIO
		}
	}
	fh, fuseFlags, errno := n.LoopbackNode.Open(ctx, flags)
	if errno == 0 {
		fh = &fileHandle{wrapped: fh, path: path, events: n.events, hyd: n.hyd}
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
				log.Printf("[hydrate] setattr %s: %v", path, err)
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
