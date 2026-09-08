//go:build linux

package vfs

// compulsoryOptions returns the mount options drivel always requests. There is
// deliberately no flag to disable them (DESIGN.md §9, M15).
//
// This is a cloud-storage client: a device node or a setuid binary arriving from
// a remote is never something a user asked for, so there is nothing to weigh
// against refusing it. Linux is the platform where both flags exist and both are
// reachable, and the reason to name them here is that drivel takes two different
// mount paths and only one of them is safe by default. The unprivileged path is
// already covered — fusermount3 mounts with MS_NOSUID|MS_NODEV and only a
// privileged caller can clear them — but go-fuse's direct-mount path derives its
// flags from these strings, and its own default is discarded outright the moment
// MountOptions.DirectMountFlags is set. Naming them makes the guarantee a
// property of drivel rather than of which helper ran and who ran it.
//
// It does not protect the backing store. In separate-directory mode -data is an
// ordinary directory on an ordinary filesystem, reachable without going through
// the mount at all, so a setuid bit restored there from remote metadata is live
// however the mountpoint is flagged. This narrows the blast radius; it does not
// discharge DESIGN.md §10.4's rule that permission metadata from a remote source
// is executable trust and that setuid/setgid are masked by default.
//
// A fresh slice each call: go-fuse reads MountOptions.Options directly, and a
// shared package-level slice is a mutable global for the sake of one allocation
// per mount.
func compulsoryOptions() []string { return []string{"nodev", "nosuid"} }
