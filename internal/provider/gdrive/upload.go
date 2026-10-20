// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package gdrive

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"google.golang.org/api/drive/v3"

	"github.com/syshlted/drivel/provider"
)

// Upload implements provider.Uploader.
//
// Modified becomes the file's modifiedTime, in the request that carries the
// content. Replaces cannot be checked before the write: Drive v3 has no
// conditional update — v2's etag and If-Match did not survive into v3 — so the
// engine's Stat before the upload was the last look anybody got, and a write
// landing during a long upload was replaced unseen. What Drive does keep is
// every revision, so the check is made afterwards instead (displacedBy), and a
// displaced edit is reported for the engine to keep as the conflict's loser.
func (d *Drive) Upload(ctx context.Context, p string, r io.Reader, opts provider.UploadOptions) (provider.Uploaded, error) {
	return d.upload(ctx, p, r, opts)
}

// revisionVersion marks a RemoteFile.Version naming a revision rather than a
// file's version counter, which is what GetVersion accepts.
const revisionVersion = "rev:"

// displacedBy reports the revision an update of p replaced, when that was not
// the content the caller expected (want), or nil.
//
// Every change to a file moves its version counter by at least one, so a new
// version exactly one past the one the caller saw means nothing else touched
// the file in between and no request is needed — the common case. Any larger
// step lists the revisions and looks at the one just before the update's own:
// if it holds what want describes, whatever moved the counter was not content.
// A failure to look is logged and reported as nothing displaced, which is no
// worse than the Put this replaces; the upload itself has succeeded.
func (d *Drive) displacedBy(ctx context.Context, p string, res *drive.File, want provider.RemoteFile) *provider.RemoteFile {
	if before, err := strconv.ParseInt(want.Version, 10, 64); err == nil && res.Version == before+1 {
		return nil
	}
	if res.HeadRevisionId == "" {
		d.logf("[drive] %s: no head revision reported, so whether this upload replaced somebody else's edit is unknown", p)
		return nil
	}
	revs, err := d.revisions(ctx, res.Id)
	if err != nil {
		d.logf("[drive] %s: listing revisions to check for an edit this upload replaced: %v", p, err)
		return nil
	}
	head := -1
	for i, r := range revs {
		if r.Id == res.HeadRevisionId {
			head = i
		}
	}
	if head < 1 {
		// Ours is missing, or nothing precedes it that Drive still keeps.
		return nil
	}
	prev := revs[head-1]
	if prev.Md5Checksum == "" || prev.Md5Checksum == want.Hash || prev.Md5Checksum == res.Md5Checksum {
		return nil
	}
	var modified time.Time
	if prev.ModifiedTime != "" {
		modified, _ = time.Parse(time.RFC3339, prev.ModifiedTime)
	}
	return &provider.RemoteFile{
		Path:     p,
		Size:     prev.Size,
		Hash:     prev.Md5Checksum,
		Version:  revisionVersion + prev.Id,
		Modified: modified,
	}
}

// revisions lists every revision Drive keeps for fileID, oldest first.
func (d *Drive) revisions(ctx context.Context, fileID string) ([]*drive.Revision, error) {
	var out []*drive.Revision
	token := ""
	for {
		call := d.svc.Revisions.List(fileID).PageSize(1000).
			Fields("nextPageToken,revisions(id,md5Checksum,size,modifiedTime,keepForever)").Context(ctx)
		if token != "" {
			call = call.PageToken(token)
		}
		page, err := call.Do()
		if err != nil {
			return nil, classify(err)
		}
		out = append(out, page.Revisions...)
		if token = page.NextPageToken; token == "" {
			return out, nil
		}
	}
}

// GetVersion implements provider.VersionGetter for the revisions displacedBy
// names.
//
// Drive serves a blob revision's content only once it is marked keep-forever,
// so the revision is marked first and unmarked when the reader is closed: kept,
// it would count against the user's quota for good and use one of the 200
// marks a file can carry. A failure to unmark is logged and left; the revision
// then simply stays.
func (d *Drive) GetVersion(ctx context.Context, p, version string) (io.ReadCloser, error) {
	revID, ok := strings.CutPrefix(version, revisionVersion)
	if !ok || revID == "" {
		return nil, fmt.Errorf("version %q of %s: not a revision this provider reported: %w", version, p, provider.ErrNotExist)
	}
	d.mu.Lock()
	fileID, exists := d.resolveLocked(ctx, p)
	d.mu.Unlock()
	if !exists {
		return nil, fmt.Errorf("version %q of %s: %w", version, p, provider.ErrNotExist)
	}
	keep := func(on bool) error {
		_, err := d.svc.Revisions.Update(fileID, revID, &drive.Revision{KeepForever: on, ForceSendFields: []string{"KeepForever"}}).
			Fields("id,keepForever").Context(ctx).Do()
		return err
	}
	marked := keep(true) == nil
	resp, err := d.svc.Revisions.Get(fileID, revID).Context(ctx).Download() //nolint:bodyclose // closed by revisionBody.Close
	if err != nil {
		if marked {
			_ = keep(false)
		}
		if isNotFound(err) {
			return nil, fmt.Errorf("version %q of %s: %w", version, p, provider.ErrNotExist)
		}
		return nil, classify(err)
	}
	return &revisionBody{ReadCloser: resp.Body, unmark: func() error {
		if !marked {
			return nil
		}
		// Not the caller's context: it may be cancelled by the time the body has
		// been read, and an unmark that cannot run keeps the revision forever.
		uctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err := d.svc.Revisions.Update(fileID, revID, &drive.Revision{KeepForever: false, ForceSendFields: []string{"KeepForever"}}).
			Fields("id,keepForever").Context(uctx).Do()
		return err
	}, logf: d.logf, what: p}, nil
}

// revisionBody is a revision's content that unmarks the revision when closed.
type revisionBody struct {
	io.ReadCloser
	unmark func() error
	logf   func(string, ...any)
	what   string
	done   bool
}

func (b *revisionBody) Close() error {
	err := b.ReadCloser.Close()
	if !b.done {
		b.done = true
		if uerr := b.unmark(); uerr != nil {
			b.logf("[drive] %s: unmarking the displaced revision after reading it: %v (it stays kept)", b.what, uerr)
		}
	}
	return err
}
