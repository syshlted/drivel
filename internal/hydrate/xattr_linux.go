//go:build linux

package hydrate

import (
	"errors"
	"syscall"
)

// xattrSupported reports whether this build can read/write user xattrs at all.
const xattrSupported = true

func getxattr(path, attr string) ([]byte, error) {
	// Two-step: size the value, then read it. The attribute is small (a JSON
	// marker), so a single retry on growth is enough.
	sz, err := syscall.Getxattr(path, attr, nil)
	if err != nil {
		return nil, err
	}
	if sz == 0 {
		return nil, nil
	}
	buf := make([]byte, sz)
	n, err := syscall.Getxattr(path, attr, buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

func setxattr(path, attr string, data []byte) error {
	return syscall.Setxattr(path, attr, data, 0)
}

func removexattr(path, attr string) error {
	return syscall.Removexattr(path, attr)
}

// isNoAttr reports whether err means "this attribute is not set" as opposed to a
// real I/O failure. ENODATA is the Linux spelling; ENOATTR is an alias for it.
func isNoAttr(err error) bool {
	return errors.Is(err, syscall.ENODATA) || errors.Is(err, syscall.ENOENT)
}

// isNoSupport reports whether err means the filesystem cannot store user xattrs.
func isNoSupport(err error) bool {
	return errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP)
}
