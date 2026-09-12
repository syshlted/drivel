package provider

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/zishmusic/drivel/ranges"
)

// bare implements only the required surface.
type bare struct{}

func (bare) Put(context.Context, string, io.Reader) (RemoteFile, error) {
	return RemoteFile{}, nil
}
func (bare) Mkdir(context.Context, string) (RemoteFile, error)        { return RemoteFile{}, nil }
func (bare) Move(context.Context, string, string) (RemoteFile, error) { return RemoteFile{}, nil }
func (bare) Remove(context.Context, string) error                     { return nil }
func (bare) Get(context.Context, string) (io.ReadCloser, error)       { return nil, nil }
func (bare) Stat(context.Context, string) (RemoteFile, bool, error)   { return RemoteFile{}, false, nil }

// full implements everything, the way a plugin proxy does.
type full struct{ bare }

func (full) StartCursor(context.Context) (string, error) { return "", nil }
func (full) Changes(context.Context, string) ([]RemoteChange, string, error) {
	return nil, "", nil
}
func (full) Enumerate(context.Context, string) ([]RemoteFile, string, error) { return nil, "", nil }
func (full) GetRange(context.Context, string, int64, int64) (io.ReadCloser, error) {
	return nil, nil
}
func (full) PutRange(context.Context, string, io.ReaderAt, int64, []ranges.Range) (RemoteFile, error) {
	return RemoteFile{}, nil
}
func (full) HashContent(io.Reader) (string, error) { return "", nil }

// declaring is full with a declaration, which is what the plugin proxy is.
type declaring struct {
	full
	set CapabilitySet
}

func (d declaring) Capabilities() CapabilitySet { return d.set }

func TestCapabilitiesFromTheMethodSet(t *testing.T) {
	if got := Capabilities(bare{}); got != 0 {
		t.Errorf("a store with no optional methods reports %s", got)
	}
	if got := Capabilities(full{}); got != AllCapabilities {
		t.Errorf("a store with every optional method reports %s", got)
	}
	if got := Capabilities(nil); got != 0 {
		t.Errorf("a nil store reports %s", got)
	}
}

// The whole point of the mechanism: one type with every method, narrowed to what
// the backend behind it can really do.
func TestDeclarationNarrows(t *testing.T) {
	d := declaring{set: CapabilitySet(0).With(CapEnumerator).With(CapRangeGetter)}
	got := Capabilities(d)
	if !got.Has(CapEnumerator) || !got.Has(CapRangeGetter) {
		t.Errorf("declared capabilities lost: %s", got)
	}
	if got.Has(CapChangeSource) || got.Has(CapRangePutter) || got.Has(CapContentHasher) {
		t.Errorf("undeclared capabilities reported: %s", got)
	}
	if _, ok := AsChangeSource(d); ok {
		t.Error("AsChangeSource succeeded for a store that did not declare it")
	}
	if _, ok := AsEnumerator(d); !ok {
		t.Error("AsEnumerator failed for a store that declared it")
	}
}

// A declaration must never be able to ADD a capability. If it could, the engine
// would call a method that is not there — and the whole failure mode this
// mechanism exists to prevent is a store being asked to do what it cannot.
func TestDeclarationCannotWiden(t *testing.T) {
	// A store with no optional methods that claims all of them.
	type liar struct {
		bare
	}
	got := Capabilities(struct {
		liar
		Declarer
	}{Declarer: constantDeclarer(AllCapabilities)})
	if got != 0 {
		t.Errorf("a store with no optional methods was credited with %s", got)
	}
}

// constantDeclarer is a Declarer that always answers the same set.
type constantDeclarer CapabilitySet

func (c constantDeclarer) Capabilities() CapabilitySet { return CapabilitySet(c) }

// A bit this build does not know about must be dropped, not misread as one it
// does. That is what lets a newer plugin talk to an older drivel.
func TestUnknownCapabilityBitsAreMasked(t *testing.T) {
	future := AllCapabilities | CapabilitySet(1<<20)
	got := Capabilities(declaring{set: future})
	if got != AllCapabilities {
		t.Errorf("Capabilities = %s; a bit from the future should be dropped", got)
	}
}

func TestCapabilityNamesRoundTrip(t *testing.T) {
	for _, c := range []Capability{
		CapChangeSource, CapEnumerator, CapRangeGetter, CapRangePutter, CapContentHasher,
	} {
		got, ok := CapabilityByName(c.String())
		if !ok || got != c {
			t.Errorf("CapabilityByName(%q) = %v, %v", c.String(), got, ok)
		}
	}
}

// The wire carries names, and an unrecognised one is ignored rather than
// refused: a plugin offering something this build cannot call should lose the
// capability, not the connection.
func TestCapabilitySetFromNamesIgnoresUnknowns(t *testing.T) {
	set, unknown := CapabilitySetFromNames([]string{"enumerator", "teleporter", "change-source"})
	if !set.Has(CapEnumerator) || !set.Has(CapChangeSource) {
		t.Errorf("known names lost: %s", set)
	}
	if !slices.Equal(unknown, []string{"teleporter"}) {
		t.Errorf("unknown = %v; want [teleporter]", unknown)
	}
}

// The names are a compatibility surface: renaming one silently drops that
// capability for every plugin built against the old spelling. Pinning them here
// makes a rename a test failure instead.
func TestCapabilityNamesArePinned(t *testing.T) {
	want := []string{"change-source", "enumerator", "range-getter", "range-putter", "content-hasher"}
	if got := AllCapabilities.Names(); !slices.Equal(got, want) {
		t.Errorf("capability names = %v; want %v (these cross the plugin protocol)", got, want)
	}
}

func TestCapabilitySetString(t *testing.T) {
	if got := CapabilitySet(0).String(); got != "none" {
		t.Errorf("empty set renders as %q; want \"none\"", got)
	}
	got := CapabilitySet(0).With(CapEnumerator).With(CapRangePutter).String()
	if !strings.Contains(got, "enumerator") || !strings.Contains(got, "range-putter") {
		t.Errorf("set renders as %q", got)
	}
}
