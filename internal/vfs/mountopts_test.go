package vfs

import (
	"slices"
	"testing"
)

// Whatever else a platform can express, nosuid is not negotiable. This is the
// pure half of the guarantee — that drivel asks — and it runs everywhere,
// including where no FUSE mount can be made.
func TestCompulsoryOptionsAlwaysRefuseSetuid(t *testing.T) {
	opts := compulsoryOptions()
	if !slices.Contains(opts, "nosuid") {
		t.Fatalf("compulsoryOptions() = %v; every platform must request nosuid", opts)
	}
	// Nothing may ask for the opposite of what this list exists to guarantee. A
	// stray "suid" or "dev" would be translated by go-fuse into clearing the very
	// mount flag the list is here to set, and it would do it silently.
	for _, bad := range []string{"suid", "dev", "exec"} {
		if slices.Contains(opts, bad) {
			t.Errorf("compulsoryOptions() = %v; contains %q, which clears the flag it must set", opts, bad)
		}
	}
}

// The mount that comes up is nosuid and (where the kernel has the flag) nodev.
//
// This is the half that matters, because the option strings are only a request:
// they travel through a different code path per platform — fusermount3's option
// parser, go-fuse's direct-mount flag translation, mount_fusefs's fixed table —
// and any of them could drop one without saying so. Asking statfs is asking the
// kernel what it will actually enforce.
//
// A setuid binary or a device node arriving from a remote is never something a
// user asked for. The remote half of that is not testable here; that a file
// carrying the bits is inert at the mountpoint is.
func TestMountRefusesSetuidAndDevices(t *testing.T) {
	mnt, _ := mountTest(t, t.TempDir(), nil)

	nosuid, nodev, nodevKnown := mountFlags(t, mnt)
	if !nosuid {
		t.Errorf("mount is not nosuid: a setuid bit arriving from the remote would be live at the mountpoint")
	}
	if nodevKnown && !nodev {
		t.Errorf("mount is not nodev: a device node arriving from the remote would be live at the mountpoint")
	}
}
