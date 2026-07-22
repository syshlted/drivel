// Package provider defines the seam between the sync engine and a concrete
// cloud-storage backend. v1 ships a Google Drive implementation (added in M2),
// but the FS and sync layers depend only on these interfaces so other providers
// can be added later.
//
// The seam is deliberately **path-addressed**: every method speaks root-relative
// slash paths (no leading slash) — the same paths internal/fsevent emits. Drive's
// opaque file IDs, S3 keys, WebDAV URLs, etc. are the provider's private business;
// the sync engine never sees them. A provider that is natively ID-addressed (Drive)
// owns the path↔ID translation internally. See DESIGN.md §2.5.
package provider

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrNotExist is returned by Move when the source path is not known to the store.
// The engine treats this as "upload the destination as fresh content" rather than
// a hard failure (e.g. renaming a file the store never received).
var ErrNotExist = errors.New("provider: path does not exist")

// IsRetryable reports whether err is a transient failure worth retrying with
// backoff (a rate limit, a 5xx, a dropped connection) rather than a permanent
// one (bad request, permission denied, not found). A provider signals "retryable"
// by returning an error whose chain contains a value implementing
//
//	interface { Retryable() bool }
//
// that reports true — the same errors.As convention as ErrNotExist, keeping the
// engine's retry policy provider-agnostic (it never imports a provider package).
// Errors that don't implement the interface are treated as permanent.
func IsRetryable(err error) bool {
	var r interface{ Retryable() bool }
	return errors.As(err, &r) && r.Retryable()
}

// RemoteFile is the provider-agnostic view of a stored object.
type RemoteFile struct {
	Path     string    // root-relative slash path
	IsDir    bool
	Size     int64
	Hash     string    // content checksum (e.g. Drive md5Checksum), when the provider exposes one
	Version  string    // opaque provider version/etag
	Modified time.Time
}

// RemoteChange is one entry from a provider's incremental change feed.
type RemoteChange struct {
	Path    string      // root-relative slash path of the changed object
	File    *RemoteFile // nil when Removed is true
	Removed bool
}

// Store is the required cloud-backend surface: mutations plus reads, all
// path-addressed. Implementations must be safe for concurrent use — the uploader
// and downloader call them from different goroutines.
//
// Put and Mkdir create any missing ancestor directories; the engine never has to
// pre-create parents. Move renames/relocates an object; providers without a
// native server-side move (e.g. S3) implement it as copy+delete, which resets the
// object's identity (see DESIGN.md §2.5, "Move semantics").
type Store interface {
	// Put creates or replaces the file at path with the contents of r.
	Put(ctx context.Context, path string, r io.Reader) (RemoteFile, error)
	// Mkdir creates the directory at path (and any missing ancestors).
	Mkdir(ctx context.Context, path string) (RemoteFile, error)
	// Move relocates/renames oldPath to newPath.
	Move(ctx context.Context, oldPath, newPath string) (RemoteFile, error)
	// Remove deletes the object at path (recursively, for directories).
	Remove(ctx context.Context, path string) error
	// Get opens the object at path for reading.
	Get(ctx context.Context, path string) (io.ReadCloser, error)
	// Stat reports the object at path; ok is false if it does not exist.
	Stat(ctx context.Context, path string) (rf RemoteFile, ok bool, err error)
}

// ChangeSource is an OPTIONAL capability: an incremental inbound change feed used
// by the pull loop (M3). Providers with a native cursor feed (Drive's
// changes.list, Dropbox's list_folder/continue) implement it; providers without
// one (S3, WebDAV) omit it and run outbound-only. The engine enables inbound sync
// only for stores that also satisfy this interface.
type ChangeSource interface {
	// StartCursor returns an opaque token marking "now" in the change feed.
	StartCursor(ctx context.Context) (string, error)
	// Changes returns changes since cursor and the cursor to use next time.
	Changes(ctx context.Context, cursor string) (changes []RemoteChange, next string, err error)
}
