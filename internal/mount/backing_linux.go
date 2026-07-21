package mount

import (
	"fmt"
	"io"
	"os"
)

// openInPlace opens a directory fd to dir BEFORE it is mounted over, and returns a
// path that resolves through that fd. Once the FUSE filesystem is mounted at dir,
// path-based access to dir hits the overlay (our own handler); operations via
// /proc/self/fd/N instead resolve to the original underlying directory, which is
// exactly what we want for the backing store. The load-bearing rule: never touch
// the backing store by the mountpoint path — only through this fd — or reads/writes
// recurse into our FUSE handler and deadlock.
//
// The returned *os.File must be held open (not GC'd) for the mount's lifetime, or
// the fd closes and /proc/self/fd/N breaks; the caller keeps it via Backing.
func openInPlace(dir string) (string, io.Closer, error) {
	f, err := os.Open(dir)
	if err != nil {
		return "", nil, fmt.Errorf("opening backing dir %q for in-place mount: %w", dir, err)
	}
	// /proc/self refers to this process; the FUSE server runs in-process, so the
	// fd stays valid as long as f is held open.
	return fmt.Sprintf("/proc/self/fd/%d", f.Fd()), f, nil
}
