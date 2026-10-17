// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package control

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// SocketName is the control socket's file name inside its directory.
const SocketName = "control.sock"

// DefaultSocketPath is $XDG_RUNTIME_DIR/drivel/control.sock.
//
// There is no fallback when XDG_RUNTIME_DIR is unset, and that is the safe
// direction rather than an omission: the obvious one is a directory under /tmp,
// which anybody can pre-create — and the socket hands out every synced path and
// can stop the process. The caller names a path with -control-socket instead.
func DefaultSocketPath() (string, error) {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		return "", errors.New("XDG_RUNTIME_DIR is not set, so there is no private directory to put the control socket in; pass -control-socket PATH")
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("XDG_RUNTIME_DIR is %q, which is not an absolute path; pass -control-socket PATH", dir)
	}
	return filepath.Join(dir, "drivel", SocketName), nil
}

// Listen binds the control socket at path.
//
// The directory is created 0700 and then checked rather than trusted: it must be
// a real directory, owned by this user, and closed to everyone else, because the
// directory's mode is what authenticates a connection on platforms where a
// socket's own mode bits are advisory. The socket is then 0600.
//
// A socket left behind by a crash is connected to FIRST and removed only when
// the connection is refused. Removing it blindly would steal a running daemon's
// socket: the old process would go on serving a path nobody can reach, and the
// new one would answer for mounts it does not have.
func Listen(path string) (net.Listener, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("control socket %q: not an absolute path", path)
	}
	// Checked here because the kernel's own answer is "bind: invalid argument",
	// which names neither the path nor the limit — found by the first real run,
	// whose runtime directory was a deep scratch path.
	if len(path) >= maxSocketPath {
		return nil, fmt.Errorf("control socket %s: the path is %d bytes, and a unix socket's path must be under %d on this platform; pass a shorter -control-socket",
			path, len(path), maxSocketPath)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("control socket directory: %w", err)
	}
	if err := checkPrivateDir(dir); err != nil {
		return nil, err
	}
	if err := clearStale(path); err != nil {
		return nil, err
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "unix", path)
	if err != nil {
		return nil, fmt.Errorf("control socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("control socket: %w", err)
	}
	return ln, nil
}

// clearStale removes a dead socket at path and refuses a live one.
func clearStale(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("control socket: %w", err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("control socket: %s exists and is not a socket; refusing to replace it", path)
	}
	d := net.Dialer{Timeout: time.Second}
	c, err := d.DialContext(context.Background(), "unix", path)
	if err == nil {
		_ = c.Close()
		return fmt.Errorf("control socket: another drivel is already serving %s (stop it, or give this one a different -control-socket)", path)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("control socket: %s exists and could not be checked (%w); refusing to remove it", path, err)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("control socket: removing the stale %s: %w", path, err)
	}
	return nil
}
