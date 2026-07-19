// Package provider defines the seam between the sync engine and a concrete
// cloud-storage backend. v1 ships a Google Drive implementation (added in M2),
// but the FS and sync layers depend only on this interface so other providers
// can be added later.
package provider

import (
	"context"
	"io"
	"time"
)

// RemoteFile is the provider-agnostic view of a stored object after a write.
type RemoteFile struct {
	ID       string
	Name     string
	ParentID string
	IsDir    bool
	Size     int64
	MD5      string    // content checksum, when the provider exposes one
	Version  string    // opaque provider version/etag
	Modified time.Time
}

// RemoteChange is one entry from the provider's incremental change feed.
type RemoteChange struct {
	FileID  string
	File    *RemoteFile // nil when Removed is true
	Removed bool
}

// Provider is a minimal cloud backend. Implementations must be safe for
// concurrent use: the uploader and downloader call it from different goroutines.
type Provider interface {
	// StartCursor returns an opaque token marking "now" in the change feed.
	StartCursor(ctx context.Context) (string, error)

	// Changes returns changes since cursor and the cursor to use next time.
	Changes(ctx context.Context, cursor string) (changes []RemoteChange, next string, err error)

	Mkdir(ctx context.Context, parentID, name string) (RemoteFile, error)
	Upload(ctx context.Context, parentID, name string, r io.Reader) (RemoteFile, error)
	Update(ctx context.Context, fileID string, r io.Reader) (RemoteFile, error)
	Move(ctx context.Context, fileID, newParentID, newName string) (RemoteFile, error)
	Delete(ctx context.Context, fileID string) error
	Download(ctx context.Context, fileID string) (io.ReadCloser, error)
}
