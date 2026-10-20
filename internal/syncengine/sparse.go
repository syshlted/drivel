// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package syncengine

import (
	"bytes"
	"errors"
	"io"
	"os"
)

// A download used to write every byte it received, so a sparse file — a disk
// image, a database with preallocated pages — arrived fully allocated on every
// machine but the one it was made on. The backends carry no notion of a hole,
// and Drive's content is a plain byte stream, so the hole cannot be restored
// from metadata; but an all-zero block reads the same whether it is stored or
// not, so not storing it is free. This is cp --sparse=auto's rule, at the
// granularity a filesystem allocates in.

// sparseBlock is the run of zeros worth leaving unwritten: the page size and the
// usual filesystem block. A smaller run cannot become a hole anyway.
const sparseBlock = 4096

var zeroBlock [sparseBlock]byte

// copySparse copies r into f, a freshly created and therefore empty file,
// leaving every all-zero sparseBlock unwritten so that the filesystem keeps it
// as a hole. It returns the number of bytes r produced, which is the file's
// length afterwards: a trailing hole is never written, so the length is set
// explicitly at the end. On a filesystem with no holes the unwritten blocks
// are simply zeros, which is what they were.
func copySparse(f *os.File, r io.Reader) (int64, error) {
	buf := make([]byte, 32*sparseBlock)
	var off int64
	for {
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			if werr := writeNonZero(f, buf[:n], off); werr != nil {
				return off, werr
			}
			off += int64(n)
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			return off, err
		}
	}
	return off, f.Truncate(off)
}

// writeNonZero writes b to f at off, skipping its all-zero blocks and joining
// the rest into as few writes as they allow.
func writeNonZero(f *os.File, b []byte, off int64) error {
	for i := 0; i < len(b); {
		end := min(i+sparseBlock, len(b))
		if bytes.Equal(b[i:end], zeroBlock[:end-i]) {
			i = end
			continue
		}
		j := end
		for j < len(b) {
			next := min(j+sparseBlock, len(b))
			if bytes.Equal(b[j:next], zeroBlock[:next-j]) {
				break
			}
			j = next
		}
		if _, err := f.WriteAt(b[i:j], off+int64(i)); err != nil {
			return err
		}
		i = j
	}
	return nil
}
