package ranges

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"testing"
)

// Property tests for the bitmap (DESIGN.md §9, M0 item 3).
//
// ranges_test.go pins the cases we thought of. These state the invariants the
// rest of the system leans on and check them against generated op sequences,
// because the invariants are what M5 and M6 are actually built out of:
//
//   - A PRESENT set never exceeds the bytes it was told about (Mark rounds
//     inward). A byte marked present that was never written is a hole served as
//     content — silent corruption, the worst outcome available to M5.
//   - A DIRTY set always covers them (MarkCovering rounds outward). A written
//     byte outside every extent is an edit the uploader never ships, and the
//     remote keeps stale content forever.
//   - A union across mismatched block grids marks nothing and says so, which is
//     the caller's cue to fall back to a whole-file push.
//
// Everything is derived from a fixed seed range, so the suite is deterministic
// and a failure prints the seed and the exact spans that produced it.

// propCases is the number of seeds each property runs. Seeds are 0..propCases-1
// and the sequence for a given seed never changes, so a red run reproduces.
const propCases = 200

// propKey separates this file's streams from any other PCG use with the same
// low seeds.
const propKey = 0x0ff5e75e75e75e7

// span is one Mark/MarkCovering call, in the shape a FUSE write or a hydrate
// fault delivers it — degenerate cases included.
type span struct{ off, n int64 }

func (s span) String() string { return fmt.Sprintf("(%d,%d)", s.off, s.n) }

// bytesOf renders spans as the byte set they name against a file of size: the
// obvious O(bytes) implementation the bitmap is a compression of. It shares no
// code with Set on purpose — an oracle built out of clamp and blockSpan would
// agree with a broken Set for exactly the wrong reason.
func bytesOf(spans []span, size int64) []bool {
	in := make([]bool, size)
	for _, s := range spans {
		lo, hi := s.off, s.off+s.n
		if lo < 0 {
			lo = 0
		}
		if hi > size {
			hi = size
		}
		for b := lo; b < hi; b++ {
			in[b] = true
		}
	}
	return in
}

// blockExpand widens a byte set to whole blocks — the most a correctly rounding
// dirty set may ever mark, and therefore the upper bound MarkCovering is held to.
func blockExpand(in []bool, block int64) []bool {
	size := int64(len(in))
	out := make([]bool, size)
	for b := int64(0); b < size; b++ {
		if !in[b] {
			continue
		}
		lo := (b / block) * block
		hi := lo + block
		if hi > size {
			hi = size
		}
		for i := lo; i < hi; i++ {
			out[i] = true
		}
	}
	return out
}

// markedBytes renders the set's own account of itself as a byte set, checking on
// the way that Extents is well formed: ascending, non-empty, block-aligned,
// inside the file, and maximally coalesced. Two extents that touch would mean a
// run got split, which a ranged PUT would ship as two requests for no reason.
func markedBytes(t testing.TB, rs Set) []bool {
	t.Helper()
	out := make([]bool, rs.Size)
	prevEnd := int64(-1)
	for i, r := range rs.Extents() {
		end := r.Off + r.Len
		switch {
		case r.Len <= 0:
			t.Fatalf("extent %d of %+v is empty: %+v", i, rs, r)
		case r.Off < 0 || end > rs.Size:
			t.Fatalf("extent %d of %+v escapes the file: %+v", i, rs, r)
		case r.Off%rs.blockSize() != 0:
			t.Fatalf("extent %d of %+v does not start on a block boundary: %+v", i, rs, r)
		case end != rs.Size && end%rs.blockSize() != 0:
			t.Fatalf("extent %d of %+v ends mid-block short of EOF: %+v", i, rs, r)
		case r.Off <= prevEnd:
			t.Fatalf("extent %d of %+v is out of order or touches its predecessor: %+v", i, rs, r)
		}
		for b := r.Off; b < end; b++ {
			out[b] = true
		}
		prevEnd = end
	}
	return out
}

// blocksOf snapshots which blocks are marked, for the properties that are about
// the bitmap rather than about bytes.
func blocksOf(rs Set) []bool {
	out := make([]bool, rs.Blocks())
	for i := range out {
		out[i] = rs.hasBlock(int64(i))
	}
	return out
}

// randDims picks a file size and block size. Both are small so the byte-level
// oracle stays cheap, and block sizes are not powers of two by default — a
// rounding bug that only shows up on an unaligned grid is exactly the kind this
// file exists to find.
func randDims(r *rand.Rand) (size, block int64) {
	return r.Int64N(4096) + 1, r.Int64N(200) + 1
}

// randSpans generates the shapes production produces plus the ones it must
// survive: whole-file writes, single bytes, straddling spans, spans past EOF,
// empty spans and negative offsets.
func randSpans(r *rand.Rand, size int64, n int) []span {
	out := make([]span, n)
	for i := range out {
		switch r.IntN(8) {
		case 0:
			out[i] = span{0, size} // the whole file
		case 1:
			out[i] = span{r.Int64N(size), 1} // one byte
		case 2:
			out[i] = span{-r.Int64N(16) - 1, r.Int64N(size + 8)} // starts before 0
		case 3:
			out[i] = span{r.Int64N(size), size} // runs past EOF
		case 4:
			out[i] = span{r.Int64N(size), 0} // empty
		case 5:
			out[i] = span{r.Int64N(size), -r.Int64N(8) - 1} // negative length
		default:
			off := r.Int64N(size)
			out[i] = span{off, r.Int64N(size - off + 1)}
		}
	}
	return out
}

// eachCase runs f over the fixed seed range, handing it a fresh generator.
func eachCase(t *testing.T, f func(seed uint64, r *rand.Rand)) {
	t.Helper()
	for seed := uint64(0); seed < propCases; seed++ {
		f(seed, rand.New(rand.NewPCG(seed, propKey)))
	}
}

// --- the two rounding directions ---------------------------------------------

// A present set never claims a byte nobody wrote. This is the M5 direction: the
// consequence of getting it wrong is a read that serves a hole as content, and
// no later fetch ever repairs it because the set says there is nothing missing.
func TestPropMarkNeverExceedsWhatItWasGiven(t *testing.T) {
	eachCase(t, func(seed uint64, r *rand.Rand) {
		size, block := randDims(r)
		spans := randSpans(r, size, r.IntN(16)+1)

		rs := New(size, block)
		for _, s := range spans {
			rs.Mark(s.off, s.n)
		}

		named := bytesOf(spans, size)
		got := markedBytes(t, rs)
		for b := range got {
			if got[b] && !named[b] {
				t.Fatalf("seed %d (size=%d block=%d spans=%v): byte %d reports present, but no Mark named it",
					seed, size, block, spans, b)
			}
		}

		// ...and it is not vacuously safe: a set told about every byte is
		// Complete, whatever order the spans arrived in.
		all := true
		for _, ok := range named {
			all = all && ok
		}
		if all && !rs.Complete() {
			t.Fatalf("seed %d (size=%d block=%d spans=%v): every byte was marked, yet Complete() is false; missing %+v",
				seed, size, block, spans, rs.Missing(0, size))
		}
	})
}

// A dirty set always ships every byte it was told about. This is the M6
// direction: a written byte outside every extent is an edit that never reaches
// the remote, and the file stays wrong until something else rewrites it.
func TestPropMarkCoveringAlwaysCoversWhatItWasGiven(t *testing.T) {
	eachCase(t, func(seed uint64, r *rand.Rand) {
		size, block := randDims(r)
		spans := randSpans(r, size, r.IntN(16)+1)

		rs := New(size, block)
		for _, s := range spans {
			rs.MarkCovering(s.off, s.n)
			if !rs.Has(s.off, s.n) {
				t.Fatalf("seed %d (size=%d block=%d): Has%v is false immediately after MarkCovering%v",
					seed, size, block, s, s)
			}
		}

		named := bytesOf(spans, size)
		got := markedBytes(t, rs)
		bound := blockExpand(named, block)
		for b := range got {
			if named[b] && !got[b] {
				t.Fatalf("seed %d (size=%d block=%d spans=%v): byte %d was written and is in no extent",
					seed, size, block, spans, b)
			}
			if got[b] && !bound[b] {
				t.Fatalf("seed %d (size=%d block=%d spans=%v): byte %d is dirty but its block was never touched",
					seed, size, block, spans, b)
			}
		}
	})
}

// Marking only ever adds. Both readings depend on it: a present block that goes
// absent re-fetches (merely slow), but a dirty block that goes clean drops an
// edit, and the coalescer folds sets together assuming neither can happen.
func TestPropMarkingIsMonotoneAndIdempotent(t *testing.T) {
	eachCase(t, func(seed uint64, r *rand.Rand) {
		size, block := randDims(r)
		spans := randSpans(r, size, r.IntN(16)+1)

		rs := New(size, block)
		for i, s := range spans {
			before := blocksOf(rs)
			if i%2 == 0 {
				rs.Mark(s.off, s.n)
			} else {
				rs.MarkCovering(s.off, s.n)
			}
			after := blocksOf(rs)
			for b := range before {
				if before[b] && !after[b] {
					t.Fatalf("seed %d (size=%d block=%d): %v cleared block %d", seed, size, block, s, b)
				}
			}

			// The same call again must be a no-op; the engine replays spans
			// freely when it coalesces.
			if i%2 == 0 {
				rs.Mark(s.off, s.n)
			} else {
				rs.MarkCovering(s.off, s.n)
			}
			if again := blocksOf(rs); !reflect.DeepEqual(after, again) {
				t.Fatalf("seed %d (size=%d block=%d): repeating %v changed the set", seed, size, block, s)
			}
		}
	})
}

// --- what the readers ask ----------------------------------------------------

// Extents and Missing are exact complements over the file, and the summaries
// agree with them: Bytes counts the marked bytes, Complete means nothing is
// missing, Empty means nothing is marked. A hydrating read trusts Missing to
// name every absent byte; an upload trusts Extents to name every present one.
func TestPropExtentsAndMissingPartitionTheFile(t *testing.T) {
	eachCase(t, func(seed uint64, r *rand.Rand) {
		size, block := randDims(r)
		spans := randSpans(r, size, r.IntN(16)+1)

		rs := New(size, block)
		for i, s := range spans {
			if i%2 == 0 {
				rs.Mark(s.off, s.n)
			} else {
				rs.MarkCovering(s.off, s.n)
			}
		}

		got := markedBytes(t, rs)
		var missing int64
		for _, m := range rs.Missing(0, size) {
			if m.Len <= 0 || m.Off < 0 || m.Off+m.Len > size {
				t.Fatalf("seed %d: malformed missing range %+v for %+v", seed, m, rs)
			}
			for b := m.Off; b < m.Off+m.Len; b++ {
				if got[b] {
					t.Fatalf("seed %d (size=%d block=%d): byte %d is reported both marked and missing",
						seed, size, block, b)
				}
				missing++
			}
		}

		var marked int64
		for _, ok := range got {
			if ok {
				marked++
			}
		}
		if marked+missing != size {
			t.Fatalf("seed %d (size=%d block=%d): %d marked + %d missing != %d bytes",
				seed, size, block, marked, missing, size)
		}
		if rs.Bytes() != marked {
			t.Fatalf("seed %d (size=%d block=%d): Bytes() = %d; counted %d", seed, size, block, rs.Bytes(), marked)
		}
		if rs.Complete() != (missing == 0) {
			t.Fatalf("seed %d (size=%d block=%d): Complete() = %v with %d bytes missing",
				seed, size, block, rs.Complete(), missing)
		}
		if rs.Empty() != (marked == 0) {
			t.Fatalf("seed %d (size=%d block=%d): Empty() = %v with %d bytes marked",
				seed, size, block, rs.Empty(), marked)
		}
	})
}

// --- union ------------------------------------------------------------------

// Mismatched grids degrade to unknown: Union reports false and leaves the
// receiver untouched, bit for bit. Marking anything here would splice one file's
// blocks into another file's grid, and the caller would never learn it had to
// fall back to the whole file.
func TestPropUnionAcrossMismatchedGridsMarksNothing(t *testing.T) {
	eachCase(t, func(seed uint64, r *rand.Rand) {
		sizeA, blockA := randDims(r)
		sizeB, blockB := randDims(r)
		if blockA == blockB {
			blockB = blockA + 1 + r.Int64N(64) // mismatch is the point of this case
		}

		a := New(sizeA, blockA)
		for _, s := range randSpans(r, sizeA, r.IntN(8)+1) {
			a.MarkCovering(s.off, s.n)
		}
		b := New(sizeB, blockB)
		for _, s := range randSpans(r, sizeB, r.IntN(8)+1) {
			b.MarkCovering(s.off, s.n)
		}
		before := a.Clone()

		if a.Union(b) {
			t.Fatalf("seed %d: Union across block sizes %d and %d reported success", seed, blockA, blockB)
		}
		if a.Size != before.Size || a.BlockSize != before.BlockSize || !reflect.DeepEqual(a.Bits, before.Bits) {
			t.Fatalf("seed %d: a refused Union still modified the receiver: %+v -> %+v", seed, before, a)
		}
	})
}

// On a shared grid Union is exactly the union of the two block sets, and the
// result describes the longer of the two files. This is what lets the coalescer
// fold a burst of writes into one push without re-reading the file.
func TestPropUnionOfMatchingGridsIsTheUnionOfTheirBlocks(t *testing.T) {
	eachCase(t, func(seed uint64, r *rand.Rand) {
		block := r.Int64N(200) + 1
		sizeA, sizeB := r.Int64N(4096)+1, r.Int64N(4096)+1

		a := New(sizeA, block)
		for _, s := range randSpans(r, sizeA, r.IntN(8)+1) {
			a.MarkCovering(s.off, s.n)
		}
		b := New(sizeB, block)
		for _, s := range randSpans(r, sizeB, r.IntN(8)+1) {
			b.MarkCovering(s.off, s.n)
		}
		wantA, wantB := blocksOf(a), blocksOf(b)

		if !a.Union(b) {
			t.Fatalf("seed %d: Union declined a shared grid (block=%d)", seed, block)
		}
		if want := max(sizeA, sizeB); a.Size != want {
			t.Fatalf("seed %d: Size after Union = %d; want %d", seed, a.Size, want)
		}
		for i := int64(0); i < a.Blocks(); i++ {
			from := func(src []bool) bool { return i < int64(len(src)) && src[i] }
			if want := from(wantA) || from(wantB); a.hasBlock(i) != want {
				t.Fatalf("seed %d (block=%d sizes=%d,%d): block %d = %v after Union; want %v",
					seed, block, sizeA, sizeB, i, a.hasBlock(i), want)
			}
		}
	})
}

// The zero Set normalises to the default grid, so folding one in is compatible
// rather than a mismatch — the shape a state DB record from an older build, or a
// handle that never saw a write, arrives in.
func TestPropZeroSetUnionsWithTheDefaultGrid(t *testing.T) {
	rs := New(10*DefaultBlockSize, 0) // 0 selects DefaultBlockSize
	rs.MarkCovering(0, 1)
	before := blocksOf(rs)

	if !rs.Union(Set{}) {
		t.Fatal("Union with the zero Set should be compatible with the default grid")
	}
	if got := blocksOf(rs); !reflect.DeepEqual(got, before) {
		t.Errorf("Union with the zero Set changed the marks: %v -> %v", before, got)
	}

	var zero Set
	if !zero.Union(rs) {
		t.Fatal("Union into the zero Set should be compatible with the default grid")
	}
	if !zero.hasBlock(0) || zero.Size != rs.Size {
		t.Errorf("Union into the zero Set lost the marks: %+v", zero)
	}
}

// --- carrying a set around ---------------------------------------------------

// Grow keeps every mark and refuses to shrink. A set that quietly reinterpreted
// its bits against a smaller file would describe extents the file no longer has;
// the caller is required to start fresh instead, and this is what makes that
// requirement enforceable.
func TestPropGrowPreservesEveryMark(t *testing.T) {
	eachCase(t, func(seed uint64, r *rand.Rand) {
		size, block := randDims(r)
		rs := New(size, block)
		for _, s := range randSpans(r, size, r.IntN(8)+1) {
			rs.MarkCovering(s.off, s.n)
		}
		before := blocksOf(rs)

		bigger := size + r.Int64N(4096)
		rs.Grow(bigger)
		if rs.Size != bigger {
			t.Fatalf("seed %d: Grow(%d) left Size = %d", seed, bigger, rs.Size)
		}
		for i, was := range before {
			if was && !rs.hasBlock(int64(i)) {
				t.Fatalf("seed %d (size=%d block=%d): Grow(%d) lost block %d", seed, size, block, bigger, i)
			}
		}

		frozen := rs.Clone()
		rs.Grow(r.Int64N(bigger + 1)) // any size at or below the current one
		if rs.Size != frozen.Size || !reflect.DeepEqual(rs.Bits, frozen.Bits) {
			t.Fatalf("seed %d: Grow to a smaller size changed the set: %+v -> %+v", seed, frozen, rs)
		}
	})
}

// A set survives the state store byte for byte. M5 reads these back after a
// restart to decide what is resident; a lossy encoding would make a placeholder
// look hydrated.
func TestPropMarshalRoundTripPreservesTheSet(t *testing.T) {
	eachCase(t, func(seed uint64, r *rand.Rand) {
		size, block := randDims(r)
		rs := New(size, block)
		for _, s := range randSpans(r, size, r.IntN(8)+1) {
			rs.Mark(s.off, s.n)
		}

		b, err := rs.MarshalBinary()
		if err != nil {
			t.Fatalf("seed %d: MarshalBinary: %v", seed, err)
		}
		var got Set
		if err := got.UnmarshalBinary(b); err != nil {
			t.Fatalf("seed %d: UnmarshalBinary: %v", seed, err)
		}
		if got.Size != rs.Size || got.BlockSize != rs.BlockSize {
			t.Fatalf("seed %d: dimensions changed: %+v -> %+v", seed, rs, got)
		}
		if !reflect.DeepEqual(got.Extents(), rs.Extents()) {
			t.Fatalf("seed %d: extents changed: %+v -> %+v", seed, rs.Extents(), got.Extents())
		}
	})
}

// A clone shares nothing. The dirty tracker hands one to the uploader while the
// writes that produced it are still arriving on another thread, so an aliased
// Bits slice would let an in-flight push grow under the executor.
func TestPropCloneIsIndependent(t *testing.T) {
	eachCase(t, func(seed uint64, r *rand.Rand) {
		size, block := randDims(r)
		rs := New(size, block)
		for _, s := range randSpans(r, size, r.IntN(8)+1) {
			rs.MarkCovering(s.off, s.n)
		}

		c := rs.Clone()
		frozen := blocksOf(c)
		rs.MarkCovering(0, size) // the writer keeps going
		if got := blocksOf(c); !reflect.DeepEqual(got, frozen) {
			t.Fatalf("seed %d: marking the original changed the clone", seed)
		}

		c2 := rs.Clone()
		frozen2 := blocksOf(rs)
		c2.Grow(size + block + 1)
		c2.MarkCovering(0, c2.Size)
		if got := blocksOf(rs); !reflect.DeepEqual(got, frozen2) {
			t.Fatalf("seed %d: marking the clone changed the original", seed)
		}
	})
}

// FuzzSetInvariants is the same three properties with the case chosen by the
// fuzzer instead of by a seed loop. `go test -race ./...` runs the corpus below;
// `go test -run=- -fuzz=FuzzSetInvariants ./internal/ranges` goes looking.
func FuzzSetInvariants(f *testing.F) {
	f.Add(uint64(1), int64(4096), int64(64), 16)
	f.Add(uint64(7), int64(1), int64(1), 4)
	f.Add(uint64(99), int64(4095), int64(199), 32)
	f.Add(uint64(1234), int64(37), int64(4096), 8)
	f.Add(uint64(7), int64(-59), int64(1), 4) // negative dimensions: the harness normalises, it does not panic

	f.Fuzz(func(t *testing.T, seed uint64, size, block int64, ops int) {
		// Normalise rather than skip, and take the modulus before the sign: the
		// fuzzer reaches negative dimensions long before it reaches interesting
		// ones, and a skip on each would spend the whole budget getting past
		// them.
		norm := func(v, hi int64) int64 {
			if v %= hi; v < 0 {
				v = -v // safe: |v| < hi after the modulus
			}
			return v + 1
		}
		size, block, ops = norm(size, 4096), norm(block, 4096), int(norm(int64(ops), 32))
		r := rand.New(rand.NewPCG(seed, propKey))
		spans := randSpans(r, size, ops)
		named := bytesOf(spans, size)

		present := New(size, block)
		dirty := New(size, block)
		for _, s := range spans {
			present.Mark(s.off, s.n)
			dirty.MarkCovering(s.off, s.n)
		}

		gotPresent := markedBytes(t, present)
		gotDirty := markedBytes(t, dirty)
		bound := blockExpand(named, block)
		for b := range named {
			if gotPresent[b] && !named[b] {
				t.Fatalf("present set claims unwritten byte %d (size=%d block=%d spans=%v)", b, size, block, spans)
			}
			if named[b] && !gotDirty[b] {
				t.Fatalf("dirty set drops written byte %d (size=%d block=%d spans=%v)", b, size, block, spans)
			}
			if gotDirty[b] && !bound[b] {
				t.Fatalf("dirty set marks untouched block at byte %d (size=%d block=%d spans=%v)", b, size, block, spans)
			}
			// Inward rounding can never exceed outward rounding on the same
			// spans; if it ever did, the two readings would disagree about the
			// same write.
			if gotPresent[b] && !gotDirty[b] {
				t.Fatalf("Mark marked byte %d that MarkCovering did not (size=%d block=%d spans=%v)", b, size, block, spans)
			}
		}
	})
}
