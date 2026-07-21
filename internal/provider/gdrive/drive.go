package gdrive

import (
	"context"
	"fmt"
	"io"
	"strconv"
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

// Drive implements provider.Provider against Google Drive API v3.
type Drive struct {
	svc   *drive.Service
	close func() error // shuts down the HTTP/3 transport
}

var _ provider.Provider = (*Drive)(nil)

// Open authenticates and returns a Drive provider. credentialsPath is a desktop
// OAuth client secret; tokenPath caches the user token across runs.
func Open(ctx context.Context, credentialsPath, tokenPath string) (*Drive, error) {
	client, closer, err := buildHTTPClient(ctx, credentialsPath, tokenPath)
	if err != nil {
		return nil, err
	}
	svc, err := drive.NewService(ctx, option.WithHTTPClient(client))
	if err != nil {
		_ = closer()
		return nil, fmt.Errorf("creating drive service: %w", err)
	}
	return &Drive{svc: svc, close: closer}, nil
}

// Close releases the underlying HTTP/3 transport.
func (d *Drive) Close() error { return d.close() }

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
			rc := provider.RemoteChange{FileID: ch.FileId, Removed: ch.Removed}
			if ch.File != nil {
				// A trashed file is, for sync purposes, a removal.
				if ch.File.Trashed {
					rc.Removed = true
				} else {
					f := toRemoteFile(ch.File)
					rc.File = &f
				}
			}
			out = append(out, rc)
		}
		if res.NextPageToken != "" {
			page = res.NextPageToken
			continue
		}
		return out, res.NewStartPageToken, nil
	}
}

func (d *Drive) Mkdir(ctx context.Context, parentID, name string) (provider.RemoteFile, error) {
	f := &drive.File{Name: name, MimeType: folderMIME}
	if parentID != "" {
		f.Parents = []string{parentID}
	}
	created, err := d.svc.Files.Create(f).Fields(fileFields).Context(ctx).Do()
	if err != nil {
		return provider.RemoteFile{}, err
	}
	return toRemoteFile(created), nil
}

func (d *Drive) Upload(ctx context.Context, parentID, name string, r io.Reader) (provider.RemoteFile, error) {
	f := &drive.File{Name: name}
	if parentID != "" {
		f.Parents = []string{parentID}
	}
	created, err := d.svc.Files.Create(f).Media(r).Fields(fileFields).Context(ctx).Do()
	if err != nil {
		return provider.RemoteFile{}, err
	}
	return toRemoteFile(created), nil
}

func (d *Drive) Update(ctx context.Context, fileID string, r io.Reader) (provider.RemoteFile, error) {
	updated, err := d.svc.Files.Update(fileID, &drive.File{}).Media(r).Fields(fileFields).Context(ctx).Do()
	if err != nil {
		return provider.RemoteFile{}, err
	}
	return toRemoteFile(updated), nil
}

func (d *Drive) Move(ctx context.Context, fileID, newParentID, newName string) (provider.RemoteFile, error) {
	// Drive moves are expressed as add/remove parents; fetch current parents to
	// remove them.
	cur, err := d.svc.Files.Get(fileID).Fields("parents").Context(ctx).Do()
	if err != nil {
		return provider.RemoteFile{}, err
	}
	call := d.svc.Files.Update(fileID, &drive.File{Name: newName}).Fields(fileFields)
	if newParentID != "" {
		call = call.AddParents(newParentID)
	}
	if len(cur.Parents) > 0 {
		call = call.RemoveParents(joinParents(cur.Parents))
	}
	moved, err := call.Context(ctx).Do()
	if err != nil {
		return provider.RemoteFile{}, err
	}
	return toRemoteFile(moved), nil
}

func (d *Drive) Delete(ctx context.Context, fileID string) error {
	return d.svc.Files.Delete(fileID).Context(ctx).Do()
}

func (d *Drive) Download(ctx context.Context, fileID string) (io.ReadCloser, error) {
	resp, err := d.svc.Files.Get(fileID).Context(ctx).Download()
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func toRemoteFile(f *drive.File) provider.RemoteFile {
	var modified time.Time
	if f.ModifiedTime != "" {
		modified, _ = time.Parse(time.RFC3339, f.ModifiedTime)
	}
	rf := provider.RemoteFile{
		ID:       f.Id,
		Name:     f.Name,
		IsDir:    f.MimeType == folderMIME,
		Size:     f.Size,
		MD5:      f.Md5Checksum,
		Version:  strconv.FormatInt(f.Version, 10),
		Modified: modified,
	}
	if len(f.Parents) > 0 {
		rf.ParentID = f.Parents[0]
	}
	return rf
}

// joinParents formats a parent-ID list for RemoveParents (comma-separated).
func joinParents(ids []string) string {
	out := ids[0]
	for _, id := range ids[1:] {
		out += "," + id
	}
	return out
}
