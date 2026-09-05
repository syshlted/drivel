package gdrive

import (
	"context"
	"crypto/md5" //nolint:gosec // G501: Drive addresses content by MD5; see HashContent
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"github.com/zishmusic/drivel/internal/pathindex"
	"github.com/zishmusic/drivel/internal/provider"
)

const folderMIME = "application/vnd.google-apps.folder"

// Upload tuning for the resumable sessions the generated client runs whenever a
// payload exceeds uploadChunkSize (DESIGN.md §9, M6).
//
// Chunking is what makes a large upload survivable: each chunk is committed
// separately and retried on its own, so a dropped connection costs one chunk
// rather than the file. The session URI is not persisted, so this covers a flaky
// network, not a drivel restart — resuming across process lifetimes is
// deliberately out of M6.
const (
	// 16 MiB, the client's own default, stated explicitly so a library change
	// cannot silently move it. Larger chunks mean fewer round-trips; smaller ones
	// mean less work lost per failure.
	uploadChunkSize = 16 << 20
	// How long one chunk may keep failing and retrying before the error reaches
	// the engine, which then applies its own backoff across the whole operation.
	// The library's default is 32s — barely one 429 backoff — so a brief rate
	// limit aborts a chunk that would have succeeded moments later.
	uploadChunkRetryDeadline = 5 * time.Minute
)

// mediaOptions are the upload options applied to every content push.
//
// EnableAutoChecksum makes Drive verify what it assembled: on a multi-chunk
// resumable upload the client sends a checksum with the final request and the
// server rejects a mismatch, so a corrupted chunk fails the upload instead of
// silently becoming the file's new content.
//
// Note what is deliberately NOT set: googleapi.ChunkTransferTimeout. It reads
// like a stall detector but is a hard per-attempt wall-clock deadline that never
// resets on progress, so any value for it silently caps the slowest link that can
// ever finish a chunk (16 MiB in 2 minutes is a ~1.1 Mbps floor). A slow upload
// would then fail permanently rather than merely take a while. A genuinely dead
// peer is already caught below us: the QUIC transport runs keepalives and its own
// idle timeout (see internal/transport), and the engine retries the whole
// operation on top. Only set this if a stalled-forever upload is ever actually
// observed, and then pick the value from the slowest link worth supporting —
// remembering it must stay well under uploadChunkRetryDeadline or the retry it
// is supposed to trigger can never run.
func mediaOptions() []googleapi.MediaOption {
	return []googleapi.MediaOption{
		googleapi.ChunkSize(uploadChunkSize),
		googleapi.ChunkRetryDeadline(uploadChunkRetryDeadline),
		googleapi.EnableAutoChecksum(),
	}
}

// fileFields is the projection we request for any File we care about. Keeping it
// in one place ensures Md5Checksum/Version (needed for echo suppression, §4) are
// always populated.
const fileFields = "id,name,mimeType,md5Checksum,version,modifiedTime,parents,trashed,size"

// Drive implements the path-addressed provider.Store (and provider.ChangeSource)
// against Google Drive API v3.
//
// Drive is natively ID-addressed, so the adapter owns the path↔fileID translation
// that the seam above it never sees. index.go holds that translation: two
// in-memory maps in front of an optional persistent index, both of them caches
// over Drive itself. A single mutex guards the maps and is held across the API
// calls of a mutation — coarse but correct, and unchanged by M7: resolution may
// now issue requests of its own, but each path is resolved at most once per
// session and the engine's own per-path workers serialise on it anyway.
type Drive struct {
	svc   *drive.Service
	close func() error // shuts down the HTTP/3 transport
	root  string       // Drive folder ID mapped to the mount root ("" => My Drive root)

	// lg is where this instance writes. Per instance rather than the package
	// default because one process may hold several Drives, and a line that does
	// not say which mount it came from is close to useless (M8).
	lg *log.Logger

	mu       sync.Mutex
	rootID   string            // concrete ID of root, which may be the "root" alias
	idByPath map[string]string // root-relative slash path -> fileID ("" => root)
	pathByID map[string]string // reverse, for resolving change-feed entries

	// kids indexes idByPath by parent directory, so dropping or moving a subtree
	// costs that subtree instead of a scan of every path we know. See the child
	// index note in index.go — it is maintained only by linkLocked/unlinkLocked.
	kids map[string]map[string]struct{}

	// Persistent path↔ID index (M7): a cache of the two maps above that outlives
	// the process. Never authoritative — see the note at the top of index.go.
	idx       *pathindex.Store
	idxState  indexState
	idxWarned bool

	// In-flight enumeration sweep (M7b), nil when none is running. See
	// enumerate.go — it is derived state, rebuilt or restarted after a restart.
	sweep *sweepState
}

var (
	_ provider.Store         = (*Drive)(nil)
	_ provider.ChangeSource  = (*Drive)(nil)
	_ provider.Enumerator    = (*Drive)(nil)
	_ provider.RangeGetter   = (*Drive)(nil)
	_ provider.ContentHasher = (*Drive)(nil)
)

// provider.RangePutter is deliberately NOT implemented, and the absence is the
// design, not an omission to fill in later.
//
// Drive has no partial-content write. files.update replaces an object's content
// wholesale; its resumable upload protocol chunks the transfer but every chunk
// still belongs to one complete new body, with no way to say "keep bytes 0..N and
// replace only these". So editing one byte of a large file costs a full re-upload
// on this provider no matter how precisely the engine knows what changed.
//
// What M6 does buy a Drive user is the other two gates in Engine.pushContent: the
// unchanged-content hash check below skips the upload entirely when the bytes did
// not really change, and mediaOptions makes the unavoidable large upload chunked
// and retryable. The RangePutter path stays exercised by providers that can
// patch — see the range-write tests in internal/syncengine.

// Config is what a Drive provider needs to open.
// The struct tags are what a `[mount.provider]` table in drivel's config file
// decodes into (M8). They exist because this IS the provider's configuration —
// the same reason a type carries json tags — and naming the keys explicitly beats
// the decoder's default case-insensitive field matching, which would spell
// RootID as "rootid".
type Config struct {
	// Credentials is the desktop OAuth client secret JSON.
	Credentials string `toml:"credentials"`
	// Token caches the user token across runs (written by `drivel login`).
	Token string `toml:"token"`
	// RootID is the Drive folder ID mapped to the mount root ("" or "root" => My
	// Drive root).
	RootID string `toml:"root"`
	// IndexPath is where the persistent path↔fileID index lives (M7). Empty
	// disables persistence, which costs API round trips and nothing else. It must
	// not sit inside the backing tree, or it would sync itself to Drive.
	IndexPath string `toml:"index"`
	// Scope is the OAuth scope the token was granted, as `drivel login` recorded
	// it. Empty means gauth.ScopeDrive.
	//
	// It matters little to a refresh — the refresh token carries the scopes the
	// user actually consented to, whatever we ask for — but asking for a scope the
	// user declined is a lie in the one place a reader would go to find out what
	// this mount can do. A read-only account should say so.
	Scope string `toml:"scope"`
}

// Open authenticates and returns a Drive provider that logs to the default
// logger. Callers that own a per-mount logger go through Factory instead.
func Open(ctx context.Context, cfg Config) (*Drive, error) {
	return open(ctx, cfg, nil)
}

func open(ctx context.Context, cfg Config, lg *log.Logger) (*Drive, error) {
	if lg == nil {
		lg = log.Default()
	}
	client, closer, err := buildHTTPClient(ctx, cfg.Credentials, cfg.Token, cfg.Scope)
	if err != nil {
		return nil, err
	}
	svc, err := drive.NewService(ctx, option.WithHTTPClient(client))
	if err != nil {
		_ = closer()
		return nil, fmt.Errorf("creating drive service: %w", err)
	}
	d := &Drive{
		svc:      svc,
		close:    closer,
		lg:       lg,
		root:     cfg.RootID,
		idByPath: map[string]string{"": cfg.RootID},
		pathByID: map[string]string{cfg.RootID: ""},
		kids:     map[string]map[string]struct{}{},
	}
	if cfg.IndexPath != "" {
		// A failure here is not fatal: the index only ever saves work, so we log it
		// and run from memory, exactly as M2-M6 did.
		idx, err := pathindex.Open(cfg.IndexPath)
		if err != nil {
			lg.Printf("[drive] path index: disabled: %v", err)
		} else {
			d.idx = idx
		}
	}
	return d, nil
}

// Close releases the underlying HTTP/3 transport and the persistent index.
func (d *Drive) Close() error {
	err := d.close()
	if d.idx != nil {
		if cerr := d.idx.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}
	return err
}

// --- provider.Store ---------------------------------------------------------

func (d *Drive) Put(ctx context.Context, p string, r io.Reader) (provider.RemoteFile, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Resolve before creating. An unresolvable path is genuinely absent remotely;
	// anything less than that check and a restart would upload a duplicate beside
	// the real file instead of replacing it (see index.go).
	if id, ok := d.resolveLocked(ctx, p); ok {
		updated, err := d.svc.Files.Update(id, &drive.File{}).Media(r, mediaOptions()...).Fields(fileFields).Context(ctx).Do()
		if err != nil {
			return provider.RemoteFile{}, classify(err)
		}
		return d.rememberLocked(ctx, p, updated), nil
	}
	parentID, err := d.ensureDirLocked(ctx, path.Dir(p))
	if err != nil {
		return provider.RemoteFile{}, classify(err)
	}
	f := &drive.File{Name: path.Base(p)}
	if parentID != "" {
		f.Parents = []string{parentID}
	}
	created, err := d.svc.Files.Create(f).Media(r, mediaOptions()...).Fields(fileFields).Context(ctx).Do()
	if err != nil {
		return provider.RemoteFile{}, classify(err)
	}
	return d.rememberLocked(ctx, p, created), nil
}

func (d *Drive) Mkdir(ctx context.Context, p string) (provider.RemoteFile, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	id, err := d.ensureDirLocked(ctx, p)
	if err != nil {
		return provider.RemoteFile{}, classify(err)
	}
	got, err := d.svc.Files.Get(id).Fields(fileFields).Context(ctx).Do()
	if err != nil {
		return provider.RemoteFile{}, classify(err)
	}
	return d.rememberLocked(ctx, p, got), nil
}

func (d *Drive) Move(ctx context.Context, oldPath, newPath string) (provider.RemoteFile, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	id, ok := d.resolveLocked(ctx, oldPath)
	if !ok {
		return provider.RemoteFile{}, provider.ErrNotExist
	}
	newParentID, err := d.ensureDirLocked(ctx, path.Dir(newPath))
	if err != nil {
		return provider.RemoteFile{}, classify(err)
	}
	// Drive expresses a move as add/remove-parents; fetch current parents to remove.
	cur, err := d.svc.Files.Get(id).Fields("parents").Context(ctx).Do()
	if err != nil {
		return provider.RemoteFile{}, classify(err)
	}
	call := d.svc.Files.Update(id, &drive.File{Name: path.Base(newPath)}).Fields(fileFields)
	if newParentID != "" {
		call = call.AddParents(newParentID)
	}
	if len(cur.Parents) > 0 {
		call = call.RemoveParents(strings.Join(cur.Parents, ","))
	}
	moved, err := call.Context(ctx).Do()
	if err != nil {
		return provider.RemoteFile{}, classify(err)
	}
	// Reindex the moved node and, if it's a directory, every descendant whose path
	// carried the old prefix.
	d.reindexLocked(ctx, oldPath, newPath)
	return d.rememberLocked(ctx, newPath, moved), nil
}

func (d *Drive) Remove(ctx context.Context, p string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	id, ok := d.resolveLocked(ctx, p)
	if !ok {
		return nil // no such object remotely; nothing to remove
	}
	if err := d.svc.Files.Delete(id).Context(ctx).Do(); err != nil {
		return classify(err)
	}
	d.forgetLocked(ctx, p)
	return nil
}

func (d *Drive) Get(ctx context.Context, p string) (io.ReadCloser, error) {
	d.mu.Lock()
	id, ok := d.resolveLocked(ctx, p)
	d.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("get %q: %w", p, provider.ErrNotExist)
	}
	// No lock held while the caller streams the body.
	resp, err := d.svc.Files.Get(id).Context(ctx).Download()
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// GetRange implements provider.RangeGetter: a ranged download of p. Drive honours
// a standard HTTP Range header on an alt=media download and answers 206, which
// googleapi accepts as success. A server that ignores the header answers 200 with
// the whole object; callers must therefore treat the reader as "at most what was
// asked for, starting at off" and stop reading at length themselves.
func (d *Drive) GetRange(ctx context.Context, p string, off, length int64) (io.ReadCloser, error) {
	d.mu.Lock()
	id, ok := d.resolveLocked(ctx, p)
	d.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("get range %q: %w", p, provider.ErrNotExist)
	}
	if off < 0 {
		off = 0
	}
	call := d.svc.Files.Get(id).Context(ctx)
	if length > 0 {
		call.Header().Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+length-1))
	} else {
		call.Header().Set("Range", fmt.Sprintf("bytes=%d-", off))
	}
	// No lock held while the caller streams the body.
	resp, err := call.Download()
	if err != nil {
		return nil, classify(err)
	}
	return resp.Body, nil
}

// HashContent implements provider.ContentHasher: Drive's md5Checksum, which is
// the MD5 of the file's bytes in lowercase hex. It is used only to compare local
// content against a checksum Drive already reported, never as a security
// property, so MD5's collision weakness is not in play here.
func (d *Drive) HashContent(r io.Reader) (string, error) {
	h := md5.New() //nolint:gosec // G401: comparison against Drive's md5Checksum, not a security property
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (d *Drive) Stat(ctx context.Context, p string) (provider.RemoteFile, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	id, ok := d.resolveLocked(ctx, p)
	if !ok {
		return provider.RemoteFile{}, false, nil
	}
	got, err := d.svc.Files.Get(id).Fields(fileFields).Context(ctx).Do()
	if err != nil {
		if isNotFound(err) {
			d.forgetLocked(ctx, p)
			return provider.RemoteFile{}, false, nil
		}
		return provider.RemoteFile{}, false, err
	}
	if got.Trashed {
		return provider.RemoteFile{}, false, nil
	}
	return d.rememberLocked(ctx, p, got), true, nil
}

// --- provider.ChangeSource --------------------------------------------------

func (d *Drive) StartCursor(ctx context.Context) (string, error) {
	res, err := d.svc.Changes.GetStartPageToken().Context(ctx).Do()
	if err != nil {
		return "", err
	}
	return res.StartPageToken, nil
}

func (d *Drive) Changes(ctx context.Context, cursor string) ([]provider.RemoteChange, string, error) {
	var out []provider.RemoteChange
	page := cursor
	for {
		res, err := d.svc.Changes.List(page).
			Spaces("drive").
			IncludeRemoved(true).
			Fields(googleapi.Field("nextPageToken,newStartPageToken,changes(fileId,removed,file(" + fileFields + "))")).
			Context(ctx).Do()
		if err != nil {
			// A dead token is not a transient failure and retrying it never
			// succeeds: the changes it covered are gone. Say so distinctly so the
			// engine can recover the only way that works — re-enumerate (M7b).
			if isPageTokenExpired(err) {
				return nil, "", fmt.Errorf("changes since cursor: %w", provider.ErrCursorExpired)
			}
			return nil, "", classify(err)
		}
		for _, ch := range res.Changes {
			out = append(out, d.toRemoteChanges(ctx, ch)...)
		}
		if res.NextPageToken != "" {
			page = res.NextPageToken
			continue
		}
		return out, res.NewStartPageToken, nil
	}
}

// toRemoteChanges resolves one Drive change into the path-addressed changes it
// means. It returns nothing for changes outside our root subtree (unresolvable
// path), one change for the ordinary case, and two when an object moved — see
// vacatedPathLocked.
func (d *Drive) toRemoteChanges(ctx context.Context, ch *drive.Change) []provider.RemoteChange {
	// Removal: the file record is gone, so we can only resolve the path from our
	// reverse index. Unknown => not in our subtree => skip.
	if ch.Removed || ch.File == nil || ch.File.Trashed {
		d.mu.Lock()
		p, known := d.knownPathForIDLocked(ctx, ch.FileId)
		if known {
			d.forgetLocked(ctx, p)
		}
		d.mu.Unlock()
		if !known {
			return nil
		}
		return []provider.RemoteChange{{Path: p, Removed: true}}
	}

	parentID := ""
	if len(ch.File.Parents) > 0 {
		parentID = ch.File.Parents[0]
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	dir, ok := d.pathForIDLocked(ctx, parentID)
	if !ok {
		return nil // outside our root subtree
	}
	p := path.Join(dir, ch.File.Name)

	// Ask before remembering: rememberLocked is what evicts the old mapping.
	vacated, moved := d.vacatedPathLocked(ch.File, p)

	rf := d.rememberLocked(ctx, p, ch.File)
	if !moved {
		return []provider.RemoteChange{{Path: p, File: &rf}}
	}
	// Removal first: the object has left that path, and the caller applies a page
	// in order.
	return []provider.RemoteChange{
		{Path: vacated, Removed: true},
		{Path: p, File: &rf},
	}
}

// vacatedPathLocked reports the path f has just left, when it left one.
//
// Drive's feed is keyed by object identity: a rename or a move is one change
// carrying the object under its *new* name, and there is no event anywhere saying
// the old path is now empty. provider.RemoteChange carries no identity, so
// nothing above the seam can work that out either — the engine sees a file appear
// at a new path and has no reason to touch the old one. Left alone, every rename
// on one client duplicates the file on every other until an enumeration sweep
// infers the delete, up to -sweep-interval later (24h by default).
//
// This is where the fact is known, so this is where it is said. Two limits, both
// of which cost a sweep rather than risking data:
//
//   - Directories are excluded. A removal above the seam is a recursive local
//     delete, and Drive reports no changes for the children of a moved folder —
//     their own metadata did not change — so the subtree would be deleted locally
//     and not come back until a sweep. Leaving the stale copy is the lesser
//     failure. A folder move is still reconciled, just not by the feed.
//   - Only the in-memory mapping is consulted, never the persistent index. Acting
//     on it here means deleting a local file, and M7's first rule is that a
//     persisted entry is a hint to be verified before it is believed. The
//     in-memory pair is maintained by linkLocked as an exact inverse, so a path it
//     reports for an ID is one this process itself recorded. A rename whose old
//     path was only ever known to an earlier process therefore falls back to the
//     sweep, as it did before.
func (d *Drive) vacatedPathLocked(f *drive.File, newPath string) (string, bool) {
	if f.MimeType == folderMIME {
		return "", false
	}
	id := f.Id
	if id == "" {
		return "", false
	}
	old, ok := d.pathByID[id]
	if !ok || old == newPath {
		return "", false
	}
	return old, true
}

// ensureDirLocked returns the Drive folder ID for slash dir path p, creating it and
// any missing ancestors. "" / "." => root.
func (d *Drive) ensureDirLocked(ctx context.Context, p string) (string, error) {
	if p == "" || p == "." || p == "/" {
		return d.root, nil
	}
	// Resolve, don't just consult memory: creating a folder that already exists
	// remotely gives Drive two same-named siblings and splits the subtree in half.
	if id, ok := d.resolveLocked(ctx, p); ok {
		return id, nil
	}
	parentID, err := d.ensureDirLocked(ctx, path.Dir(p))
	if err != nil {
		return "", err
	}
	f := &drive.File{Name: path.Base(p), MimeType: folderMIME}
	if parentID != "" {
		f.Parents = []string{parentID}
	}
	created, err := d.svc.Files.Create(f).Fields(fileFields).Context(ctx).Do()
	if err != nil {
		return "", err
	}
	d.rememberLocked(ctx, p, created)
	return created.Id, nil
}

func toRemoteFile(p string, f *drive.File) provider.RemoteFile {
	var modified time.Time
	if f.ModifiedTime != "" {
		modified, _ = time.Parse(time.RFC3339, f.ModifiedTime)
	}
	return provider.RemoteFile{
		Path:       p,
		IsDir:      f.MimeType == folderMIME,
		Size:       f.Size,
		Hash:       f.Md5Checksum,
		Version:    strconv.FormatInt(f.Version, 10),
		Modified:   modified,
		ExportOnly: isExportOnly(f.MimeType),
	}
}

// isExportOnly reports a Google-native object — a Doc, Sheet, Slide, Form or
// shortcut. They carry no md5Checksum and no byte size because they have no byte
// stream: files.get(alt=media) refuses them, and files.export produces a
// *converted* rendering whose bytes are not the object. Reporting the fact is the
// honest answer; guessing a size for a placeholder or a digest for a comparison
// would not be. See DESIGN.md §9 (M7b).
func isExportOnly(mimeType string) bool {
	return mimeType != folderMIME && strings.HasPrefix(mimeType, "application/vnd.google-apps.")
}

func isNotFound(err error) bool {
	var ae *googleapi.Error
	if errors.As(err, &ae) {
		return ae.Code == 404
	}
	return false
}

// logf writes one line for this Drive instance. The nil check keeps a
// zero-valued Drive (which the tests build directly) usable.
func (d *Drive) logf(format string, args ...any) {
	if d.lg == nil {
		log.Printf(format, args...)
		return
	}
	d.lg.Printf(format, args...)
}
