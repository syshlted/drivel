package fsevent

import (
	"reflect"
	"testing"

	"github.com/zishmusic/drivel/ranges"
)

// The package is types, so these are the three things about the types that can
// actually break something else (DESIGN.md §9, M0 item 6): the Op strings are
// part of the log format, nil Dirty has to stay representable, and Dirty is
// shared by pointer with whoever produced it.

// The Op values are not internal names. Engine.logEvent prints them into a
// fixed-width column (`[sync] %-7s path`), and the multi-client rig derives its
// upload/download counters by grepping that output — so renaming an op, or
// adding one longer than the column, changes both what a user's log says and
// what every measurement taken from it means.
func TestOpValuesAreStableAndFitTheLogColumn(t *testing.T) {
	const logColumn = 7 // the %-7s in syncengine.logEvent

	want := map[Op]string{
		OpCreate:  "create",
		OpWrite:   "write",
		OpMkdir:   "mkdir",
		OpRmdir:   "rmdir",
		OpUnlink:  "unlink",
		OpRename:  "rename",
		OpSetattr: "setattr",
	}

	seen := map[string]Op{}
	for op, s := range want {
		if string(op) != s {
			t.Errorf("Op %q is spelled %q; the log format and the rig's counters assume %q", s, string(op), s)
		}
		if len(op) > logColumn {
			t.Errorf("Op %q is %d chars; the log column is %d and would be pushed out of alignment", op, len(op), logColumn)
		}
		if prev, dup := seen[string(op)]; dup {
			t.Errorf("Op %q collides with %q; two ops that log the same are indistinguishable", op, prev)
		}
		seen[string(op)] = op
	}
}

// nil Dirty means "extents unknown, push the whole file", and it is the value an
// event gets by saying nothing. That is only true while the field is a pointer:
// as a plain ranges.Set the zero value would be an empty extent list, which
// reads as "nothing changed" and would push no bytes at all. The fail-safe
// default depends on the field's type, so pin the type.
func TestUnsetDirtyMeansUnknownExtents(t *testing.T) {
	var zero Event
	if zero.Dirty != nil {
		t.Errorf("the zero Event carries extents %+v; unknown is the only safe default", zero.Dirty)
	}

	// The shape every synthesised event has — no handle behind it, so no
	// extents to report.
	ev := Event{Op: OpSetattr, Path: "f.bin"}
	if ev.Dirty != nil {
		t.Errorf("an event built without extents carries %+v; want nil", ev.Dirty)
	}

	f, ok := reflect.TypeOf(Event{}).FieldByName("Dirty")
	if !ok {
		t.Fatal("Event has no Dirty field")
	}
	if f.Type.Kind() != reflect.Pointer {
		t.Fatalf("Dirty is %s; it must stay a pointer, or \"unset\" becomes \"nothing changed\" and the push ships no bytes", f.Type)
	}
}

// Dirty is shared, not copied: an Event travels to the uploader by value but its
// extents travel by pointer. A producer that keeps writing after it emits the
// event must hand over a Clone — which is what vfs's dirtyTracker.snapshot does,
// and what this pins, because the failure is a set that grows under a task
// already queued.
func TestDirtyMustBeClonedBeforeItIsHandedOver(t *testing.T) {
	const block = 64

	shared := ranges.New(4*block, block)
	shared.MarkCovering(0, 1)
	aliased := Event{Op: OpWrite, Path: "f.bin", Dirty: &shared}

	snapshot := shared.Clone()
	cloned := Event{Op: OpWrite, Path: "f.bin", Dirty: &snapshot}

	queued := aliased               // as if sent on the channel
	safe := cloned                  //
	shared.MarkCovering(3*block, 1) // the handle keeps writing

	if len(queued.Dirty.Extents()) != 2 {
		t.Errorf("copying an Event deep-copied its extents; the alias this test describes is gone, and so is the reason to Clone")
	}
	if got := len(safe.Dirty.Extents()); got != 1 {
		t.Errorf("a cloned set grew with the original: %d extents, want 1", got)
	}
}
