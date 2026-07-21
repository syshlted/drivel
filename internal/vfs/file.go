package vfs

import (
	"context"
	"sync/atomic"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/zishmusic/drivel/internal/fsevent"
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
	dirty   atomic.Bool
}

// Interface assertions: the wrapper must satisfy every capability go-fuse probes
// for, or those operations would silently degrade for wrapped files.
var (
	_ fs.FileReader   = (*fileHandle)(nil)
	_ fs.FileWriter   = (*fileHandle)(nil)
	_ fs.FileFlusher  = (*fileHandle)(nil)
	_ fs.FileReleaser = (*fileHandle)(nil)
	_ fs.FileFsyncer  = (*fileHandle)(nil)
	_ fs.FileLseeker  = (*fileHandle)(nil)
	_ fs.FileGetattrer = (*fileHandle)(nil)
	_ fs.FileSetattrer = (*fileHandle)(nil)
	_ fs.FileAllocater = (*fileHandle)(nil)
)

func (f *fileHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	if r, ok := f.wrapped.(fs.FileReader); ok {
		return r.Read(ctx, dest, off)
	}
	return nil, syscall.ENOTSUP
}

func (f *fileHandle) Write(ctx context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	w, ok := f.wrapped.(fs.FileWriter)
	if !ok {
		return 0, syscall.ENOTSUP
	}
	n, errno := w.Write(ctx, data, off)
	if errno == 0 {
		f.dirty.Store(true)
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
	var errno syscall.Errno
	if r, ok := f.wrapped.(fs.FileReleaser); ok {
		errno = r.Release(ctx)
	}
	// Emit the content-change event only after a successful close of a handle we
	// actually wrote to. Coalesces a burst of Writes into one upload trigger.
	if errno == 0 && f.dirty.Load() && f.events != nil {
		f.events <- fsevent.Event{Op: fsevent.OpWrite, Path: f.path}
	}
	return errno
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
	if s, ok := f.wrapped.(fs.FileSetattrer); ok {
		return s.Setattr(ctx, in, out)
	}
	return syscall.ENOTSUP
}

func (f *fileHandle) Allocate(ctx context.Context, off uint64, size uint64, mode uint32) syscall.Errno {
	if a, ok := f.wrapped.(fs.FileAllocater); ok {
		return a.Allocate(ctx, off, size, mode)
	}
	return syscall.ENOTSUP
}
