package vfs

import (
	"slices"
	"strings"
	"testing"

	"github.com/zishmusic/drivel/internal/mount"
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

// An option list from outside drivel cannot turn off what drivel makes
// compulsory.
//
// This is the seam where the two halves meet: the mount(8) helper forwards an
// administrator's `-o` list from /etc/fstab verbatim, and `dev` and `suid` are
// ordinary options meaning exactly the opposite of what every mount is supposed
// to get. go-fuse resolves the list by walking it and setting or clearing a bit
// per entry, so the guarantee is only as good as the order withCompulsory
// produces — which makes this a test about a slice and not about a style.
func TestCallerOptionsCannotClearTheCompulsoryOnes(t *testing.T) {
	got := withCompulsory([]string{"dev", "suid", "allow_other"})

	// Everything the caller asked for survives; drivel adds rather than censors.
	for _, want := range []string{"dev", "suid", "allow_other"} {
		if !slices.Contains(got, want) {
			t.Errorf("withCompulsory(...) = %v; dropped the caller's %q", got, want)
		}
	}
	// And every compulsory option comes after every caller option, so it is the
	// one that decides.
	for _, opt := range compulsoryOptions() {
		i := slices.Index(got, opt)
		if i < 0 {
			t.Fatalf("withCompulsory(...) = %v; missing the compulsory %q", got, opt)
		}
		if negation := strings.TrimPrefix(opt, "no"); negation != opt {
			if j := slices.Index(got, negation); j > i {
				t.Errorf("withCompulsory(...) = %v; %q at %d comes after %q at %d, so it wins",
					got, negation, j, opt, i)
			}
		}
	}
}

// The same claim, against the kernel rather than against a slice: a mount whose
// caller asked for dev and suid is still nodev and nosuid.
//
// It is the weaker of the two and deliberately kept anyway. Reversing the order
// in withCompulsory fails the test above and this one still passes, because on
// the unprivileged path fusermount3 forces both flags whatever the option list
// says — so on this machine the ordering is belt to fusermount's braces. The
// braces are not guaranteed: they come off for a mount made as root, and on
// go-fuse's direct-mount path there is no fusermount in the story at all. The
// slice test is what actually pins the guarantee; this one pins the outcome a
// user gets, and the two failing separately is the point.
func TestCallerOptionsCannotClearThemThroughARealMount(t *testing.T) {
	mnt, _ := mountTest(t, t.TempDir(), nil, func(o *mount.Options) {
		o.BackendOptions = []string{"dev", "suid"}
	})

	nosuid, nodev, nodevKnown := mountFlags(t, mnt)
	if !nosuid {
		t.Errorf("an fstab line saying suid produced a mount that is not nosuid")
	}
	if nodevKnown && !nodev {
		t.Errorf("an fstab line saying dev produced a mount that is not nodev")
	}
}
