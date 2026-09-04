package gdrive

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	drive "google.golang.org/api/drive/v3"
)

// The child index (kids) is what makes forgetLocked and reindexLocked cost a
// subtree rather than a scan of every path we know. It is derived state, so the
// risk it introduces is drift: a kids set that disagrees with idByPath makes
// forgetLocked under-delete, leaving a stale mapping that resolves a change-feed
// ID to a path it no longer occupies — the M7 data-loss shape. These tests are
// the guard against that, and they check the property rather than a scenario.

// assertKidsConsistent checks the invariant the traversal relies on: every path
// in idByPath is reachable from the root by walking kids, and kids contains no
// entry that describes nothing.
func assertKidsConsistent(t *testing.T, d *Drive) {
	t.Helper()
	reachable := map[string]bool{"": true}
	stack := []string{""}
	for len(stack) > 0 {
		p := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for q := range d.kids[p] {
			if reachable[q] {
				t.Fatalf("kids graph reaches %q twice", q)
			}
			reachable[q] = true
			stack = append(stack, q)
		}
	}
	for p := range d.idByPath {
		if !reachable[p] {
			t.Fatalf("path %q is in idByPath but unreachable through kids (kids=%v)", p, d.kids)
		}
	}
	for parent, set := range d.kids {
		if len(set) == 0 {
			t.Fatalf("kids[%q] is an empty set; it should have been pruned", parent)
		}
		for q := range set {
			if got := parentOf(q); got != parent {
				t.Fatalf("kids[%q] holds %q, whose parent is %q", parent, q, got)
			}
			_, mapped := d.idByPath[q]
			if !mapped && len(d.kids[q]) == 0 {
				t.Fatalf("kids[%q] holds %q, which has no mapping and no children", parent, q)
			}
		}
	}
}

// wantForgotten is the reference the fast traversal has to match: the old
// full-scan semantics, except at the root, where forgetting "" now empties the
// index to agree with pathindex.Forget("") (see forgetLocked's comment).
func wantForgotten(d *Drive, p string) map[string]bool {
	out := map[string]bool{}
	prefix := p + "/"
	for q := range d.idByPath {
		if p == "" || q == p || strings.HasPrefix(q, prefix) {
			out[q] = true
		}
	}
	return out
}

func TestForgetLockedMatchesFullScan(t *testing.T) {
	paths := []string{"a", "a/b", "a/b/c.txt", "a/b/d.txt", "ab", "ab/x.txt", "a2", "z.txt"}
	for _, target := range append([]string{""}, paths...) {
		d := newIndex()
		for i, p := range paths {
			d.rememberLocked(ctx, p, &drive.File{Id: fmt.Sprintf("id-%d", i)})
		}
		want := wantForgotten(d, target)

		before := map[string]string{}
		for p, id := range d.idByPath {
			before[p] = id
		}
		d.forgetLocked(ctx, target)

		for p := range before {
			_, still := d.idByPath[p]
			if want[p] && still {
				t.Errorf("forgetLocked(%q): %q survived but the full scan would drop it", target, p)
			}
			if !want[p] && !still {
				t.Errorf("forgetLocked(%q): %q was dropped but the full scan would keep it", target, p)
			}
		}
		assertIndexConsistent(t, d)
		assertKidsConsistent(t, d)
	}
}

// A path can be recorded before its parents are (the sweep lists a flat tree in
// no particular order), so the ancestor chain has to be linked on the way up or
// the subtree walk misses it.
func TestForgetLockedFindsChildrenOfUnrecordedParents(t *testing.T) {
	d := newIndex()
	d.rememberLocked(ctx, "a/b/c.txt", &drive.File{Id: "id-c"})

	if _, ok := d.idByPath["a"]; ok {
		t.Fatal("test premise broken: 'a' should not be recorded")
	}
	d.forgetLocked(ctx, "a")

	if _, ok := d.idByPath["a/b/c.txt"]; ok {
		t.Fatal("forgetLocked(\"a\") missed a descendant whose parents were never recorded")
	}
	assertIndexConsistent(t, d)
	assertKidsConsistent(t, d)
}

// Evicting one half of a rewrite must not orphan that path's descendants.
func TestLinkEvictionKeepsDescendantsReachable(t *testing.T) {
	d := newIndex()
	d.rememberLocked(ctx, "dir", &drive.File{Id: "id-dir"})
	d.rememberLocked(ctx, "dir/f.txt", &drive.File{Id: "id-f"})
	// The same ID turns up at a new path: "dir" moved, so its old entry is evicted.
	d.rememberLocked(ctx, "elsewhere", &drive.File{Id: "id-dir"})

	assertIndexConsistent(t, d)
	assertKidsConsistent(t, d)
	d.forgetLocked(ctx, "dir")
	if _, ok := d.idByPath["dir/f.txt"]; ok {
		t.Fatal("descendant of an evicted path became unreachable")
	}
	assertIndexConsistent(t, d)
	assertKidsConsistent(t, d)
}

func TestReindexLockedKeepsIndexesConsistent(t *testing.T) {
	d := newIndex()
	d.rememberLocked(ctx, "a", &drive.File{Id: "id-a"})
	d.rememberLocked(ctx, "a/x.txt", &drive.File{Id: "id-x"})
	d.rememberLocked(ctx, "a/sub/y.txt", &drive.File{Id: "id-y"})

	// A sibling rename whose old and new key ranges overlap as strings.
	d.reindexLocked(ctx, "a", "ab")

	for p, want := range map[string]string{"ab": "id-a", "ab/x.txt": "id-x", "ab/sub/y.txt": "id-y"} {
		if d.idByPath[p] != want {
			t.Errorf("idByPath[%q]=%q; want %q", p, d.idByPath[p], want)
		}
	}
	if _, ok := d.idByPath["a"]; ok {
		t.Error("old path survived the rename")
	}
	assertIndexConsistent(t, d)
	assertKidsConsistent(t, d)
}

// A randomized sequence of every mutation, checking both invariants after each
// step. This is the drift guard: the fast traversal is only safe while kids and
// idByPath describe the same tree.
func TestIndexInvariantsUnderRandomMutation(t *testing.T) {
	names := []string{"a", "ab", "a/b", "a/b/c", "a/b2", "abc/d", "z", "z/y/x"}
	rng := rand.New(rand.NewSource(20260904))
	d := newIndex()
	for step := 0; step < 4000; step++ {
		p := names[rng.Intn(len(names))]
		switch rng.Intn(4) {
		case 0, 1:
			d.rememberLocked(ctx, p, &drive.File{Id: fmt.Sprintf("id-%d", rng.Intn(12))})
		case 2:
			d.forgetLocked(ctx, p)
		case 3:
			d.reindexLocked(ctx, p, names[rng.Intn(len(names))])
		}
		assertIndexConsistent(t, d)
		assertKidsConsistent(t, d)
	}
}
