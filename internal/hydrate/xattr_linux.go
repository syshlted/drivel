//go:build linux

package hydrate

import "golang.org/x/sys/unix"

// errNoAttr is the errno the xattr calls return for "no such attribute". Linux
// spells it ENODATA; ENOATTR is a C-level alias for the same value that x/sys
// does not define here.
const errNoAttr = unix.ENODATA

// xattrNative reports whether the attribute the probe just wrote is held by the
// filesystem itself. On Linux a successful setxattr is the whole answer: there is
// no emulation layer to mistake it for. See the darwin implementation for why the
// question is worth asking at all.
func xattrNative(string) bool { return true }
