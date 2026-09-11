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

	"github.com/zishmusic/drivel/internal/ranges"
)

// ErrNotExist is returned by Move when the source path is not known to the store.
// The engine treats this as "upload the destination as fresh content" rather than
// a hard failure (e.g. renaming a file the store never received).
var ErrNotExist = errors.New("provider: path does not exist")

// ErrCursorExpired is returned by ChangeSource.Changes when the cursor is too old
// for the provider to answer from (Drive replies 410 to a stale page token).
//
// It is a distinct sentinel because it needs a distinct recovery: the gap it
// leaves cannot be filled by retrying — the changes it covers are simply not
// available any more — so the engine responds by re-enumerating and reconciling
// (M7b) rather than by backing off. Before it existed, a dead token logged once a
// poll and inbound sync stayed silently stopped forever.
var ErrCursorExpired = errors.New("provider: change cursor expired")

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
	Path     string // root-relative slash path
	IsDir    bool
	Size     int64
	Hash     string // content checksum (e.g. Drive md5Checksum), when the provider exposes one
	Version  string // opaque provider version/etag
	Modified time.Time

	// ExportOnly marks an object with no directly downloadable byte stream —
	// Google-native Docs/Sheets/Slides, which are *converted* on the way out
	// rather than downloaded. Such an object has no honest size and no checksum,
	// so it can be neither placeholder'd (M5 needs an apparent size) nor compared
	// (M6 needs a digest), and Get on it fails. Callers skip its content and say
	// so, instead of retrying a download that cannot work.
	ExportOnly bool
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
	//
	// Where the bytes go is the provider's business and the seam does not ask:
	// backends differ on what "deleted" can mean (Drive has a trash, a bucket may
	// be versioned, a content-addressed store has a GC policy). A provider with a
	// recoverable form should prefer it — reconcile *infers* some of these
	// deletions from a baseline, and there is no fallback above this call.
	//
	// "Prefer the recoverable form" means take the one the backend already has,
	// never manufacture one. A provider whose store offers nothing of the kind
	// deletes outright and says so in its documentation; -max-deletes is then the
	// only guard, which is a fact to disclose rather than a gap to paper over. An
	// emulated trash inside the synced tree is actively wrong — the sweep would
	// enumerate it and pull every deleted file back — and one outside it is a
	// second store to garbage-collect, with its own failure modes, bought for a
	// backend that did not ask for it.
	Remove(ctx context.Context, path string) error
	// Get opens the object at path for reading.
	Get(ctx context.Context, path string) (io.ReadCloser, error)
	// Stat reports the object at path; ok is false if it does not exist.
	Stat(ctx context.Context, path string) (rf RemoteFile, ok bool, err error)
}

// ChangeSource is an OPTIONAL capability: an incremental inbound change feed used
// by the pull loop (M3). Providers with a native cursor feed (Drive's
// changes.list, Dropbox's list_folder/continue) implement it; providers without
// one (SFTP, WebDAV, S3) omit it.
//
// Omitting it does not mean outbound-only. A store that implements Enumerator
// still syncs inbound, through the M7b sweep alone — which is then the entire
// inbound path rather than a safety net beneath a feed, so -sweep-interval
// becomes that mount's poll interval and its latency. Only a store with neither
// capability is outbound-only. Do not synthesize a feed by polling and diffing:
// the sweep already is that, with the baseline and delete guards that make an
// inferred deletion safe.
type ChangeSource interface {
	// StartCursor returns an opaque token marking "now" in the change feed.
	StartCursor(ctx context.Context) (string, error)
	// Changes returns changes since cursor and the cursor to use next time.
	Changes(ctx context.Context, cursor string) (changes []RemoteChange, next string, err error)
}

// Enumerator is an OPTIONAL capability: a complete listing of everything under
// the mount root, used by the M7b initial-enumeration sweep to make a Drive that
// existed before the first mount visible at all (the change feed only ever
// reports what changes *after* a cursor is taken).
//
// It is metadata only — no content is transferred — and is expected to cost
// roughly one request per page of objects, which is what makes running it by
// default affordable. Providers that cannot enumerate omit it and M7b is a no-op
// for them; inbound sync still works from the change feed.
//
// The listing is path-addressed like the rest of the seam, so id→path assembly
// happens below it: a provider that is natively ID-addressed resolves the tree
// itself (and warms its own path index doing so), and the engine never learns
// what a native ID is.
//
// cursor resumes an interrupted sweep — pass "" to start one, then the token
// returned by the previous call. A next of "" means the sweep is complete.
// Objects whose parent chain does not reach the mount root are the provider's to
// drop; the engine only ever sees paths inside the mount.
type Enumerator interface {
	Enumerate(ctx context.Context, cursor string) (files []RemoteFile, next string, err error)
}

// RangeGetter is an OPTIONAL capability: reading a byte range of an object
// instead of the whole thing. Lazy hydration (M5) uses it to fault in only the
// blocks a read touches; providers without ranged reads simply omit it and
// callers fall back to Get.
//
// Length <= 0 means "to end of object". Implementations return a reader over
// exactly the requested extent (clamped to the object's size); a short object is
// not an error.
type RangeGetter interface {
	GetRange(ctx context.Context, path string, off, length int64) (io.ReadCloser, error)
}

// RangePutter is an OPTIONAL capability and RangeGetter's write-side mirror (M6):
// replacing byte extents of an existing object in place, leaving every other byte
// untouched, so editing one block of a 4 GB file costs one block of upload.
//
// It is genuinely optional. Google Drive does NOT implement it — files.update
// replaces content wholesale and its resumable protocol still requires every
// chunk of the new file — so gdrive omits it and the uploader falls back to
// whole-file Put. See DESIGN.md §9 (M6).
//
// Contract, all of which the engine checks before calling and the implementation
// must re-check:
//
//   - The object must already exist and its size must equal size. A range write
//     cannot create, extend or truncate; anything that changes the file's length
//     is a whole-file Put. Implementations MUST fail rather than resize.
//   - extents are non-overlapping, ascending, and lie within [0, size).
//   - src reads the complete local file — the implementation seeks into it for
//     each extent. It is a ReaderAt, not a Reader, precisely so extents can be
//     sent in whatever order the wire protocol prefers.
//
// Returning an error is always safe: the engine logs it and retries the write as
// a whole-file Put, which is slower but never wrong.
type RangePutter interface {
	PutRange(ctx context.Context, path string, src io.ReaderAt, size int64, extents []ranges.Range) (RemoteFile, error)
}

// ContentHasher is an OPTIONAL capability: computing, over local content, the
// same checksum the provider reports in RemoteFile.Hash.
//
// It exists so the uploader can answer "does the remote already hold exactly
// these bytes?" without knowing which algorithm the provider uses — Drive's
// md5Checksum today, something else tomorrow. Comparing a hash we computed
// ourselves against RemoteFile.Hash would otherwise bake a provider's choice of
// digest into the engine.
//
// The payoff is skipping the upload entirely for a write that did not change the
// content (a touch, an editor that rewrites an identical buffer, a re-run of a
// build). On a multi-gigabyte file that trades a local read for a network
// transfer. Providers with no exposed checksum omit it and every push proceeds.
type ContentHasher interface {
	// HashContent returns the checksum of everything readable from r, in the same
	// encoding the provider populates RemoteFile.Hash with.
	HashContent(r io.Reader) (string, error)
}
