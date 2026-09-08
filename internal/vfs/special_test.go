package vfs

import (
	"bytes"
	"errors"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/zishmusic/drivel/internal/fsevent"
	"github.com/zishmusic/drivel/internal/mount"
)

// logSink collects a mount's log lines. The FUSE server writes them from its own
// goroutines while the test reads, so the buffer needs the lock — without it this
// file is a -race failure rather than a test.
type logSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *logSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// withLog captures the mount's own log output, which for these tests is half the
// behaviour under test: the point of M15 items 2 and 3 is that drivel says what it
// is declining to do.
func withLog(s *logSink) func(*mount.Options) {
	return func(o *mount.Options) { o.Logger = log.New(s, "", 0) }
}

// waitLog polls until the mount has logged a line containing want. The log is
// written from the serving goroutine after the syscall has already returned, so
// there is no ordering to rely on — only a deadline.
func waitLog(t *testing.T, s *logSink, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(s.String(), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("mount never logged a line containing %q; log was:\n%s", want, s.String())
}

// noEvent fails if anything reaches the sync engine within a short window. The
// window is a compromise and it is the right way round: a spurious event shows up
// as a failure here, while a missed one would show up as a failure in the tests
// that wait for events.
func noEvent(t *testing.T, events <-chan fsevent.Event) {
	t.Helper()
	select {
	case ev := <-events:
		t.Fatalf("unexpected sync event %s %s: this file has no remote representation and must not be pushed", ev.Op, ev.Path)
	case <-time.After(300 * time.Millisecond):
	}
}

// A hard link is refused with EPERM, and nothing is created.
//
// The failure this replaces is not an error at all, which is what made it
// dangerous: Link used to fall through to the loopback, so the link was made, the
// caller was told it succeeded, and both names — being ordinary regular files —
// were then pushed by the sweep as two independent remote objects that diverge
// from the first write onwards.
func TestHardLinkIsRefused(t *testing.T) {
	backing := t.TempDir()
	if err := os.WriteFile(filepath.Join(backing, "a.txt"), []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	var sink logSink
	mnt, events := mountTest(t, backing, nil, withLog(&sink))
	drain(events)

	err := os.Link(filepath.Join(mnt, "a.txt"), filepath.Join(mnt, "b.txt"))
	if !errors.Is(err, syscall.EPERM) {
		t.Fatalf("os.Link = %v; want EPERM, which link(2) documents for a filesystem that cannot make hard links", err)
	}
	// The refusal has to be complete: an EPERM with a file left behind in the
	// backing store is the same divergence with a confusing error attached.
	if _, err := os.Lstat(filepath.Join(backing, "b.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("backing store holds b.txt after a refused link (lstat err = %v)", err)
	}
	noEvent(t, events)
	waitLog(t, &sink, "refused a hard link at b.txt")
}

// A symlink is created and stays local: no event, so nothing is ever pushed, and
// a log line saying so.
//
// Not syncing it is the pre-M15 behaviour and is kept deliberately — DESIGN.md §9
// M15 item 5 has the argument for why representing a symlink takes two signals and
// why deciding it from content would break enumeration. What is new is the line.
func TestSymlinkStaysLocalAndSaysSo(t *testing.T) {
	backing := t.TempDir()
	var sink logSink
	mnt, events := mountTest(t, backing, nil, withLog(&sink))
	drain(events)

	if err := os.Symlink("target.txt", filepath.Join(mnt, "link.txt")); err != nil {
		t.Fatalf("creating a symlink through the mount: %v", err)
	}
	fi, err := os.Lstat(filepath.Join(backing, "link.txt"))
	if err != nil {
		t.Fatalf("the symlink did not reach the backing store: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("backing entry is %v; want a symlink", fi.Mode())
	}
	noEvent(t, events)
	waitLog(t, &sink, "skip link.txt (symbolic link: no remote representation, stays local)")
}

// mknodBroken reports a platform on which a special file cannot be created
// through a go-fuse mount at all, so the M15 behaviour has nothing to describe.
//
// FreeBSD is the one, for two separate reasons found by running this file on it.
// Its fusefs sends rdev = ~0 on the MKNOD for a fifo, and its mknod(2) accepts
// S_IFIFO only when dev == 0, so go-fuse's loopback — which passes rdev through
// verbatim — turns every fifo creation through any FUSE mount into EINVAL before
// drivel's override has anything to decide. Its mknod(2) also refuses S_IFREG
// outright, on an ordinary ZFS directory as much as through a mount.
//
// Neither is drivel's to fix in M15 and neither risks data: the caller gets a
// loud EINVAL and no file. Rather than skip, the tests below assert that is what
// happens, so the platform's behaviour is pinned and a change to it is a failure
// rather than a surprise. It is a GOOS check and not a build tag deliberately:
// the tests must keep running on macOS, where nobody has yet found out which of
// these two shapes applies.
const mknodBroken = runtime.GOOS == "freebsd"

// A fifo is created and stays local, with the same line. Sockets and device nodes
// travel the same path; a fifo is the one an unprivileged test can make.
func TestSpecialFileStaysLocalAndSaysSo(t *testing.T) {
	backing := t.TempDir()
	var sink logSink
	mnt, events := mountTest(t, backing, nil, withLog(&sink))
	drain(events)

	err := unix.Mkfifo(filepath.Join(mnt, "pipe"), 0o644)
	if mknodBroken {
		// The refusal has to be clean: an EINVAL that still left something behind
		// would be a half-created file the sweep would then have to reason about.
		if !errors.Is(err, syscall.EINVAL) {
			t.Fatalf("mkfifo through the mount = %v; want EINVAL (see mknodBroken)", err)
		}
		if _, err := os.Lstat(filepath.Join(backing, "pipe")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("backing store holds pipe after a refused mkfifo (lstat err = %v)", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("creating a fifo through the mount: %v", err)
	}
	fi, err := os.Lstat(filepath.Join(backing, "pipe"))
	if err != nil {
		t.Fatalf("the fifo did not reach the backing store: %v", err)
	}
	if fi.Mode()&os.ModeNamedPipe == 0 {
		t.Errorf("backing entry is %v; want a named pipe", fi.Mode())
	}
	noEvent(t, events)
	waitLog(t, &sink, "skip pipe (named pipe: no remote representation, stays local)")
}

// mknod(2) with no type bits creates a REGULAR file, and a regular file must sync
// however it was made.
//
// This is the one case in the group that emits. Without it the mount and the sweep
// would disagree about the same file — the sweep's local walk pushes whatever is
// regular — so whether the file synced would depend on which syscall created it
// and on whether drivel happened to be watching. That is the MC-12 shape, and it
// is a bug wherever it appears.
func TestMknodRegularFileIsSynced(t *testing.T) {
	backing := t.TempDir()
	mnt, events := mountTest(t, backing, nil)
	drain(events)

	if mknodBroken {
		// Prove it is the platform and not the mount: the same call fails on an
		// ordinary directory with no FUSE anywhere near it.
		if err := unix.Mknod(filepath.Join(backing, "direct.txt"), syscall.S_IFREG|0o644, 0); !errors.Is(err, syscall.EINVAL) {
			t.Fatalf("mknod(S_IFREG) on the backing store = %v; want EINVAL, which is why this test cannot run here", err)
		}
		return
	}
	if err := unix.Mknod(filepath.Join(mnt, "made.txt"), syscall.S_IFREG|0o644, 0); err != nil {
		t.Fatalf("mknod of a regular file through the mount: %v", err)
	}
	ev := waitEvent(t, events, fsevent.OpCreate, "made.txt")
	if ev.Path != "made.txt" {
		t.Errorf("event path = %q; want made.txt", ev.Path)
	}
}

// The two sites that report the skip must use the same words, or a user grepping
// their log for why a file is missing finds it in one place and not the other.
// The engine keeps its own copy (syncengine.kindOf) because it must not import
// the mount backend; this is the test that says the copies still agree.
func TestSpecialKindNamesEveryType(t *testing.T) {
	for _, tc := range []struct {
		mode uint32
		want string
	}{
		{syscall.S_IFIFO, "named pipe"},
		{syscall.S_IFSOCK, "socket"},
		{syscall.S_IFCHR, "character device"},
		{syscall.S_IFBLK, "block device"},
		{syscall.S_IFLNK, "symbolic link"},
		{syscall.S_IFDIR, "directory"},
		{syscall.S_IFREG, "regular file"},
	} {
		if got := specialKind(tc.mode | 0o644); got != tc.want {
			t.Errorf("specialKind(%#o) = %q; want %q", tc.mode, got, tc.want)
		}
	}
}
