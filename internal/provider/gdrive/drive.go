package gdrive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"github.com/zishmusic/drivel/internal/provider"
)

const folderMIME = "application/vnd.google-apps.folder"

// fileFields is the projection we request for any File we care about. Keeping it
// in one place ensures Md5Checksum/Version (needed for echo suppression, §4) are
// always populated.
const fileFields = "id,name,mimeType,md5Checksum,version,modifiedTime,parents,trashed,size"

// Drive implements the path-addressed provider.Store (and provider.ChangeSource)
// against Google Drive API v3.
//
// Drive is natively ID-addressed, so the adapter owns the path↔fileID translation
// that used to live in the sync engine. The index is in-memory for M2; DESIGN.md
// §2.4 replaces it with a bbolt store in M3. A single mutex guards the maps and is
// held across the API calls of a mutation — coarse but correct; the engine drives
// mutations serially in M2, and per-path concurrency lands with the M4 uploader.
type Drive struct {
	svc   *drive.Service
	close func() error // shuts down the HTTP/3 transport
	root  string       // Drive folder ID mapped to the mount root ("" => My Drive root)

	mu       sync.Mutex
	idByPath map[string]string // root-relative slash path -> fileID ("" => root)
	pathByID map[string]string // reverse, for resolving change-feed entries
}

var (
	_ provider.Store        = (*Drive)(nil)
	_ provider.ChangeSource = (*Drive)(nil)
)

// Open authenticates and returns a Drive provider. credentialsPath is a desktop
// OAuth client secret; tokenPath caches the user token across runs. rootID is the
// Drive folder ID mapped to the mount root ("" => My Drive root).
func Open(ctx context.Context, credentialsPath, tokenPath, rootID string) (*Drive, error) {
	client, closer, err := buildHTTPClient(ctx, credentialsPath, tokenPath)
	if err != nil {
		return nil, err
	}
	svc, err := drive.NewService(ctx, option.WithHTTPClient(client))
	if err != nil {
		_ = closer()
		return nil, fmt.Errorf("creating drive service: %w", err)
	}
	return &Drive{
		svc:      svc,
		close:    closer,
		root:     rootID,
		idByPath: map[string]string{"": rootID},
		pathByID: map[string]string{rootID: ""},
	}, nil
}

// Close releases the underlying HTTP/3 transport.
func (d *Drive) Close() error { return d.close() }

// --- provider.Store ---------------------------------------------------------

func (d *Drive) Put(ctx context.Context, p string, r io.Reader) (provider.RemoteFile, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if id, ok := d.idByPath[p]; ok {
		updated, err := d.svc.Files.Update(id, &drive.File{}).Media(r).Fields(fileFields).Context(ctx).Do()
		if err != nil {
			return provider.RemoteFile{}, classify(err)
		}
		return d.remember(p, updated), nil
	}
	parentID, err := d.ensureDirLocked(ctx, path.Dir(p))
	if err != nil {
		return provider.RemoteFile{}, classify(err)
	}
	f := &drive.File{Name: path.Base(p)}
	if parentID != "" {
		f.Parents = []string{parentID}
	}
	created, err := d.svc.Files.Create(f).Media(r).Fields(fileFields).Context(ctx).Do()
	if err != nil {
		return provider.RemoteFile{}, classify(err)
	}
	return d.remember(p, created), nil
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
	return d.remember(p, got), nil
}

func (d *Drive) Move(ctx context.Context, oldPath, newPath string) (provider.RemoteFile, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	id, ok := d.idByPath[oldPath]
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
	d.reindexLocked(oldPath, newPath)
	return d.remember(newPath, moved), nil
}

func (d *Drive) Remove(ctx context.Context, p string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	id, ok := d.idByPath[p]
	if !ok {
		return nil // never uploaded; nothing to remove
	}
	if err := d.svc.Files.Delete(id).Context(ctx).Do(); err != nil {
		return classify(err)
	}
	d.forgetLocked(p)
	return nil
}

func (d *Drive) Get(ctx context.Context, p string) (io.ReadCloser, error) {
	d.mu.Lock()
	id, ok := d.idByPath[p]
	d.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("get: unknown path %q", p)
	}
	// No lock held while the caller streams the body.
	resp, err := d.svc.Files.Get(id).Context(ctx).Download()
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (d *Drive) Stat(ctx context.Context, p string) (provider.RemoteFile, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	id, ok := d.idByPath[p]
	if !ok {
		return provider.RemoteFile{}, false, nil
	}
	got, err := d.svc.Files.Get(id).Fields(fileFields).Context(ctx).Do()
	if err != nil {
		if isNotFound(err) {
			d.forgetLocked(p)
			return provider.RemoteFile{}, false, nil
		}
		return provider.RemoteFile{}, false, err
	}
	if got.Trashed {
		return provider.RemoteFile{}, false, nil
	}
	return d.remember(p, got), true, nil
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
			return nil, "", err
		}
		for _, ch := range res.Changes {
			rc, ok := d.toRemoteChange(ctx, ch)
			if ok {
				out = append(out, rc)
			}
		}
		if res.NextPageToken != "" {
			page = res.NextPageToken
			continue
		}
		return out, res.NewStartPageToken, nil
	}
}

// toRemoteChange resolves a Drive change into a path-addressed RemoteChange. It
// returns ok=false for changes outside our root subtree (unresolvable path), which
// the caller skips.
func (d *Drive) toRemoteChange(ctx context.Context, ch *drive.Change) (provider.RemoteChange, bool) {
	// Removal: the file record is gone, so we can only resolve the path from our
	// reverse index. Unknown => not in our subtree => skip.
	if ch.Removed || ch.File == nil || ch.File.Trashed {
		d.mu.Lock()
		p, known := d.pathByID[ch.FileId]
		if known {
			d.forgetLocked(p)
		}
		d.mu.Unlock()
		if !known {
			return provider.RemoteChange{}, false
		}
		return provider.RemoteChange{Path: p, Removed: true}, true
	}

	parentID := ""
	if len(ch.File.Parents) > 0 {
		parentID = ch.File.Parents[0]
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	dir, ok := d.pathForIDLocked(ctx, parentID)
	if !ok {
		return provider.RemoteChange{}, false // outside our root subtree
	}
	p := path.Join(dir, ch.File.Name)
	rf := d.remember(p, ch.File)
	return provider.RemoteChange{Path: p, File: &rf}, true
}

// --- index helpers (all callers hold d.mu except Get) ----------------------

// remember records the path↔ID mapping and returns the RemoteFile view.
func (d *Drive) remember(p string, f *drive.File) provider.RemoteFile {
	d.idByPath[p] = f.Id
	d.pathByID[f.Id] = p
	return toRemoteFile(p, f)
}

// forgetLocked drops p (and, for a directory, all descendants) from the index.
func (d *Drive) forgetLocked(p string) {
	prefix := p + "/"
	for q, id := range d.idByPath {
		if q == p || strings.HasPrefix(q, prefix) {
			delete(d.idByPath, q)
			delete(d.pathByID, id)
		}
	}
}

// reindexLocked rewrites index keys after moving oldPath -> newPath, including any
// descendants of a moved directory.
func (d *Drive) reindexLocked(oldPath, newPath string) {
	oldPrefix := oldPath + "/"
	moves := map[string]string{oldPath: newPath}
	for q := range d.idByPath {
		if strings.HasPrefix(q, oldPrefix) {
			moves[q] = newPath + "/" + strings.TrimPrefix(q, oldPrefix)
		}
	}
	for from, to := range moves {
		id := d.idByPath[from]
		delete(d.idByPath, from)
		d.idByPath[to] = id
		d.pathByID[id] = to
	}
}

// ensureDirLocked returns the Drive folder ID for slash dir path p, creating it and
// any missing ancestors. "" / "." => root.
func (d *Drive) ensureDirLocked(ctx context.Context, p string) (string, error) {
	if p == "" || p == "." || p == "/" {
		return d.root, nil
	}
	if id, ok := d.idByPath[p]; ok {
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
	d.remember(p, created)
	return created.Id, nil
}

// pathForIDLocked resolves a folder ID to its root-relative path, walking parents
// and caching results. ok is false if the chain does not reach our root.
func (d *Drive) pathForIDLocked(ctx context.Context, id string) (string, bool) {
	if id == "" || id == d.root {
		return "", true
	}
	if p, ok := d.pathByID[id]; ok {
		return p, true
	}
	got, err := d.svc.Files.Get(id).Fields("name,parents").Context(ctx).Do()
	if err != nil || len(got.Parents) == 0 {
		return "", false
	}
	parent, ok := d.pathForIDLocked(ctx, got.Parents[0])
	if !ok {
		return "", false
	}
	p := path.Join(parent, got.Name)
	d.idByPath[p] = id
	d.pathByID[id] = p
	return p, true
}

func toRemoteFile(p string, f *drive.File) provider.RemoteFile {
	var modified time.Time
	if f.ModifiedTime != "" {
		modified, _ = time.Parse(time.RFC3339, f.ModifiedTime)
	}
	return provider.RemoteFile{
		Path:     p,
		IsDir:    f.MimeType == folderMIME,
		Size:     f.Size,
		Hash:     f.Md5Checksum,
		Version:  strconv.FormatInt(f.Version, 10),
		Modified: modified,
	}
}

func isNotFound(err error) bool {
	var ae *googleapi.Error
	if errors.As(err, &ae) {
		return ae.Code == 404
	}
	return false
}
