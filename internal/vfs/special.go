// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package vfs

import (
	"context"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/zishmusic/drivel/internal/fsevent"
)

// Hard links, symlinks and special files: what the mount does with the things a
// path-addressed cloud store has no way to hold (DESIGN.md §9, M15 items 2 and 3).
//
// Before M15 none of Link, Symlink or Mknod was overridden, so all three fell
// through to fs.LoopbackNode: they were created correctly in the backing store and
// emitted no fsevent at all. Symlinks and special files staying local is the right
// answer and is kept. Hard links are not — see Link below — and the silence was
// wrong in every case, because a cost the user cannot see is one they report as a
// bug. The skip is now said out loud at both sites that decide it: here, when the
// mount declines to emit, and in the sweep's local walk, when it steps over the
// same file (syncengine/reconcile.go).

var (
	_ fs.NodeLinker    = (*node)(nil)
	_ fs.NodeSymlinker = (*node)(nil)
	_ fs.NodeMknoder   = (*node)(nil)
)

// Link refuses to create a hard link.
//
// No provider on the roadmap can represent one, and there is no benefit in
// inventing a representation: what a hard link buys — two names, one inode, one
// copy of the bytes — is precisely what a path-addressed remote cannot express.
//
// Refusing is strictly safer than the fall-through it replaces. A link created in
// the backing store emits no event, but both names are then *regular files*, so
// the sweep's local walk pushes both, as two independent remote objects that
// diverge from each other from the first write onwards. The user was told the
// link succeeded and gets two files that silently stop agreeing.
//
// EPERM rather than ENOSYS because link(2) documents it for exactly this case —
// "the filesystem containing oldpath and newpath does not support the creation of
// hard links" — so ln and cp -l print the right diagnostic with no special case.
// (rclone answers ENOSYS; EPERM is the more standard spelling of the same
// refusal.) Reversible if enough users ask for it, in a way that silently broken
// links are not.
func (n *node) Link(_ context.Context, _ fs.InodeEmbedder, name string, _ *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	n.logf("[mount] refused a hard link at %s (no provider can represent one)", n.childPath(name))
	return nil, syscall.EPERM
}

// Symlink creates the link in the backing store and says that it stops there.
//
// Deliberately not synced: representing a symlink on a path-addressed remote takes
// two signals, and deciding it from the content alone breaks enumeration, opens a
// symlink-injection channel and freezes a wire format a user can edit in a web UI.
// DESIGN.md §9 M15 item 5 has the full argument; until it lands, a symlink is a
// local fact about this machine, which is also what rclone does by default.
func (n *node) Symlink(ctx context.Context, target, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	inode, errno := n.LoopbackNode.Symlink(ctx, target, name, out)
	if errno == 0 {
		n.logf("[mount] skip %s (symbolic link: no remote representation, stays local)", n.childPath(name))
	}
	return inode, errno
}

// Mknod creates the node in the backing store and reports whether it will sync.
//
// A fifo, socket or device node has no byte stream, so there is nothing to upload
// and it stays local. A *regular* file is a different matter: mknod(2) with a mode
// naming no type creates one, and it must sync like any other regular file, or the
// mount and the sweep disagree about the same file — the sweep's walk pushes
// whatever is regular, so staying silent here would make syncing depend on which
// syscall created the file. That is the MC-12 shape and it is a bug wherever it
// appears, so this path emits OpCreate exactly as Create does.
func (n *node) Mknod(ctx context.Context, name string, mode, rdev uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	inode, errno := n.LoopbackNode.Mknod(ctx, name, mode, rdev, out)
	if errno != 0 {
		return inode, errno
	}
	path := n.childPath(name)
	if mode&syscall.S_IFMT == syscall.S_IFREG || mode&syscall.S_IFMT == 0 {
		n.emit(fsevent.Event{Op: fsevent.OpCreate, Path: path})
		return inode, errno
	}
	n.logf("[mount] skip %s (%s: no remote representation, stays local)", path, specialKind(mode))
	return inode, errno
}

// specialKind names the file type in a mode word, for the one job of telling a
// user which of their files is not being synced.
//
// The sweep's local walk needs the same vocabulary and has its own copy
// (syncengine/reconcile.go, kindOf). Sharing one helper would mean either package
// importing the other: the engine must not depend on the mount backend — that
// would pull go-fuse into every build of the sync core and cost the cross-compile
// proof of DESIGN.md §2.9 — and the mount must not depend on the engine. Two
// four-line switches over different input types (a mknod mode word here, an
// fs.FileMode there) is the cheaper of the two prices. Keep the wording identical.
func specialKind(mode uint32) string {
	switch mode & syscall.S_IFMT {
	case syscall.S_IFIFO:
		return "named pipe"
	case syscall.S_IFSOCK:
		return "socket"
	case syscall.S_IFCHR:
		return "character device"
	case syscall.S_IFBLK:
		return "block device"
	case syscall.S_IFLNK:
		return "symbolic link"
	case syscall.S_IFDIR:
		return "directory"
	case syscall.S_IFREG:
		return "regular file"
	}
	return "special file"
}
