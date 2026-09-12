package syncengine

import (
	"math/rand/v2"
	"testing"

	"github.com/zishmusic/drivel/internal/fsevent"
	"github.com/zishmusic/drivel/ranges"
)

// Property tests for the coalescer's merge rule (DESIGN.md §9, M0 item 3).
//
// The example tests above pin the shapes we thought to write down. This one
// generates event sequences — nil sets, compatible sets, sets built on another
// block grid, several paths interleaved — and holds the coalesced push to the
// rule the uploader depends on:
//
//	the push either says "extents unknown" (nil: send the whole file), or it
//	covers every block every contributing event named, and nothing else.
//
// The direction that matters is asymmetric. A push that forgets an extent writes
// a file that never existed; a push that sends too much costs bandwidth. So
// unknown must absorb known, and a mismatched grid must degrade to unknown
// rather than reinterpret one file's bits against another file's blocks.

const (
	coalescerCases = 200
	coalescerKey   = 0xc0a1e5cec0a1e5c
)

// gridOf is the block size a set will actually be read with, normalisation
// included — the zero value means DefaultBlockSize, not "no grid".
func gridOf(s ranges.Set) int64 {
	if s.BlockSize <= 0 {
		return ranges.DefaultBlockSize
	}
	return s.BlockSize
}

// markedBlocks reports which blocks a set holds, asking only through its public
// surface so the oracle cannot inherit a bug from the bitmap's internals.
func markedBlocks(s ranges.Set) map[int64]bool {
	out := map[int64]bool{}
	bs := gridOf(s)
	for i := int64(0); i*bs < s.Size; i++ {
		if s.Has(i*bs, 1) {
			out[i] = true
		}
	}
	return out
}

// acc is the accumulator the coalescer implements, written out longhand: the
// merge rule stated once more, in a form that cannot share a mistake with it.
type acc struct {
	pending bool
	unknown bool
	grid    int64
	size    int64
	blocks  map[int64]bool
}

func (a *acc) mark(s *ranges.Set) {
	switch {
	case !a.pending:
		a.pending = true
		if s == nil {
			a.unknown = true
			return
		}
		a.grid, a.size, a.blocks = gridOf(*s), s.Size, markedBlocks(*s)
	case a.unknown || s == nil:
		a.unknown = true // unknown absorbs known, and never lets go
	case gridOf(*s) != a.grid:
		a.unknown = true // incompatible grids: nobody can name the changed bytes
	default:
		if s.Size > a.size {
			a.size = s.Size
		}
		for i := range markedBlocks(*s) {
			a.blocks[i] = true
		}
	}
}

func (a *acc) reset() { *a = acc{blocks: map[int64]bool{}} }

// check holds one dispatched task to the accumulator's verdict.
func (a *acc) check(t *testing.T, seed uint64, path string, ev fsevent.Event) {
	t.Helper()
	if ev.Op != fsevent.OpWrite || ev.Path != path {
		t.Fatalf("seed %d: dispatched %s %q; want write %q", seed, ev.Op, ev.Path, path)
	}
	if a.unknown {
		if ev.Dirty != nil {
			t.Fatalf("seed %d: %s coalesced to extents %v; want unknown, i.e. the whole file",
				seed, path, ev.Dirty.Extents())
		}
		return
	}
	if ev.Dirty == nil {
		t.Fatalf("seed %d: %s coalesced to unknown; every contributing event named its extents", seed, path)
	}
	if got := gridOf(*ev.Dirty); got != a.grid {
		t.Fatalf("seed %d: %s coalesced onto block grid %d; want %d", seed, path, got, a.grid)
	}
	if ev.Dirty.Size != a.size {
		t.Fatalf("seed %d: %s coalesced at size %d; want %d", seed, path, ev.Dirty.Size, a.size)
	}
	got := markedBlocks(*ev.Dirty)
	for i := range a.blocks {
		if !got[i] {
			t.Fatalf("seed %d: %s drops block %d, which an event marked dirty", seed, path, i)
		}
	}
	for i := range got {
		if !a.blocks[i] {
			t.Fatalf("seed %d: %s ships block %d, which no event marked", seed, path, i)
		}
	}
}

// randEvent returns the dirty set for one write: unknown, a set on the shared
// grid, or — occasionally — a set built on a different grid, which is what a
// handle carried across a block-size change looks like.
func randEvent(r *rand.Rand, grid int64) *ranges.Set {
	switch {
	case r.IntN(4) == 0:
		return nil
	default:
		bs := grid
		if r.IntN(6) == 0 {
			bs = grid + 8 // a grid nothing can be merged onto
		}
		size := r.Int64N(12*grid) + 1
		s := ranges.New(size, bs)
		for n := r.IntN(4) + 1; n > 0; n-- {
			off := r.Int64N(size)
			s.MarkCovering(off, r.Int64N(size-off)+1)
		}
		return &s
	}
}

// The coalesced push is either unknown or exactly the union of what it was told,
// across interleaved paths, and it never keeps a reference to a caller's set.
func TestPropCoalescedPushIsUnknownOrCoversEveryEvent(t *testing.T) {
	paths := []string{"a.bin", "dir/b.bin", "c.bin"}

	for seed := uint64(0); seed < coalescerCases; seed++ {
		r := rand.New(rand.NewPCG(seed, coalescerKey))
		grid := int64(64) << uint(r.IntN(4))

		c := coalescer{pending: map[string]*ranges.Set{}}
		want := map[string]*acc{}
		for _, p := range paths {
			want[p] = &acc{blocks: map[int64]bool{}}
		}

		for i, n := 0, r.IntN(24)+1; i < n; i++ {
			p := paths[r.IntN(len(paths))]

			if r.IntN(6) == 0 {
				// A structural op must dispatch the path's pending content
				// first — "write then delete" has to stay in that order.
				got := c.structural(fsevent.Event{Op: fsevent.OpUnlink, Path: p})
				if want[p].pending {
					if len(got) != 2 {
						t.Fatalf("seed %d: structural(unlink %s) = %d tasks; want the pending write and the unlink",
							seed, p, len(got))
					}
					want[p].check(t, seed, p, got[0])
				} else if len(got) != 1 {
					t.Fatalf("seed %d: structural(unlink %s) = %d tasks; want just the unlink", seed, p, len(got))
				}
				if last := got[len(got)-1]; last.Op != fsevent.OpUnlink || last.Path != p {
					t.Fatalf("seed %d: structural(unlink %s) ended with %s %q", seed, p, last.Op, last.Path)
				}
				want[p].reset()
				continue
			}

			s := randEvent(r, grid)
			want[p].mark(s)
			c.markContent(p, s)
			if s != nil {
				// The handle that produced those extents keeps writing. The
				// coalescer must have taken a copy: an accumulator that aliased
				// the caller's bitmap would grow under the uploader after the
				// task was already queued.
				s.MarkCovering(0, s.Size)
			}
		}

		// Shutdown drains whatever is left, one task per pending path.
		drained := map[string]bool{}
		for _, ev := range c.flushAll() {
			a, ok := want[ev.Path]
			if !ok || !a.pending {
				t.Fatalf("seed %d: flushAll dispatched %s %q, which had nothing pending", seed, ev.Op, ev.Path)
			}
			a.check(t, seed, ev.Path, ev)
			drained[ev.Path] = true
		}
		for _, p := range paths {
			if want[p].pending && !drained[p] {
				t.Fatalf("seed %d: flushAll never dispatched %s, which had content pending", seed, p)
			}
		}
		if len(c.pending) != 0 {
			t.Fatalf("seed %d: %d paths still pending after flushAll", seed, len(c.pending))
		}
	}
}

// Whatever the sequence, flushing a path twice never dispatches the same content
// twice — the second call is what the debounce timer does after a shutdown drain
// has already run.
func TestPropFlushIsExactlyOnce(t *testing.T) {
	for seed := uint64(0); seed < coalescerCases; seed++ {
		r := rand.New(rand.NewPCG(seed, coalescerKey+1))
		c := coalescer{pending: map[string]*ranges.Set{}}

		for i, n := 0, r.IntN(8)+1; i < n; i++ {
			c.markContent("f.bin", randEvent(r, 64))
		}
		if got := c.flush("f.bin"); len(got) != 1 {
			t.Fatalf("seed %d: first flush = %d tasks; want 1", seed, len(got))
		}
		if got := c.flush("f.bin"); got != nil {
			t.Fatalf("seed %d: second flush = %v; want nothing", seed, opPaths(got))
		}
		if got := c.flushAll(); len(got) != 0 {
			t.Fatalf("seed %d: flushAll after flush = %v; want nothing", seed, opPaths(got))
		}
	}
}
