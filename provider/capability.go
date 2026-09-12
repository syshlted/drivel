package provider

import (
	"sort"
	"strconv"
	"strings"
)

// Capability names one of the optional interfaces in this package.
//
// Until M9 the only way to ask whether a store had one was a type assertion:
//
//	if cs, ok := store.(provider.ChangeSource); ok { … }
//
// That works exactly as long as every store is a Go value the engine can see the
// method set of. An out-of-process plugin is not: the host side of the seam is
// one generated proxy type, and a proxy that implements ChangeSource for the one
// backend that has a change feed implements it for every backend that does not.
// Type assertion would answer "yes" for all five capabilities, for every plugin,
// and the pull loop would poll a feed that does not exist.
//
// So the question moves into the seam. A store may DECLARE what it offers, and
// callers ask through Capabilities and the As* accessors rather than asserting.
//
// The bits are stable and additive: a new capability takes the next one, and an
// unknown bit from a newer plugin is masked off rather than misread. They are
// not a wire format on their own — the plugin protocol carries capability
// *names*, so a host and a plugin built against different versions of this
// package still agree on which is which (see the plugin package).
type Capability uint32

// The optional interfaces, one bit each. Keep the order of declaration: the
// String forms below are indexed by it, and the plugin protocol's name table is
// built from the same list.
const (
	// CapChangeSource is ChangeSource: an incremental inbound change feed.
	CapChangeSource Capability = 1 << iota
	// CapEnumerator is Enumerator: a complete listing for the M7b sweep.
	CapEnumerator
	// CapRangeGetter is RangeGetter: ranged reads, used by M5 hydration.
	CapRangeGetter
	// CapRangePutter is RangePutter: in-place extent writes, used by M6.
	CapRangePutter
	// CapContentHasher is ContentHasher: a local digest in the provider's own
	// encoding, used by M6's unchanged-content gate.
	CapContentHasher

	// capMax is one past the last defined bit. Everything at or above it is a
	// capability this build does not know about.
	capMax
)

// capNames is the canonical name of each capability, in bit order. These strings
// cross the plugin protocol, so they are part of its compatibility surface:
// rename one and a plugin built against the old name loses that capability
// silently. Add, never rename.
var capNames = [...]string{
	"change-source",
	"enumerator",
	"range-getter",
	"range-putter",
	"content-hasher",
}

// String names the capability, or reports the raw bit if it is one this build
// does not know.
func (c Capability) String() string {
	for i, name := range capNames {
		if c == 1<<uint(i) {
			return name
		}
	}
	return "capability(" + strconv.FormatUint(uint64(c), 10) + ")"
}

// CapabilityByName resolves a name from the wire back to its bit. An unknown
// name reports false rather than erroring: a plugin built against a newer
// version of this package may offer capabilities this host has no code to call,
// and the right response is to ignore them, not to refuse the plugin.
func CapabilityByName(name string) (Capability, bool) {
	for i, n := range capNames {
		if n == name {
			return 1 << uint(i), true
		}
	}
	return 0, false
}

// CapabilitySet is a set of capabilities.
type CapabilitySet uint32

// AllCapabilities is every capability this build knows about. It is the mask
// applied to anything arriving from outside the process.
const AllCapabilities = CapabilitySet(capMax - 1)

// Has reports whether the set contains c.
func (s CapabilitySet) Has(c Capability) bool { return s&CapabilitySet(c) != 0 }

// With returns s plus c.
func (s CapabilitySet) With(c Capability) CapabilitySet { return s | CapabilitySet(c) }

// Without returns s minus c.
func (s CapabilitySet) Without(c Capability) CapabilitySet { return s &^ CapabilitySet(c) }

// Names lists the set's capabilities by name, in bit order, dropping any bit
// this build does not know. It is what the plugin protocol sends and what a log
// line prints.
func (s CapabilitySet) Names() []string {
	var out []string
	for i := range capNames {
		if c := Capability(1 << uint(i)); s.Has(c) {
			out = append(out, capNames[i])
		}
	}
	return out
}

// String renders the set for a log line: "change-source, enumerator", or "none".
func (s CapabilitySet) String() string {
	names := s.Names()
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// CapabilitySetFromNames builds a set from wire names, ignoring any it does not
// recognise. It also reports the unknown ones, so a caller that wants to say
// "this plugin offers something this drivel is too old to use" can.
func CapabilitySetFromNames(names []string) (set CapabilitySet, unknown []string) {
	for _, n := range names {
		if c, ok := CapabilityByName(n); ok {
			set = set.With(c)
			continue
		}
		unknown = append(unknown, n)
	}
	sort.Strings(unknown)
	return set, unknown
}

// Declarer is an OPTIONAL interface a Store implements to say which optional
// capabilities it offers, instead of leaving the answer to its method set.
//
// Only a store whose method set is not the truth needs it — in practice the
// plugin proxy, which has every method because it is generated once for every
// backend. An ordinary in-process provider implements the interfaces it can
// honour and nothing else, which is already an exact answer; gdrive and sftp do
// not implement this and should not start.
//
// A declaration may only NARROW. See Capabilities.
type Declarer interface {
	Capabilities() CapabilitySet
}

// Capabilities reports which optional interfaces s actually offers, and is the
// only supported way to ask.
//
// The result is the intersection of two things: what s's method set can do, and
// what s declares if it is a Declarer. That direction is the load-bearing part —
// **a declaration narrows and can never widen.** A store cannot talk its way
// into a capability it has no method for, so the worst a wrong declaration can
// do is cost a feature, never produce a call into a method that is not there.
// The failure that matters is the other one: a proxy that answers "yes" to a
// capability its backend does not have makes the engine poll a feed that returns
// nothing forever, or patch extents into a store that cannot patch.
//
// A nil Store has no capabilities.
func Capabilities(s Store) CapabilitySet {
	if s == nil {
		return 0
	}
	var have CapabilitySet
	if _, ok := s.(ChangeSource); ok {
		have = have.With(CapChangeSource)
	}
	if _, ok := s.(Enumerator); ok {
		have = have.With(CapEnumerator)
	}
	if _, ok := s.(RangeGetter); ok {
		have = have.With(CapRangeGetter)
	}
	if _, ok := s.(RangePutter); ok {
		have = have.With(CapRangePutter)
	}
	if _, ok := s.(ContentHasher); ok {
		have = have.With(CapContentHasher)
	}
	if d, ok := s.(Declarer); ok {
		have &= d.Capabilities() & AllCapabilities
	}
	return have
}

// AsChangeSource returns s as a ChangeSource if it offers that capability.
//
// Callers use this instead of a type assertion; see Capability for why the
// assertion stopped being the truth when a store could live in another process.
func AsChangeSource(s Store) (ChangeSource, bool) {
	if !Capabilities(s).Has(CapChangeSource) {
		return nil, false
	}
	cs, ok := s.(ChangeSource)
	return cs, ok
}

// AsEnumerator returns s as an Enumerator if it offers that capability.
func AsEnumerator(s Store) (Enumerator, bool) {
	if !Capabilities(s).Has(CapEnumerator) {
		return nil, false
	}
	e, ok := s.(Enumerator)
	return e, ok
}

// AsRangeGetter returns s as a RangeGetter if it offers that capability.
func AsRangeGetter(s Store) (RangeGetter, bool) {
	if !Capabilities(s).Has(CapRangeGetter) {
		return nil, false
	}
	rg, ok := s.(RangeGetter)
	return rg, ok
}

// AsRangePutter returns s as a RangePutter if it offers that capability.
func AsRangePutter(s Store) (RangePutter, bool) {
	if !Capabilities(s).Has(CapRangePutter) {
		return nil, false
	}
	rp, ok := s.(RangePutter)
	return rp, ok
}

// AsContentHasher returns s as a ContentHasher if it offers that capability.
func AsContentHasher(s Store) (ContentHasher, bool) {
	if !Capabilities(s).Has(CapContentHasher) {
		return nil, false
	}
	h, ok := s.(ContentHasher)
	return h, ok
}
