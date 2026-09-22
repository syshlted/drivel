// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package sftp

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/zishmusic/drivel/provider"
	"github.com/zishmusic/drivel/ranges"
)

// GetRange reads a byte range of p (provider.RangeGetter, M5).
//
// SSH_FXP_READ takes an offset, so this is one seek and a bounded read rather
// than a download with a discarded prefix — which the provider guide singles out
// as worse than not implementing the interface at all, because the hydrator would
// then choose the ranged path believing it cheap.
//
// length <= 0 means "to end of object".
func (s *Store) GetRange(ctx context.Context, p string, off, length int64) (io.ReadCloser, error) {
	return do(ctx, s, func(c *conn) (io.ReadCloser, error) {
		abs := c.abs(p)
		f, err := c.cli.Open(abs)
		if err != nil {
			return nil, fmt.Errorf("opening %s: %w", abs, err)
		}
		if off > 0 {
			if _, err := f.Seek(off, io.SeekStart); err != nil {
				_ = f.Close()
				return nil, fmt.Errorf("seeking %s to %d: %w", abs, off, err)
			}
		}
		if length <= 0 {
			return f, nil
		}
		// Reading past the end is not an error — the contract says a short object
		// is not one — so the limit clamps rather than validates.
		return boundedFile{Reader: io.LimitReader(f, length), Closer: f}, nil
	})
}

// boundedFile bounds how much of a remote handle may be read while still closing
// the handle itself: io.LimitReader alone is not a Closer, and leaking the handle
// would leak a request slot on the session.
type boundedFile struct {
	io.Reader
	io.Closer
}

// PutRange replaces byte extents of p in place (provider.RangePutter, M6).
//
// This is the milestone's headline. SSH_FXP_WRITE takes an offset, so the seam's
// contract — "replace exactly these extents, neither create nor resize" — maps
// onto the protocol directly, and M6's range-write path finally runs against a
// real server instead of a fake. Drive has no partial write at all, so before
// this backend every byte of gate 2 was exercised by test doubles.
//
// The contract is re-checked here rather than trusted. The engine already
// verifies that the object exists at exactly size and that the remote still
// matches the §4 echo before calling — the second of those is what stops extents
// being spliced into a file someone else has edited, producing a hybrid that
// existed nowhere — but the seam says the implementation must re-check too, and
// the reason is sharp: a mismatch means our idea of the file's length is stale,
// and every offset computed from it lands in the wrong place. Failing is cheap.
// The engine logs it and re-does the write as a whole-file Put.
func (s *Store) PutRange(ctx context.Context, p string, src io.ReaderAt, size int64, extents []ranges.Range) (provider.RemoteFile, error) {
	return do(ctx, s, func(c *conn) (provider.RemoteFile, error) {
		abs := c.abs(p)
		// Validate before opening: an invalid extent list must not leave a handle
		// open on a file it was never going to be allowed to touch.
		var last int64
		for _, ext := range extents {
			switch {
			case ext.Len <= 0:
				return provider.RemoteFile{}, fmt.Errorf("extent at %d has length %d", ext.Off, ext.Len)
			case ext.Off < last:
				return provider.RemoteFile{}, fmt.Errorf("extent at %d overlaps or precedes the previous one", ext.Off)
			case ext.Off+ext.Len > size:
				return provider.RemoteFile{}, fmt.Errorf("extent %d..%d runs past the file's %d bytes", ext.Off, ext.Off+ext.Len, size)
			}
			last = ext.Off + ext.Len
		}
		if len(extents) == 0 {
			return provider.RemoteFile{}, fmt.Errorf("no extents to write")
		}

		// O_WRONLY and never O_TRUNC: this call may not resize the file, and
		// O_CREATE is equally wrong — an object that has gone missing is a whole-
		// file Put's problem, not something to recreate with holes in it.
		f, err := c.cli.OpenFile(abs, os.O_WRONLY)
		if err != nil {
			return provider.RemoteFile{}, fmt.Errorf("opening %s for a range write: %w", abs, err)
		}
		defer f.Close() //nolint:errcheck // the Stat below is what reports success

		fi, err := f.Stat()
		if err != nil {
			return provider.RemoteFile{}, fmt.Errorf("checking the size of %s: %w", abs, err)
		}
		if fi.Size() != size {
			return provider.RemoteFile{}, fmt.Errorf("%s is %d bytes remotely, not the %d this write was computed against", abs, fi.Size(), size)
		}

		for _, ext := range extents {
			if err := ctx.Err(); err != nil {
				return provider.RemoteFile{}, err
			}
			if _, err := f.Seek(ext.Off, io.SeekStart); err != nil {
				return provider.RemoteFile{}, fmt.Errorf("seeking %s to %d: %w", abs, ext.Off, err)
			}
			// io.Copy hands off to File.ReadFrom, which writes from the handle's
			// current offset and may pipeline several packets; the SectionReader
			// bounds it to exactly this extent, so nothing can run past the file's
			// length and resize it.
			n, err := io.Copy(f, io.NewSectionReader(src, ext.Off, ext.Len))
			if err != nil {
				return provider.RemoteFile{}, fmt.Errorf("writing %d bytes to %s at %d: %w", ext.Len, abs, ext.Off, err)
			}
			if n != ext.Len {
				// A short local read means the file shrank under us, so the extents
				// describe a file nobody is holding any more.
				return provider.RemoteFile{}, fmt.Errorf("extent at %d: read %d of %d bytes locally", ext.Off, n, ext.Len)
			}
		}
		if err := f.Close(); err != nil {
			return provider.RemoteFile{}, fmt.Errorf("closing %s: %w", abs, err)
		}
		return c.statFile(p, abs)
	})
}
