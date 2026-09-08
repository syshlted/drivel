//go:build freebsd

package vfs

import (
	"testing"

	"golang.org/x/sys/unix"
)

// mountFlags asks the kernel what a mounted filesystem actually enforces. See the
// linux file for the contract.
//
// FreeBSD reports no nodev, and that is the kernel's position rather than a hole
// in this helper: MNT_NODEV was removed — it is not in x/sys/unix for freebsd at
// all — because only devfs may hold device nodes, so a device node anywhere else
// is inert whether or not a flag says so. Reporting nodevKnown = false is what
// keeps the test from asserting a guarantee this platform expresses by
// construction instead of by flag; see compulsoryOptions in mountopts_freebsd.go.
func mountFlags(t *testing.T, path string) (nosuid, nodev, nodevKnown bool) {
	t.Helper()
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		t.Fatalf("statfs %s: %v", path, err)
	}
	return st.Flags&unix.MNT_NOSUID != 0, false, false
}
