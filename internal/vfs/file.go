package vfs

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/zishmusic/drivel/internal/fsevent"
	"github.com/zishmusic/drivel/internal/mount"
	"github.com/zishmusic/drivel/internal/ranges"
)

// fileHandle wraps a loopback file handle to capture content writes. All ops are
// forwarded to the wrapped handle; Write additionally marks the handle dirty, and
// Release emits a single OpWrite for the file once it was written and is closed
// (push-on-close semantics). Node-level events (create/rename/...) are emitted by
// *node; this covers in-place content edits, which the node layer can't see.
type fileHandle struct {
	wrapped fs.FileHandle
	path    string
	events  chan<- fsevent.Event
	hyd     mount.Hydrator // nil => eager mode
	dirty   dirtyTracker
	// resident latches once the content is known to be present, so the steady-state
	// read path costs one atomic load rather than a getxattr per operation.
	resident atomic.Bool
}

// dirtyTracker records which byte extents a handle wrote, so the uploader can
// ship only those to a provider that supports range writes (M6). It is per-handle
// and in memory only: the extents exist to describe one pending push, and if the
// process dies before the push the event that would have carried them is lost
// too, leaving nothing to be stale.
//
// The tracker has one job beyond bookkeeping, and it is the load-bearing one:
// knowing when NOT to claim knowledge. Any operation that changes the file's
// length invalidates the extent map — the offsets no longer describe the same
// file — so it poisons the record instead of trying to adjust it, and the handle
// reports "unknown", which the engine reads as "push the whole file". The wrong
// direction here writes a stale block over a good one.
type dirtyTracker struct {
	mu      sync.Mutex
	written bool // some Write succeeded on this handle
	whole   bool // extents unusable; the whole file must be pushed
	set     ranges.Set
}

// mark records that [off, off+n) was written, growing the map if the file did.
func (d *dirtyTracker) mark(off int64, n int) {
	if n <= 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.written = true
	if d.set.BlockSize == 0 {
		d.set.BlockSize = ranges.DefaultBlockSize
	}
	d.set.Grow(off + int64(n))
	d.set.MarkCovering(off, int64(n))
}

// poison discards any claim to know which extents changed, without itself
// triggering a push. Deliberately it does NOT set written: a bare truncate or
// fallocate through an open handle pushes nothing today (the engine treats
// OpSetattr as metadata-only), and M6 is not the milestone to change that. What
// it must guarantee is that a truncate *alongside* real writes downgrades the
// push to whole-file rather than shipping extents measured against the old
// length.
func (d *dirtyTracker) poison() {
	d.mu.Lock()
	d.whole = true
	d.mu.Unlock()
}

// snapshot reports whether the handle was written to, and the extents if they are
// trustworthy. A nil set means "unknown" — push everything.
//
// size is the file's current length, or negative when it could not be read. It
// has to be supplied from outside because the tracker sees only the writes that
// came through this handle: after editing one block in the middle of a large
// file, its own idea of the length stops at the last byte written, and extents
// measured against that phantom length would describe a file the uploader is not
// holding. The engine cross-checks the set's Size against the file on disk and
// declines the range write when they disagree, so getting this wrong would not
// corrupt anything — it would quietly make M6 never fire.
func (d *dirtyTracker) snapshot(size int64) (written bool, extents *ranges.Set) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.written || d.whole {
		return d.written, nil
	}
	if size < d.set.Size {
		// Either the size is unknown, or the file shrank behind this handle's back
		// (another handle truncated it). Both make every recorded offset suspect.
		return true, nil
	}
	// Clone: writes may still be arriving on another thread sharing this handle,
	// and the snapshot travels to the uploader on a channel.
	c := d.set.Clone()
	c.Grow(size)
	return true, &c
}

// ensureResident faults the file's content in before the first I/O on this handle
// (M5). Hydration is deferred to here rather than to Open so that opening a file —
// or a whole tree — costs nothing, and so an open/truncate/rewrite sequence never
// downloads content it is about to discard.
//
// A failure returns EIO rather than letting the read proceed: a placeholder reads
// as zeros, and serving those as if they were content is silent corruption.
// Concurrent handles are safe — the hydrator collapses them into one fetch.
func (f *fileHandle) ensureResident(ctx context.Context) syscall.Errno {
	if f.hyd == nil || f.resident.Load() {
		return 0
	}
	if f.hyd.IsPlaceholder(f.path) {
		if err := f.hyd.Hydrate(ctx, f.path); err != nil {
			log.Printf("[hydrate] %s: %v", f.path, err)
			return syscall.EIO
		}
	}
	f.resident.Store(true)
	return 0
}

// Interface assertions: the wrapper must satisfy every capability go-fuse probes
// for, or those operations would silently degrade for wrapped files.
var (
	_ fs.FileReader    = (*fileHandle)(nil)
	_ fs.FileWriter    = (*fileHandle)(nil)
	_ fs.FileFlusher   = (*fileHandle)(nil)
	_ fs.FileReleaser  = (*fileHandle)(nil)
	_ fs.FileFsyncer   = (*fileHandle)(nil)
	_ fs.FileLseeker   = (*fileHandle)(nil)
	_ fs.FileGetattrer = (*fileHandle)(nil)
	_ fs.FileSetattrer = (*fileHandle)(nil)
	_ fs.FileAllocater = (*fileHandle)(nil)
)

func (f *fileHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	if errno := f.ensureResident(ctx); errno != 0 {
		return nil, errno
	}
	if r, ok := f.wrapped.(fs.FileReader); ok {
		return r.Read(ctx, dest, off)
	}
	return nil, syscall.ENOTSUP
}

func (f *fileHandle) Write(ctx context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	// A partial write into an unhydrated placeholder would leave the untouched
	// extents as zeros and then push that over the real remote file.
	if errno := f.ensureResident(ctx); errno != 0 {
		return 0, errno
	}
	w, ok := f.wrapped.(fs.FileWriter)
	if !ok {
		return 0, syscall.ENOTSUP
	}
	n, errno := w.Write(ctx, data, off)
	if errno == 0 {
		// Record what the kernel actually accepted, not what it offered.
		f.dirty.mark(off, int(n))
	}
	return n, errno
}

func (f *fileHandle) Flush(ctx context.Context) syscall.Errno {
	if fl, ok := f.wrapped.(fs.FileFlusher); ok {
		return fl.Flush(ctx)
	}
	return 0
}

func (f *fileHandle) Release(ctx context.Context) syscall.Errno {
	// Size first: releasing the wrapped handle closes the descriptor this reads.
	size := f.currentSize(ctx)

	var errno syscall.Errno
	if r, ok := f.wrapped.(fs.FileReleaser); ok {
		errno = r.Release(ctx)
	}
	// Emit the content-change event only after a successful close of a handle we
	// actually wrote to. Coalesces a burst of Writes into one upload trigger, and
	// carries the extents those writes touched (nil if they cannot be trusted).
	if written, extents := f.dirty.snapshot(size); errno == 0 && written && f.events != nil {
		f.events <- fsevent.Event{Op: fsevent.OpWrite, Path: f.path, Dirty: extents}
	}
	return errno
}

// currentSize fstats the open file, returning -1 if it cannot. Only the dirty
// extents depend on it, and they degrade to "unknown" — a whole-file push —
// rather than to anything wrong.
func (f *fileHandle) currentSize(ctx context.Context) int64 {
	g, ok := f.wrapped.(fs.FileGetattrer)
	if !ok {
		return -1
	}
	var out fuse.AttrOut
	if errno := g.Getattr(ctx, &out); errno != 0 {
		return -1
	}
	return int64(out.Size)
}

func (f *fileHandle) Fsync(ctx context.Context, flags uint32) syscall.Errno {
	if s, ok := f.wrapped.(fs.FileFsyncer); ok {
		return s.Fsync(ctx, flags)
	}
	return 0
}

func (f *fileHandle) Lseek(ctx context.Context, off uint64, whence uint32) (uint64, syscall.Errno) {
	if l, ok := f.wrapped.(fs.FileLseeker); ok {
		return l.Lseek(ctx, off, whence)
	}
	return 0, syscall.ENOTSUP
}

func (f *fileHandle) Getattr(ctx context.Context, out *fuse.AttrOut) syscall.Errno {
	if g, ok := f.wrapped.(fs.FileGetattrer); ok {
		return g.Getattr(ctx, out)
	}
	return syscall.ENOTSUP
}

func (f *fileHandle) Setattr(ctx context.Context, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	s, ok := f.wrapped.(fs.FileSetattrer)
	if !ok {
		return syscall.ENOTSUP
	}
	errno := s.Setattr(ctx, in, out)
	// A size change moves every byte after the cut, so the extents this handle
	// recorded no longer describe the file. Chmod/utimes leave content alone.
	if errno == 0 {
		if _, isResize := in.GetSize(); isResize {
			f.dirty.poison()
		}
	}
	return errno
}

func (f *fileHandle) Allocate(ctx context.Context, off uint64, size uint64, mode uint32) syscall.Errno {
	a, ok := f.wrapped.(fs.FileAllocater)
	if !ok {
		return syscall.ENOTSUP
	}
	errno := a.Allocate(ctx, off, size, mode)
	if errno == 0 {
		// fallocate can extend the file, and with FALLOC_FL_PUNCH_HOLE/COLLAPSE_RANGE
		// it rewrites content this handle never saw. Neither is expressible as an
		// extent this handle owns, so stop claiming to know.
		f.dirty.poison()
	}
	return errno
}
