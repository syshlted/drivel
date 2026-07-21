//go:build !linux

package mount

import (
	"errors"
	"io"
)

// openInPlace is unsupported off Linux: the in-place trick relies on the
// /proc/self/fd magic symlink. Other Unix backends would use fd-relative *at
// syscalls instead (see DESIGN.md), which is future work. Until then, non-Linux
// platforms must use a separate backing directory (-data).
func openInPlace(dir string) (string, io.Closer, error) {
	return "", nil, errors.New("in-place mode (single directory) is only supported on Linux; pass -data to use a separate backing directory")
}
