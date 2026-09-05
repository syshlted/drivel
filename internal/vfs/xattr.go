package vfs

import (
	"context"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
)

// Extended attributes are refused at the mountpoint unless the mount opted in
// (mount.Options.Xattr, off by default). Drivel is an interceptor, not a plain
// loopback: M5's authoritative placeholder marker is a user xattr on the backing
// file, so proxying xattrs publishes drivel's own control metadata at the
// mountpoint and makes it writable by anything that can write there. Removing the
// marker from an unhydrated placeholder makes the uploader push its zeros over the
// real remote file; attaching one to a resident file makes the next read fetch the
// remote copy over local content.
//
// fuse.MountOptions.DisableXAttrs (see backend.go) already stops the kernel from
// sending GETXATTR and LISTXATTR, but it covers only those two: go-fuse's
// doSetXAttr and doRemoveXAttr have no such check, so a SETXATTR arriving on this
// mount would reach fs.LoopbackNode and land on the backing file. The write side
// is the dangerous one, so the refusal lives on the node as well, where it holds
// regardless of the server options a future backend passes.
//
// The refusal is ENOSYS rather than EPERM because that is the answer the FUSE
// protocol reserves for "this filesystem does not implement the operation": the
// kernel records it, stops issuing that operation for the mount, and returns
// EOPNOTSUPP to the caller — the same result a filesystem without xattr support
// gives. EPERM would say the attribute exists and is protected, which is a
// different (and false) claim.

var (
	_ fs.NodeGetxattrer    = (*node)(nil)
	_ fs.NodeSetxattrer    = (*node)(nil)
	_ fs.NodeRemovexattrer = (*node)(nil)
	_ fs.NodeListxattrer   = (*node)(nil)
)

func (n *node) Getxattr(ctx context.Context, attr string, dest []byte) (uint32, syscall.Errno) {
	if !n.xattr {
		return 0, syscall.ENOSYS
	}
	return n.LoopbackNode.Getxattr(ctx, attr, dest)
}

func (n *node) Setxattr(ctx context.Context, attr string, data []byte, flags uint32) syscall.Errno {
	if !n.xattr {
		return syscall.ENOSYS
	}
	return n.LoopbackNode.Setxattr(ctx, attr, data, flags)
}

func (n *node) Removexattr(ctx context.Context, attr string) syscall.Errno {
	if !n.xattr {
		return syscall.ENOSYS
	}
	return n.LoopbackNode.Removexattr(ctx, attr)
}

func (n *node) Listxattr(ctx context.Context, dest []byte) (uint32, syscall.Errno) {
	if !n.xattr {
		return 0, syscall.ENOSYS
	}
	return n.LoopbackNode.Listxattr(ctx, dest)
}
