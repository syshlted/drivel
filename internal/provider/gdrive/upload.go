// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package gdrive

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"google.golang.org/api/drive/v3"

	"github.com/syshlted/drivel/internal/provider/gdrive/gdconf"
	"github.com/syshlted/drivel/provider"
)

// Upload implements provider.Uploader.
//
// Modified becomes the file's modifiedTime, in the request that carries the
// content. Replaces cannot be checked before the write: Drive v3 has no
// conditional update — v2's etag and If-Match did not survive into v3 — so the
// engine's Stat before the upload was the last look anybody got, and a write
// landing during a long upload was replaced unseen. What Drive does keep is
// every revision, so a rewrite the mount's overwrite-check covers compares the
// file's revisions before and after it (checkBefore, displacedBy), and reports
// any edit it replaced for the engine to keep as a conflict copy.
func (d *Drive) Upload(ctx context.Context, p string, r io.Reader, opts provider.UploadOptions) (provider.Uploaded, error) {
	return d.upload(ctx, p, r, opts)
}

// revisionVersion marks a RemoteFile.Version naming a revision rather than a
// file's version counter, which is what GetVersion accepts.
const revisionVersion = "rev:"

// checkBefore decides whether a rewrite of p (file id) is checked for an edit it
// replaces, and if so lists the revisions the file has before the upload
// begins. It returns the reader the upload must use, because deciding by size
// reads the start of r.
//
// A listing that fails leaves the rewrite unchecked, which is no worse than the
// Put it replaces; the upload goes ahead.
func (d *Drive) checkBefore(ctx context.Context, p, id string, r io.Reader) (io.Reader, []*drive.Revision, bool, error) {
	switch d.overwriteMode {
	case OverwriteOff:
		return r, nil, false, nil
	case OverwriteAlways:
	default:
		// Read up to the threshold and put it back in front: a rewrite that ends
		// before it is a small one. A buffer grown to what was read, not one of
		// the threshold's size, because most rewrites are small.
		var head bytes.Buffer
		n, err := io.CopyN(&head, r, gdconf.OverwriteCheckMinSize)
		r = io.MultiReader(&head, r)
		if errors.Is(err, io.EOF) || (err == nil && n < gdconf.OverwriteCheckMinSize) {
			return r, nil, false, nil
		}
		if err != nil {
			return nil, nil, false, err
		}
	}
	revs, err := d.revisions(ctx, id)
	if err != nil {
		d.logf("[drive] %s: listing revisions before the upload, to find an edit it might replace: %v (this upload is not checked)", p, err)
		return r, nil, false, nil
	}
	return r, revs, true, nil
}

// displacedBy reports the edits an update of p (file id) replaced: revisions
// that exist now and did not when the upload began (before), other than the
// update's own, holding neither its content nor what the caller expected to
// replace (want). Oldest first. A failure to look is logged and reported as
// nothing displaced; the upload itself has succeeded.
//
// By set and never by position, because Drive lists revisions by modifiedTime,
// and an upload sets that from the writer's clock (Upload's Modified): the
// revision listed before ours is whichever was dated earlier, not the one ours
// replaced. Reading the list that way reported an old revision of the file as
// somebody's edit, and would have missed a real edit dated before ours. Nor by
// the file's version counter, which one update moves by anything from one to
// four and which moves again with nobody writing — both measured on the live
// rig (TestLiveUpload).
//
// What the set cannot tell apart is an edit that landed after ours but before
// the second listing: it is reported too, and kept as a copy of what the file
// now holds anyway, which duplicates and loses nothing.
func (d *Drive) displacedBy(ctx context.Context, p, id string, res *drive.File, before []*drive.Revision, want provider.RemoteFile) []provider.RemoteFile {
	after, err := d.revisions(ctx, id)
	if err != nil {
		d.logf("[drive] %s: listing revisions to find an edit this upload replaced: %v (if one landed during it, it is in the file's revision history)", p, err)
		return nil
	}
	known := make(map[string]bool, len(before))
	for _, r := range before {
		known[r.Id] = true
	}
	var out []provider.RemoteFile
	for _, r := range after {
		if known[r.Id] || r.Id == res.HeadRevisionId {
			continue
		}
		// The same bytes as ours or as what we meant to replace: nobody's work
		// is lost with it.
		if r.Md5Checksum == "" || r.Md5Checksum == want.Hash || r.Md5Checksum == res.Md5Checksum {
			continue
		}
		var modified time.Time
		if r.ModifiedTime != "" {
			modified, _ = time.Parse(time.RFC3339Nano, r.ModifiedTime)
		}
		out = append(out, provider.RemoteFile{
			Path:     p,
			Size:     r.Size,
			Hash:     r.Md5Checksum,
			Version:  revisionVersion + r.Id,
			Modified: modified,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Modified.Before(out[j].Modified) })
	return out
}

// revisions lists every revision Drive keeps for fileID, in Drive's order,
// which is by modifiedTime and not by when each was made.
func (d *Drive) revisions(ctx context.Context, fileID string) ([]*drive.Revision, error) {
	var out []*drive.Revision
	token := ""
	for {
		call := d.svc.Revisions.List(fileID).PageSize(1000).
			Fields("nextPageToken,revisions(id,md5Checksum,size,modifiedTime)").Context(ctx)
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
// names: a download of that revision, while Drive still keeps it (a revision
// that is not the head is purged 30 days after it was replaced).
//
// Nothing is marked keep-forever to read it. Drive serves a superseded
// revision without the mark, and a mark cannot be taken back — Drive refuses
// to set keepForever false again (illegalKeepForeverModification) — so marking
// one would hold it against the user's quota for good and spend one of the 200
// marks a file can carry. Both measured on the live rig (TestLiveUpload).
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
	resp, err := d.svc.Revisions.Get(fileID, revID).Context(ctx).Download() //nolint:bodyclose // the caller closes it
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("version %q of %s: %w", version, p, provider.ErrNotExist)
		}
		return nil, classify(err)
	}
	return resp.Body, nil
}
