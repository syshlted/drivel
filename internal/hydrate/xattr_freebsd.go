//go:build freebsd

package hydrate

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// xattrSupported reports whether this build can read/write user xattrs at all.
const xattrSupported = true

// xattrName is the placeholder marker, spelled differently here on purpose.
// Linux and macOS take one string in which "user." is a namespace prefix;
// FreeBSD takes the namespace as a separate argument and the name without it.
// Same attribute in the same namespace, two spellings — which is why XattrName is
// per platform and why nothing may hardcode the Linux form.
//
// A backing tree therefore carries markers that a mount on another OS would not
// find: they are different attributes to the kernel, and an unfound marker means
// an unmarked file, which the uploader will push. Copying a lazy backing store
// between operating systems is not safe — but neither is copying one at all, since
// no ordinary copy tool preserves these attributes either.
const xattrName = "drivel.placeholder"

// attrNamespace is the unprivileged namespace. EXTATTR_NAMESPACE_SYSTEM needs
// root and belongs to the kernel; a sync tool writing there is DESIGN.md §10.4's
// executable-trust mistake with a different spelling.
const attrNamespace = unix.EXTATTR_NAMESPACE_USER

// The extattr_* family is a genuinely different interface from the one
// xattr_unix.go shares between Linux and macOS, which is why this file implements
// the three calls rather than joining that build tag: the namespace is an
// argument, the buffer is a raw address rather than a slice, and set reports how
// many bytes it wrote instead of succeeding wholesale.
//
// All three follow symlinks (extattr_get_file, not extattr_get_link), matching the
// other platforms: the backing store holds regular files, and a marker on a
// symlink would describe the wrong object.

// extattrGet and extattrSet exist only to carry //go:uintptrescapes, and that
// pragma is the load-bearing part of this file.
//
// x/sys types the buffer as a uintptr, so the address of a Go slice crosses a
// function boundary as an integer. A uintptr is not a reference: it does not keep
// the array alive, and — the case runtime.KeepAlive cannot fix — it is not
// rewritten if the goroutine's stack is copied to grow it. The wrappers below call
// BytePtrFromString twice before reaching the kernel, each an allocation that can
// trigger exactly that growth, so a stack-allocated marker could be read from or
// written to at an address that no longer holds it. Escape analysis confirms the
// exposure is real rather than theoretical: without the pragma the compiler
// reports setxattr's data as not escaping, which is the compiler saying it may
// leave the caller's buffer on the stack.
//
// //go:uintptrescapes tells the compiler that a pointer converted to uintptr in a
// call to these functions must be heap-allocated and kept alive for the whole
// call, nested calls included. The conversion therefore has to appear at the call
// site, not behind a helper, which is why the argument is spelled out twice below.

//go:uintptrescapes
func extattrGet(path, attr string, data uintptr, nbytes int) (int, error) {
	return unix.ExtattrGetFile(path, attrNamespace, attr, data, nbytes)
}

//go:uintptrescapes
func extattrSet(path, attr string, data uintptr, nbytes int) (int, error) {
	return unix.ExtattrSetFile(path, attrNamespace, attr, data, nbytes)
}

func getxattr(path, attr string) ([]byte, error) {
	// Two-step: a null buffer asks for the size, then read it. The attribute is
	// small (a JSON marker), so a single retry on growth is enough.
	sz, err := extattrGet(path, attr, 0, 0)
	if err != nil {
		return nil, err
	}
	if sz <= 0 {
		return nil, nil
	}
	buf := make([]byte, sz)
	n, err := extattrGet(path, attr, uintptr(unsafe.Pointer(&buf[0])), len(buf))
	if err != nil {
		return nil, err
	}
	if n > len(buf) {
		n = len(buf)
	}
	return buf[:n], nil
}

func setxattr(path, attr string, data []byte) error {
	var (
		n   int
		err error
	)
	if len(data) == 0 {
		n, err = extattrSet(path, attr, 0, 0)
	} else {
		n, err = extattrSet(path, attr, uintptr(unsafe.Pointer(&data[0])), len(data))
	}
	if err != nil {
		return err
	}
	// A short write is not something the other platforms can report, and it must
	// not pass for success: a truncated marker is unparseable JSON, which Marker
	// reads as "placeholder, contents unknown" — safe, but permanently unpushable.
	if n != len(data) {
		return fmt.Errorf("extattr_set_file wrote %d of %d bytes to %s", n, len(data), path)
	}
	return nil
}

func removexattr(path, attr string) error {
	return unix.ExtattrDeleteFile(path, attrNamespace, attr)
}

// isNoAttr reports whether err means "this attribute is not set" as opposed to a
// real I/O failure. FreeBSD spells it ENOATTR, as macOS does and unlike Linux's
// ENODATA; getting it wrong turns a missing marker into an I/O error, which
// IsPlaceholder fails safe on by reporting "placeholder" and never pushing the
// file again.
func isNoAttr(err error) bool {
	return errors.Is(err, unix.ENOATTR) || errors.Is(err, unix.ENOENT)
}

// isNoSupport reports whether err means the filesystem cannot store user
// attributes — EOPNOTSUPP here, and the case is commoner than on Linux: tmpfs has
// no extended attributes at all, so a backing directory under a tmpfs /tmp lands
// on this path rather than on a working marker.
func isNoSupport(err error) bool {
	return errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOTSUP)
}

// xattrNative reports whether the attribute the probe just wrote is held by the
// filesystem itself. FreeBSD has no emulation layer to mistake it for; see the
// darwin implementation for why the question is asked at all.
func xattrNative(string) bool { return true }
