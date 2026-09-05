package vfs

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/zishmusic/drivel/internal/mount"
	"github.com/zishmusic/drivel/internal/testenv"
)

// The marker M5 keeps on the backing file. Spelled out rather than imported from
// internal/hydrate: what this package refuses is any user.* attribute, and a test
// that imported the constant would still pass if the guard covered only that name.
const placeholderMarker = "user.drivel.placeholder"

// backingHoldsXattrs reports whether dir's filesystem stores user.* attributes,
// which the passthrough test needs before it can assert what passthrough delivers.
func backingHoldsXattrs(t *testing.T, dir string) bool {
	t.Helper()
	probe := filepath.Join(dir, ".xattr-probe")
	if err := os.WriteFile(probe, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(probe)
	return syscall.Setxattr(probe, "user.drivel.probe", []byte("1"), 0) == nil
}

// unsupported reports whether err is the kernel's "this filesystem has no xattrs"
// answer. The node returns ENOSYS, which the kernel records and reports to the
// caller as EOPNOTSUPP.
func unsupported(err error) bool {
	return errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.ENOSYS)
}

// By default the mount answers xattr operations as unsupported instead of
// proxying them, in both directions. The write side is the one that matters: a
// SETXATTR or REMOVEXATTR reaching the backing store could forge or strip M5's
// placeholder marker, and go-fuse's DisableXAttrs does not cover those two
// opcodes (see xattr.go).
func TestXattrRefusedByDefault(t *testing.T) {
	backing := t.TempDir()
	if err := os.WriteFile(filepath.Join(backing, "f.txt"), []byte("body"), 0o644); err != nil {
		t.Fatal(err)
	}
	mnt, _ := mountTest(t, backing, nil)
	f := filepath.Join(mnt, "f.txt")

	if err := syscall.Setxattr(f, "user.test", []byte("v"), 0); !unsupported(err) {
		t.Errorf("setxattr through the mount = %v; want EOPNOTSUPP", err)
	}
	if _, err := syscall.Getxattr(f, "user.test", make([]byte, 64)); !unsupported(err) {
		t.Errorf("getxattr through the mount = %v; want EOPNOTSUPP", err)
	}
	if _, err := syscall.Listxattr(f, make([]byte, 256)); !unsupported(err) {
		t.Errorf("listxattr through the mount = %v; want EOPNOTSUPP", err)
	}
	if err := syscall.Removexattr(f, "user.test"); !unsupported(err) {
		t.Errorf("removexattr through the mount = %v; want EOPNOTSUPP", err)
	}

	// Nothing reached the backing file either, which is the claim the errno alone
	// does not make: an op could fail on the way out and still have landed.
	if _, err := syscall.Getxattr(filepath.Join(backing, "f.txt"), "user.test", make([]byte, 64)); err == nil {
		t.Error("user.test reached the backing file; the refusal came after the write")
	}
}

// The placeholder marker specifically: a placeholder in the backing store must
// not be strippable through the mountpoint, because that marker is what stops the
// uploader replacing the remote file's content with the placeholder's zeros
// (M5 invariant 2).
func TestXattrCannotStripPlaceholderMarker(t *testing.T) {
	backing := t.TempDir()
	if !backingHoldsXattrs(t, backing) {
		testenv.Unavailable(t, testenv.Xattr, "backing filesystem does not store user.* attributes")
	}
	ph := filepath.Join(backing, "lazy.txt")
	if err := os.WriteFile(ph, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Setxattr(ph, placeholderMarker, []byte(`{"v":1}`), 0); err != nil {
		t.Fatal(err)
	}

	mnt, _ := mountTest(t, backing, nil)
	if err := syscall.Removexattr(filepath.Join(mnt, "lazy.txt"), placeholderMarker); !unsupported(err) {
		t.Errorf("removexattr of the marker = %v; want EOPNOTSUPP", err)
	}
	if _, err := syscall.Getxattr(ph, placeholderMarker, make([]byte, 64)); err != nil {
		t.Errorf("marker gone from the backing file: %v", err)
	}
}

// With the option on, the mount is a plain loopback again: attributes set through
// the mountpoint land on the backing file and read back through either path.
func TestXattrPassthroughWhenEnabled(t *testing.T) {
	backing := t.TempDir()
	if !backingHoldsXattrs(t, backing) {
		testenv.Unavailable(t, testenv.Xattr, "backing filesystem does not store user.* attributes")
	}
	if err := os.WriteFile(filepath.Join(backing, "f.txt"), []byte("body"), 0o644); err != nil {
		t.Fatal(err)
	}

	mnt, _ := mountTest(t, backing, nil, func(o *mount.Options) { o.Xattr = true })
	f := filepath.Join(mnt, "f.txt")

	if err := syscall.Setxattr(f, "user.test", []byte("value"), 0); err != nil {
		t.Fatalf("setxattr through the mount: %v", err)
	}
	buf := make([]byte, 64)
	n, err := syscall.Getxattr(filepath.Join(backing, "f.txt"), "user.test", buf)
	if err != nil {
		t.Fatalf("the backing file did not receive it: %v", err)
	}
	if got := string(buf[:n]); got != "value" {
		t.Errorf("backing value = %q; want %q", got, "value")
	}

	if n, err = syscall.Getxattr(f, "user.test", buf); err != nil {
		t.Fatalf("getxattr through the mount: %v", err)
	}
	if got := string(buf[:n]); got != "value" {
		t.Errorf("value through the mount = %q; want %q", got, "value")
	}
	if _, err := syscall.Listxattr(f, buf); err != nil {
		t.Errorf("listxattr through the mount: %v", err)
	}
	if err := syscall.Removexattr(f, "user.test"); err != nil {
		t.Errorf("removexattr through the mount: %v", err)
	}
	if _, err := syscall.Getxattr(filepath.Join(backing, "f.txt"), "user.test", buf); err == nil {
		t.Error("the attribute survived removal on the backing file")
	}
}
