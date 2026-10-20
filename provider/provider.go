// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

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

	"github.com/syshlted/drivel/ranges"
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

// ErrExist is returned by Creator.Create when something already stands at the
// path. The engine answers it by keeping what stands there as a conflict copy
// before it uploads (DESIGN.md §6), so a provider must return it only when the
// path is occupied, and must not have changed the path when it does.
var ErrExist = errors.New("provider: path already exists")

// ErrChanged is returned by Uploader.Upload when the caller said which object it
// expected to replace (UploadOptions.Replaces) and the store found, before
// replacing anything, that the path no longer holds it. The path must be left
// untouched. The engine answers it by looking at the remote again, which keeps
// the newer version as a conflict copy before the next attempt (DESIGN.md §6).
var ErrChanged = errors.New("provider: remote object changed")

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

	// MovedFrom is the path the object at Path has just left, for a feed keyed by
	// object identity rather than by path. It is OPTIONAL in both directions: a
	// provider that cannot answer it leaves it empty, and a host that acts on it
	// must behave correctly without it, because every host did before it existed.
	//
	// What it buys is the difference between a move and a delete-plus-create. A
	// feed keyed by identity reports a rename as the object under its new name and
	// says nothing at all about the name it left — so a path-addressed engine sees
	// content appear somewhere new and has no reason to connect it to the content
	// it already holds. For a *file* that costs a re-download per client; for a
	// *directory* it costs the whole subtree, in both directions at once, because
	// the children of a moved folder are not themselves reported (see MC-26b and
	// MC-27 in docs/dev/multiclient-test-plan.md: at 1100 files the receiving
	// client downloads the subtree a second time and then proposes 1101 deletions,
	// which -max-deletes refuses, leaving it doubled permanently).
	//
	// Three requirements on a provider that sets it, and the first is the one that
	// makes the other two safe:
	//
	//   - Set it only for a MOVE OF THE SAME OBJECT — the thing at Path is the
	//     thing that was at MovedFrom, with its content unchanged. A host may
	//     carry the §4 baseline across it, so "probably the same file" is not
	//     enough. A provider whose move is a copy-and-delete (no server-side
	//     rename) must leave it empty; identity is exactly what such a move does
	//     not preserve.
	//   - Derive it from something authoritative about THIS process's own view, not
	//     from a persistent cache. A host acts on it by renaming local files, so a
	//     stale answer moves the wrong tree. gdrive derives it from the in-memory
	//     reverse map, which linkLocked maintains as an exact inverse, and
	//     deliberately not from the M7 path index — see M7 invariant 1.
	//   - It is advice, never an instruction. The host validates it, declines it
	//     for any reason it likes, and falls back to treating the change as an
	//     ordinary appearance at Path. Nothing about correctness may depend on the
	//     host taking it.
	MovedFrom string
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
//
// **A non-empty next MUST differ from the cursor it was given.** The engine
// reads its own cursor handed back as a provider that cannot advance and ends
// the sweep there rather than listing one page forever — which leaves the
// listing a prefix of the tree, so no deletion is inferred from it and it does
// not count as a completed sweep. A provider whose real continuation state is
// in memory still has to return something that moves; a page counter is enough.
// This is not a formality: gdrive's scoped descent returned a constant, which
// truncated every scoped sweep to two calls (MC-12, 2026-10-14).
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

// Creator is an OPTIONAL capability: a Put that refuses to replace.
//
// The engine uploads a file it has no baseline for — a first push — through
// Create when the store offers it, so that a remote object this mount has never
// seen is found by the upload itself instead of by a Stat before it. That is
// what makes the check free on a store whose Put already resolves the path
// (Drive's does, to choose between update and create), and it is why a Creator
// may refuse BEFORE it reads r: the content need not cross the network to learn
// that the path is taken.
//
// Create returns an error wrapping ErrExist when the path is occupied, by a file
// or a directory, and leaves it untouched. It is not a conditional write and
// callers must not treat it as one: a store that can make the check and the
// create a single operation should (SFTP's hard link does), and one that cannot
// checks and then creates, so two clients creating one path at one instant may
// both succeed exactly as two Puts would. A store without the capability costs
// the engine a Stat per first push instead.
type Creator interface {
	Create(ctx context.Context, path string, r io.Reader) (RemoteFile, error)
}

// Trasher is an OPTIONAL capability: a removal the user can undo, whatever the
// store was configured to do on an ordinary Remove.
//
// It exists for conflict copies under `conflicts = "remote-trash"`, where the
// promise is that the losing version can be restored from the backend's trash.
// Remove cannot carry that promise, because a store may be configured to delete
// permanently (Drive's `delete = "permanent"`) and the host cannot see that
// setting. A store with no trash at all — SFTP, and every M17–M21 backend — does
// not implement it, and a mount asking for remote-trash on one is refused at
// open rather than quietly given a permanent deletion.
//
// Trash returns an error wrapping ErrNotExist when nothing stands at the path:
// the caller has just uploaded the copy it is trashing, so absence means the
// copy is not where it is about to report it to be.
type Trasher interface {
	Trash(ctx context.Context, path string) error
}

// Copier is an OPTIONAL capability: a copy made by the backend, without the
// bytes crossing the network.
//
// It exists for conflict copies: keeping the remote's losing version on the
// remote (`conflicts-dir = "remote"` or `"both"`, and `conflicts =
// "remote-trash"`) otherwise costs downloading it and uploading it again. A
// store that would have to do that itself to copy must not implement it; the
// engine already knows how, and doing it below the seam would hide the cost.
//
// Copy makes a new object at to holding the content of the object at from now,
// creating to's missing parent directories as Put does, and returns the new
// object. It returns an error wrapping ErrNotExist when nothing stands at from
// and one wrapping ErrExist when something stands at to, which it leaves
// untouched. The source may have changed since the caller last looked; the
// returned Hash is how a caller that cares finds out.
type Copier interface {
	Copy(ctx context.Context, from, to string) (RemoteFile, error)
}

// Uploader is an OPTIONAL capability: Put and Create, given the facts an upload
// can carry and the plain methods have no room for. A store that offers it is
// uploaded to through it; one that does not gets Put and Create as before, and
// loses only what the options would have bought.
//
// It exists for two of them. Modified, because a Put cannot set an object's
// modification time, so every remote file was stamped with its upload time and
// every other client dated the file to the upload. And Replaces, because the
// engine checks the remote against its baseline with a Stat before replacing
// it, and a write landing between that Stat and the end of the upload — the
// whole duration of a large one — was replaced unseen: two clients' concurrent
// edits ended with one of them gone and no conflict copy (DESIGN.md §6).
//
// Replaces asks the store to make that check as late as it can. A store that
// can refuse before replacing returns ErrChanged and leaves the path alone. One
// that can only find out afterwards — Drive has no conditional write — reports
// the version it displaced in Uploaded.Displaced, readable through
// VersionGetter for as long as the backend keeps it, and the engine keeps it as
// the conflict's losing side. A store that can do neither ignores Replaces, and
// is then exactly as safe as Put.
type Uploader interface {
	Upload(ctx context.Context, path string, r io.Reader, opts UploadOptions) (Uploaded, error)
}

// UploadOptions is what Uploader.Upload is told beyond the path and content.
// The zero value asks for nothing and is Put.
type UploadOptions struct {
	// Create refuses an occupied path with ErrExist, exactly as Creator.Create
	// does, and may do so before reading any content.
	Create bool

	// Modified is the modification time the object should carry, where the
	// store keeps one it can set. It must be set as part of the upload, never as
	// a second request after it: the store's change feed would otherwise report
	// the object between the two, under an identity the engine has no record of,
	// and the engine would read its own upload as somebody else's change. Zero
	// leaves the time to the store.
	Modified time.Time

	// Replaces is the object the caller last saw at the path and means to
	// replace: compared by Hash where both have one, else by Version, as the
	// engine's own baselines are. Nil replaces whatever is there. Ignored with
	// Create.
	Replaces *RemoteFile
}

// Matches reports whether got is the content want describes, by the identity
// UploadOptions.Replaces is compared with — the engine's own baseline rule:
// the content checksum where either side has one, else the opaque version.
// Two objects with neither are never the same, which is the safe answer for a
// question whose wrong "yes" overwrites somebody's work.
func Matches(want, got RemoteFile) bool {
	if want.IsDir != got.IsDir {
		return false
	}
	if want.Hash != "" || got.Hash != "" {
		return want.Hash == got.Hash
	}
	return want.Version != "" && want.Version == got.Version
}

// Uploaded is the result of an Upload.
type Uploaded struct {
	File RemoteFile

	// Displaced is the version this upload replaced when that was not
	// Replaces, for a store that can tell only after the fact. Its Version is a
	// name VersionGetter.GetVersion accepts. Nil when nothing unexpected was
	// replaced, or when the store cannot say.
	Displaced *RemoteFile
}

// VersionGetter is an OPTIONAL capability: reading a version of an object
// other than its current content — the one an Upload displaced. version is a
// RemoteFile.Version the store handed out in Uploaded.Displaced; any other value
// may be refused. A store with no history does not implement it.
type VersionGetter interface {
	GetVersion(ctx context.Context, path, version string) (io.ReadCloser, error)
}
