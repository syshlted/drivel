// Package mount is the seam between the sync core and a concrete mount frontend.
// A Backend mounts an interceptor filesystem that proxies to a backing store and
// emits change events; go-fuse is the first (and currently only) implementation.
// The seam keeps the platform-coupled mount code isolated so the rest of Drivel
// stays portable, and leaves room for other backends (cgofuse for Windows, an
// NFS-loopback backend for platforms without a Go FUSE binding).
package mount

import (
	"context"
	"io"
	"log"

	"github.com/zishmusic/drivel/internal/fsevent"
)

// Hydrator is the OPTIONAL lazy-hydration hook (M5). A mount backend consults it
// before serving a file whose content may not be resident in the backing store.
// Declaring it here rather than importing internal/hydrate keeps the mount seam —
// and every backend below it — free of the sync and provider layers.
//
// A nil Options.Hydrator selects eager mode: the backing store is assumed to hold
// full content, which is the M1–M4 behaviour and remains the default.
type Hydrator interface {
	// IsPlaceholder reports whether rel (root-relative, slash-separated) exists in
	// the backing store without its content. It must fail safe — report true when
	// it cannot tell — because the cost of a wrong "false" is an empty upload.
	IsPlaceholder(rel string) bool
	// Hydrate blocks until rel's content is resident, or returns an error.
	Hydrate(ctx context.Context, rel string) error
	// Discard drops the placeholder mark without fetching, for a caller that is
	// about to replace the content wholesale (a truncating open).
	Discard(rel string) error
}

// Options configure a single mount.
type Options struct {
	Mountpoint string               // where the filesystem is mounted
	Backing    string               // path the FS proxies to (may be /proc/self/fd/N in-place)
	Events     chan<- fsevent.Event // change events sink
	FsName     string               // display name for the mount
	Debug      bool                 // backend-level tracing
	Hydrator   Hydrator             // nil => eager mode (content always resident)
	// Logger is where the backend reports; nil uses the log package's default.
	// With several mounts in one process it carries the mount's identity, without
	// which a hydration failure does not say whose file failed.
	Logger *log.Logger
}

// Backend mounts and serves an interceptor filesystem.
type Backend interface {
	// Name identifies the backend (e.g. "go-fuse") for logging.
	Name() string
	// Serve mounts at opts.Mountpoint proxying to opts.Backing and blocks until
	// ctx is cancelled (which unmounts) or the mount otherwise ends.
	Serve(ctx context.Context, opts Options) error
}

// Backing describes the resolved backing store for a mount.
type Backing struct {
	// Path is what both the mount backend and the sync engine read/write through.
	// In in-place mode this is /proc/self/fd/N, which routes around the mount
	// overlay — see ResolveBacking.
	Path    string
	InPlace bool

	closer io.Closer
}

// Close releases any resource held for the backing (the pre-mount dirfd in
// in-place mode). Must be called only after the mount is unmounted.
func (b *Backing) Close() error {
	if b.closer != nil {
		return b.closer.Close()
	}
	return nil
}

// ResolveBacking picks the backing store for a mount.
//
//   - dataDir != "": separate-directory mode. The backing is dataDir as-is.
//   - dataDir == "": in-place mode. The mountpoint is its own backing store. A
//     directory fd is opened *before* mounting; the returned Path routes through
//     it (/proc/self/fd/N on Linux) so backing I/O bypasses the FUSE overlay
//     rather than recursing into it. In-place mode is Linux-only for now.
//
// The caller must hold the returned Backing open until after unmount.
func ResolveBacking(mountpoint, dataDir string) (*Backing, error) {
	if dataDir != "" {
		return &Backing{Path: dataDir, InPlace: false}, nil
	}
	path, closer, err := openInPlace(mountpoint)
	if err != nil {
		return nil, err
	}
	return &Backing{Path: path, InPlace: true, closer: closer}, nil
}
