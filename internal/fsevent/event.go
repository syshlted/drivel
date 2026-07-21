// Package fsevent defines the backend-neutral change events emitted by a mount
// frontend (go-fuse today; cgofuse or an NFS-loopback backend later) and consumed
// by the sync engine. Keeping it separate from any concrete backend lets the
// sync core stay portable and lets multiple mount backends share one event type.
package fsevent

// Op identifies the kind of filesystem mutation an Event describes.
type Op string

const (
	OpCreate  Op = "create"
	OpWrite   Op = "write"
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
