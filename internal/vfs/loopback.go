// Package vfs implements the FUSE interception layer: a loopback filesystem
// that proxies every operation to an underlying directory (the source of truth
// / local cache) and emits a change Event for each mutating operation so the
// sync engine can push those changes to the cloud provider.
//
// Milestone M1 captures node-level mutations (create/mkdir/rmdir/unlink/rename/
// setattr). Capturing file *content* writes precisely requires wrapping the
// file handle and is wired up in M2 alongside the uploader; see the OpWrite
// note below.
package vfs

import (
	"context"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// Op identifies the kind of filesystem mutation an Event describes.
type Op string

const (
	OpCreate  Op = "create"
	OpWrite   Op = "write" // reserved for M2 file-handle write capture
	OpMkdir   Op = "mkdir"
	OpRmdir   Op = "rmdir"
	OpUnlink  Op = "unlink"
	OpRename  Op = "rename"
	OpSetattr Op = "setattr"
)

// Event describes a single mutation observed at the mount, addressed by paths
// relative to the filesystem root (no leading slash). NewPath is set only for
// OpRename.
type Event struct {
	Op      Op
	Path    string
	NewPath string
}

// node is a loopback node that emits a change Event after each successful
// mutating operation. It embeds fs.LoopbackNode so all non-overridden behaviour
// (reads, lookups, attrs, xattrs, ...) passes straight through to the
// underlying directory.
type node struct {
	fs.LoopbackNode
	events chan<- Event
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

// NewRoot builds the root InodeEmbedder for a loopback mount rooted at dir.
// Every node created under it is a *node that reports mutations on events.
func NewRoot(dir string, events chan<- Event) (fs.InodeEmbedder, error) {
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
func (n *node) emit(ev Event) {
	if n.events != nil {
		n.events <- ev
	}
}

func (n *node) Create(ctx context.Context, name string, flags, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	inode, fh, ff, errno := n.LoopbackNode.Create(ctx, name, flags, mode, out)
	if errno == 0 {
		path := n.childPath(name)
		n.emit(Event{Op: OpCreate, Path: path})
		fh = &fileHandle{wrapped: fh, path: path, events: n.events}
	}
	return inode, fh, ff, errno
}

// Open wraps the loopback handle so content writes to an existing file surface as
// an OpWrite on close (see file.go). Reads pass straight through.
func (n *node) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	fh, fuseFlags, errno := n.LoopbackNode.Open(ctx, flags)
	if errno == 0 {
		fh = &fileHandle{wrapped: fh, path: n.Path(nil), events: n.events}
	}
	return fh, fuseFlags, errno
}

func (n *node) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	inode, errno := n.LoopbackNode.Mkdir(ctx, name, mode, out)
	if errno == 0 {
		n.emit(Event{Op: OpMkdir, Path: n.childPath(name)})
	}
	return inode, errno
}

func (n *node) Rmdir(ctx context.Context, name string) syscall.Errno {
	errno := n.LoopbackNode.Rmdir(ctx, name)
	if errno == 0 {
		n.emit(Event{Op: OpRmdir, Path: n.childPath(name)})
	}
	return errno
}

func (n *node) Unlink(ctx context.Context, name string) syscall.Errno {
	errno := n.LoopbackNode.Unlink(ctx, name)
	if errno == 0 {
		n.emit(Event{Op: OpUnlink, Path: n.childPath(name)})
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
		n.emit(Event{Op: OpRename, Path: n.childPath(name), NewPath: newPath})
	}
	return errno
}

func (n *node) Setattr(ctx context.Context, fh fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	errno := n.LoopbackNode.Setattr(ctx, fh, in, out)
	if errno == 0 {
		n.emit(Event{Op: OpSetattr, Path: n.Path(nil)})
	}
	return errno
}
