//go:build linux || darwin

package hydrate

import (
	"errors"

	"golang.org/x/sys/unix"
)

// xattrSupported reports whether this build can read/write user xattrs at all.
const xattrSupported = true

// One implementation for two kernels, on purpose. The system calls are not the
// same — macOS carries an extra position argument (meaningful only for resource
// forks) and an options word where Linux has flags — but x/sys/unix normalises
// both to the Linux shape, so the only genuinely per-platform pieces left are
// errNoAttr and xattrNative, one line and one probe each. Sharing the body is
// what makes a Linux CI run cover the code macOS executes: the untested delta on
// a machine nobody has is two symbols rather than a file. See DESIGN.md §9, M10.
//
// All three follow symlinks, as the Linux implementation always did. The backing
// store holds regular files; a placeholder marker on a symlink would describe the
// wrong object.

func getxattr(path, attr string) ([]byte, error) {
	// Two-step: size the value, then read it. The attribute is small (a JSON
	// marker), so a single retry on growth is enough.
	sz, err := unix.Getxattr(path, attr, nil)
	if err != nil {
		return nil, err
	}
	if sz == 0 {
		return nil, nil
	}
	buf := make([]byte, sz)
	n, err := unix.Getxattr(path, attr, buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

func setxattr(path, attr string, data []byte) error {
	return unix.Setxattr(path, attr, data, 0)
}

func removexattr(path, attr string) error {
	return unix.Removexattr(path, attr)
}

// isNoAttr reports whether err means "this attribute is not set" as opposed to a
// real I/O failure. The errno differs by kernel — see errNoAttr — and getting it
// wrong turns a missing marker into an I/O error, which IsPlaceholder fails safe
// on by reporting "placeholder" and never pushing the file again.
func isNoAttr(err error) bool {
	return errors.Is(err, errNoAttr) || errors.Is(err, unix.ENOENT)
}

// isNoSupport reports whether err means the filesystem cannot store user xattrs.
// Both spellings are checked because they are the same value on Linux and two
// different ones on macOS.
func isNoSupport(err error) bool {
	return errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP)
}

// xattrName is one string carrying both the namespace and the name on these two
// kernels. FreeBSD splits them, which is why the exported XattrName is assembled
// per platform rather than written once — see xattr_freebsd.go.
const xattrName = "user.drivel.placeholder"
