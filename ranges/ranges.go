// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

// Package ranges is the byte-extent bookkeeping shared by lazy hydration (M5)
// and range writes (M6). It is a leaf: pure value types, no I/O, no provider.
//
// One structure, read two ways (DESIGN.md §9, M6):
//
//   - As PRESENT ranges (internal/hydrate) it answers "which bytes of this file
//     are actually resident in the backing store?" — so a read knows what it must
//     fault in. Here a block counts only when it is *fully* written, so a partial
//     fault or a crash mid-write can never be mistaken for cached content: Mark
//     rounds the span INWARD.
//   - As DIRTY ranges (internal/syncengine) it answers "which bytes has the user
//     changed since we last pushed?" — so an upload can ship only those extents.
//     Here a block counts as soon as any byte of it is touched, or the upload
//     would omit a partially-written block: MarkCovering rounds the span OUTWARD.
//
// The rounding direction is the whole difference, and it is not symmetric by
// accident. Both errors must fall on the side of doing more work rather than
// losing data — re-fetching a block we already had, or re-sending a block that
// had not changed.
package ranges

import "encoding/json"

// DefaultBlockSize is the granularity of the bitmap. 4 MiB keeps it small (a
// 4 GiB file needs 128 bytes) while staying large enough that a faulted block
// amortises a request round-trip. On the dirty side it is also the write
// amplification floor: a one-byte edit ships one block.
const DefaultBlockSize int64 = 4 << 20

// Range is a half-open byte interval [Off, Off+Len).
type Range struct {
	Off int64
	Len int64
}

// Set records which blocks of a file are marked — resident, or dirty, depending
// on the caller (see the package doc).
//
// The zero Set describes a zero-length file and reports Complete.
type Set struct {
	BlockSize int64  `json:"block"`
	Size      int64  `json:"size"`
	Bits      []byte `json:"bits,omitempty"` // little-endian bit i => block i marked
}

// New returns an empty (nothing marked) set for a file of size bytes. A
// non-positive blockSize selects DefaultBlockSize.
func New(size, blockSize int64) Set {
	if blockSize <= 0 {
		blockSize = DefaultBlockSize
	}
	if size < 0 {
		size = 0
	}
	rs := Set{BlockSize: blockSize, Size: size}
	rs.Bits = make([]byte, (rs.Blocks()+7)/8)
	return rs
}

// Blocks is the number of blocks covering the file.
func (rs Set) Blocks() int64 {
	if rs.Size <= 0 || rs.blockSize() <= 0 {
		return 0
	}
	return (rs.Size + rs.blockSize() - 1) / rs.blockSize()
}

func (rs Set) blockSize() int64 {
	if rs.BlockSize <= 0 {
		return DefaultBlockSize
	}
	return rs.BlockSize
}

// hasBlock reports whether block i is marked.
func (rs Set) hasBlock(i int64) bool {
	idx := i / 8
	if i < 0 || idx >= int64(len(rs.Bits)) {
		return false
	}
	return rs.Bits[idx]&(1<<uint(i%8)) != 0
}

func (rs *Set) setBlock(i int64) {
	idx := i / 8
	if i < 0 || idx >= int64(len(rs.Bits)) {
		return
	}
	rs.Bits[idx] |= 1 << uint(i%8)
}

// Grow extends the set to describe a file of size bytes, preserving every mark.
// Shrinking is not supported — a file that got smaller has a different shape than
// the one these marks describe, and the caller must start a fresh Set rather than
// reinterpret stale bits against a new size.
func (rs *Set) Grow(size int64) {
	if size <= rs.Size {
		return
	}
	rs.Size = size
	if need := (rs.Blocks() + 7) / 8; int64(len(rs.Bits)) < need {
		grown := make([]byte, need)
		copy(grown, rs.Bits)
		rs.Bits = grown
	}
}

// blockSpan returns the half-open block index range covering bytes [off, off+n),
// clamped to the file.
func (rs Set) blockSpan(off, n int64) (first, last int64) {
	if off < 0 {
		n += off
		off = 0
	}
	if n <= 0 || rs.Size <= 0 {
		return 0, 0
	}
	end := off + n
	if end > rs.Size {
		end = rs.Size
	}
	if off >= end {
		return 0, 0
	}
	return off / rs.blockSize(), (end + rs.blockSize() - 1) / rs.blockSize()
}

// Has reports whether every byte of [off, off+n) is marked. A read past EOF is
// satisfied by whatever precedes EOF, so the span is clamped to Size first.
func (rs Set) Has(off, n int64) bool {
	first, last := rs.blockSpan(off, n)
	for i := first; i < last; i++ {
		if !rs.hasBlock(i) {
			return false
		}
	}
	return true
}

// Missing returns the byte ranges that must be fetched to satisfy a read of
// [off, off+n), coalescing adjacent absent blocks into as few ranges as possible.
// Returned ranges are block-aligned and clamped to Size — exactly what a ranged
// GET should ask for.
func (rs Set) Missing(off, n int64) []Range {
	first, last := rs.blockSpan(off, n)
	return rs.runs(first, last, false)
}

// Extents returns the marked byte ranges, coalesced — the dirty-side complement
// of Missing, and exactly what a ranged PUT should send.
func (rs Set) Extents() []Range {
	return rs.runs(0, rs.Blocks(), true)
}

// runs walks blocks [first, last) and coalesces maximal runs whose marked state
// equals want into byte ranges clamped to Size.
func (rs Set) runs(first, last int64, want bool) []Range {
	var out []Range
	for i := first; i < last; {
		if rs.hasBlock(i) != want {
			i++
			continue
		}
		start := i
		for i < last && rs.hasBlock(i) == want {
			i++
		}
		begin := start * rs.blockSize()
		end := i * rs.blockSize()
		if end > rs.Size {
			end = rs.Size
		}
		out = append(out, Range{Off: begin, Len: end - begin})
	}
	return out
}

// Bytes is the total number of marked bytes, clamped to Size. It is what the
// uploader compares against the file size to decide whether patching is actually
// cheaper than replacing.
func (rs Set) Bytes() int64 {
	var total int64
	for _, r := range rs.Extents() {
		total += r.Len
	}
	return total
}

// Mark records [off, off+n) as PRESENT. Only blocks *entirely* covered by the
// range are marked; a trailing partial block counts as covered when the range
// reaches EOF, since there are no further bytes to fetch. See the package doc for
// why this rounds inward and MarkCovering rounds outward.
func (rs *Set) Mark(off, n int64) {
	off, end, ok := rs.clamp(off, n)
	if !ok {
		return
	}
	bs := rs.blockSize()
	// Round the start up and the end down to block boundaries: only whole blocks
	// are marked. EOF is a legitimate block end even when it is not aligned.
	first := (off + bs - 1) / bs
	last := end / bs
	if end == rs.Size {
		last = rs.Blocks()
	}
	for i := first; i < last; i++ {
		rs.setBlock(i)
	}
}

// MarkCovering records every block that [off, off+n) TOUCHES as dirty, rounding
// outward. A write of one byte dirties the whole block containing it — which is
// the point: the block is the unit an upload can ship, and shipping the untouched
// remainder of a block is free correctness (we hold those bytes locally and they
// are the same ones the remote already has).
func (rs *Set) MarkCovering(off, n int64) {
	off, end, ok := rs.clamp(off, n)
	if !ok {
		return
	}
	bs := rs.blockSize()
	for i := off / bs; i < (end+bs-1)/bs; i++ {
		rs.setBlock(i)
	}
}

// clamp normalises a span against the file, reporting ok=false when nothing of it
// falls inside.
func (rs Set) clamp(off, n int64) (start, end int64, ok bool) {
	if off < 0 {
		n += off
		off = 0
	}
	if n <= 0 || rs.Size <= 0 {
		return 0, 0, false
	}
	end = off + n
	if end > rs.Size {
		end = rs.Size
	}
	if off >= end {
		return 0, 0, false
	}
	return off, end, true
}

// MarkAll records the whole file.
func (rs *Set) MarkAll() { rs.Mark(0, rs.Size) }

// Union folds other's marks into rs. It reports false — marking nothing — when
// the two sets do not describe the same block grid, because reinterpreting bits
// across block sizes would silently mismark. A false return is the caller's cue
// to fall back to whole-file handling.
func (rs *Set) Union(other Set) bool {
	if rs.blockSize() != other.blockSize() {
		return false
	}
	rs.Grow(other.Size)
	for i, n := int64(0), other.Blocks(); i < n; i++ {
		if other.hasBlock(i) {
			rs.setBlock(i)
		}
	}
	return true
}

// Complete reports whether the entire file is marked. A zero-length file is
// trivially complete.
func (rs Set) Complete() bool {
	n := rs.Blocks()
	for i := int64(0); i < n; i++ {
		if !rs.hasBlock(i) {
			return false
		}
	}
	return true
}

// Empty reports whether no block is marked (and the file is non-empty) — the
// state a freshly created placeholder is in.
func (rs Set) Empty() bool {
	n := rs.Blocks()
	if n == 0 {
		return false
	}
	for i := int64(0); i < n; i++ {
		if rs.hasBlock(i) {
			return false
		}
	}
	return true
}

// Clone returns a deep copy, so a snapshot handed to another goroutine cannot be
// mutated behind its back.
func (rs Set) Clone() Set {
	c := rs
	c.Bits = append([]byte(nil), rs.Bits...)
	return c
}

// MarshalBinary encodes the set for the state store.
func (rs Set) MarshalBinary() ([]byte, error) { return json.Marshal(rs) }

// UnmarshalBinary decodes a set encoded by MarshalBinary.
func (rs *Set) UnmarshalBinary(b []byte) error { return json.Unmarshal(b, rs) }
