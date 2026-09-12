package ranges

import (
	"reflect"
	"testing"
)

// A fresh set for a non-empty file is Empty, not Complete, and reports the whole
// file as missing.
func TestNewRangeSetIsEmpty(t *testing.T) {
	rs := New(10<<20, 4<<20) // 10 MiB over 4 MiB blocks => 3 blocks
	if rs.Blocks() != 3 {
		t.Fatalf("Blocks() = %d; want 3", rs.Blocks())
	}
	if !rs.Empty() {
		t.Error("a fresh set should be Empty")
	}
	if rs.Complete() {
		t.Error("a fresh set should not be Complete")
	}
	got := rs.Missing(0, 10<<20)
	want := []Range{{Off: 0, Len: 10 << 20}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Missing = %+v; want %+v", got, want)
	}
}

// A zero-length file has nothing to fetch, so it is trivially Complete. Empty is
// false because there is no absent block to describe.
func TestZeroLengthIsComplete(t *testing.T) {
	rs := New(0, 4<<20)
	if !rs.Complete() {
		t.Error("zero-length file should be Complete")
	}
	if rs.Empty() {
		t.Error("zero-length file should not report Empty")
	}
	if got := rs.Missing(0, 100); got != nil {
		t.Errorf("Missing = %+v; want nil", got)
	}
}

// MarkAll makes the set Complete and leaves nothing missing — the only transition
// M5 performs.
func TestMarkAll(t *testing.T) {
	rs := New(9, 4)
	rs.MarkAll()
	if !rs.Complete() {
		t.Fatal("MarkAll should make the set Complete")
	}
	if rs.Empty() {
		t.Error("MarkAll should clear Empty")
	}
	if got := rs.Missing(0, 9); got != nil {
		t.Errorf("Missing after MarkAll = %+v; want nil", got)
	}
}

// Only fully-covered blocks are marked: a partial write must not be mistaken for
// cached content, or a later read would serve a hole as data.
func TestMarkOnlyWholeBlocks(t *testing.T) {
	rs := New(40, 10) // 4 blocks of 10
	rs.Mark(5, 20)    // covers [5,25): block 1 whole, blocks 0 and 2 partial
	if !rs.Has(10, 10) {
		t.Error("block 1 is fully covered and should be present")
	}
	if rs.Has(0, 10) {
		t.Error("block 0 is only partially covered and must not be marked")
	}
	if rs.Has(20, 10) {
		t.Error("block 2 is only partially covered and must not be marked")
	}
}

// The trailing partial block counts as whole when the range reaches EOF — there
// are no further bytes that could ever be fetched for it.
func TestMarkTrailingPartialBlockAtEOF(t *testing.T) {
	rs := New(25, 10) // blocks: [0,10) [10,20) [20,25)
	rs.Mark(20, 5)
	if !rs.Has(20, 5) {
		t.Error("the final short block should be present once written to EOF")
	}
	rs.Mark(0, 20)
	if !rs.Complete() {
		t.Error("marking every block should make the set Complete")
	}
}

// Missing coalesces runs of absent blocks and clamps the last range to the file
// size, so a caller can issue one ranged GET per gap.
func TestMissingCoalescesAndClamps(t *testing.T) {
	rs := New(45, 10) // 5 blocks, last one short (40..45)
	rs.Mark(10, 10)   // block 1 present
	got := rs.Missing(0, 45)
	want := []Range{{Off: 0, Len: 10}, {Off: 20, Len: 25}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Missing = %+v; want %+v", got, want)
	}
}

// A read past EOF is satisfied by what precedes EOF rather than reporting an
// unfetchable range.
func TestReadPastEOFClampsToSize(t *testing.T) {
	rs := New(15, 10)
	rs.MarkAll()
	if !rs.Has(10, 1000) {
		t.Error("a read running past EOF should be satisfied by the resident tail")
	}
	if got := rs.Missing(0, 1000); got != nil {
		t.Errorf("Missing past EOF = %+v; want nil", got)
	}
}

// Negative and zero-length reads are inert rather than panicking or marking.
func TestDegenerateSpans(t *testing.T) {
	rs := New(20, 10)
	if !rs.Has(0, 0) {
		t.Error("a zero-length read is trivially satisfied")
	}
	rs.Mark(0, 0)
	if !rs.Empty() {
		t.Error("a zero-length Mark must not mark anything")
	}
	rs.Mark(-100, 50) // clamps to [0,?) — only whole blocks below 50-100 offset
	if got := rs.Missing(-5, 5); got != nil {
		t.Errorf("Missing of a fully-negative span = %+v; want nil", got)
	}
}

// A set survives the state-store round trip, bits intact.
func TestMarshalRoundTrip(t *testing.T) {
	rs := New(100, 10)
	rs.Mark(30, 20)

	b, err := rs.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	var got Set
	if err := got.UnmarshalBinary(b); err != nil {
		t.Fatal(err)
	}
	if got.Size != rs.Size || got.BlockSize != rs.BlockSize {
		t.Errorf("geometry lost: got %+v want %+v", got, rs)
	}
	if !got.Has(30, 20) {
		t.Error("marked blocks did not survive the round trip")
	}
	if got.Has(0, 10) {
		t.Error("unmarked blocks came back marked")
	}
}

// A zero-value Set must not panic or divide by zero.
func TestZeroValueSafe(t *testing.T) {
	var rs Set
	if rs.Blocks() != 0 {
		t.Errorf("Blocks() = %d; want 0", rs.Blocks())
	}
	if !rs.Complete() {
		t.Error("the zero value describes an empty file and is Complete")
	}
	rs.Mark(0, 100) // must not panic
	if got := rs.Missing(0, 100); got != nil {
		t.Errorf("Missing = %+v; want nil", got)
	}
}

// --- dirty side (M6) --------------------------------------------------------

// MarkCovering rounds OUTWARD: a one-byte write dirties the whole block holding
// it. Mark, the present-side counterpart, would mark nothing for the same span.
func TestMarkCoveringRoundsOutward(t *testing.T) {
	const bs = 4 << 20
	dirty := New(10<<20, bs)
	dirty.MarkCovering(bs+1, 1) // one byte inside block 1

	present := New(10<<20, bs)
	present.Mark(bs+1, 1)

	if got := dirty.Extents(); !reflect.DeepEqual(got, []Range{{Off: bs, Len: bs}}) {
		t.Fatalf("dirty Extents() = %v; want the whole block 1", got)
	}
	if got := present.Extents(); got != nil {
		t.Fatalf("present Extents() = %v; want nothing (inward rounding)", got)
	}
}

// A write spanning a block boundary dirties both blocks, and Extents coalesces
// them into one contiguous range.
func TestMarkCoveringSpansBoundary(t *testing.T) {
	const bs = 4 << 20
	rs := New(10<<20, bs)
	rs.MarkCovering(bs-1, 2) // last byte of block 0, first of block 1

	want := []Range{{Off: 0, Len: 2 * bs}}
	if got := rs.Extents(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Extents() = %v; want %v", got, want)
	}
	if got := rs.Bytes(); got != 2*bs {
		t.Fatalf("Bytes() = %d; want %d", got, 2*bs)
	}
}

// Extents clamps the trailing partial block to Size, so a patch never claims to
// carry bytes past EOF.
func TestExtentsClampsTrailingBlockToSize(t *testing.T) {
	const bs = 4 << 20
	rs := New(10<<20, bs) // block 2 is only 2 MiB long
	rs.MarkCovering(9<<20, 1)

	want := []Range{{Off: 2 * bs, Len: 10<<20 - 2*bs}}
	if got := rs.Extents(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Extents() = %v; want %v", got, want)
	}
}

// Extents and Missing are complements over a fully described file.
func TestExtentsAndMissingAreComplements(t *testing.T) {
	const bs = 4 << 20
	rs := New(20<<20, bs) // 5 blocks
	rs.MarkCovering(0, 1)
	rs.MarkCovering(3*bs, 1)

	if got, want := rs.Extents(), []Range{{Off: 0, Len: bs}, {Off: 3 * bs, Len: bs}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Extents() = %v; want %v", got, want)
	}
	if got, want := rs.Missing(0, 20<<20), []Range{{Off: bs, Len: 2 * bs}, {Off: 4 * bs, Len: bs}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Missing() = %v; want %v", got, want)
	}
}

// Grow extends the set for a file that got bigger without disturbing existing
// marks, and refuses to shrink (stale bits must never be reinterpreted).
func TestGrowPreservesMarksAndRefusesToShrink(t *testing.T) {
	const bs = 4 << 20
	rs := New(bs, bs)
	rs.MarkCovering(0, 1)

	rs.Grow(12 << 20) // 1 block -> 3 blocks
	if rs.Blocks() != 3 {
		t.Fatalf("Blocks() after Grow = %d; want 3", rs.Blocks())
	}
	if !rs.Has(0, bs) {
		t.Error("Grow lost the existing mark on block 0")
	}
	if rs.Complete() {
		t.Error("the newly added blocks should be unmarked")
	}

	rs.Grow(bs) // shrink attempt: ignored
	if rs.Size != 12<<20 {
		t.Fatalf("Grow shrank the set to %d; want it left at %d", rs.Size, 12<<20)
	}
}

// Union folds one set into another, growing to the larger size.
func TestUnion(t *testing.T) {
	const bs = 4 << 20
	a := New(8<<20, bs)
	a.MarkCovering(0, 1)

	b := New(16<<20, bs)
	b.MarkCovering(3*bs, 1)

	if !a.Union(b) {
		t.Fatal("Union of matching block grids should succeed")
	}
	want := []Range{{Off: 0, Len: bs}, {Off: 3 * bs, Len: bs}}
	if got := a.Extents(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Extents() after Union = %v; want %v", got, want)
	}
}

// Mismatched block grids cannot be unioned bit-for-bit; Union must refuse rather
// than silently mismark, so the caller can fall back to whole-file handling.
func TestUnionRefusesMismatchedBlockSize(t *testing.T) {
	a := New(8<<20, 4<<20)
	b := New(8<<20, 1<<20)
	b.MarkCovering(0, 1)

	if a.Union(b) {
		t.Fatal("Union across different block sizes should report false")
	}
	if !a.Empty() {
		t.Fatal("a refused Union must not have marked anything")
	}
}

// Clone must deep-copy the bitmap, or a snapshot handed to the uploader would be
// mutated by writes still arriving on the handle.
func TestCloneIsDeep(t *testing.T) {
	const bs = 4 << 20
	rs := New(8<<20, bs)
	rs.MarkCovering(0, 1)

	snap := rs.Clone()
	rs.MarkCovering(bs, 1)

	if len(snap.Extents()) != 1 {
		t.Fatalf("clone was mutated by a later mark: %v", snap.Extents())
	}
}

// Bytes is the amount a patch would actually ship, and is 0 for an untouched set.
func TestBytes(t *testing.T) {
	const bs = 4 << 20
	rs := New(20<<20, bs)
	if got := rs.Bytes(); got != 0 {
		t.Fatalf("Bytes() on a fresh set = %d; want 0", got)
	}
	rs.MarkAll()
	if got := rs.Bytes(); got != 20<<20 {
		t.Fatalf("Bytes() after MarkAll = %d; want %d", got, 20<<20)
	}
}
