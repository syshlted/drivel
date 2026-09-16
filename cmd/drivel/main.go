// Command drivel mounts a loopback FUSE filesystem that proxies operations to an
// underlying directory and syncs that directory with Google Drive.
//
// Subcommands:
//
//	drivel login  [flags]   # interactive OAuth setup (writes credentials.json + token.json)
//	drivel mount  [flags]   # mount and sync (default if no subcommand given)
//
// It is also the mount(8) helper for /etc/fstab, when invoked through the
// /sbin/mount.fuse.drivel symlink or as `drivel mount-helper` (Linux only).
//
// See DESIGN.md for the architecture.
//
// Copyright (C) 2026 SystemHalted and Jeremy Melanson. Drivel is free software
// under the GNU Affero General Public License, version 3; see the LICENSE file
// at the repository root.
package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	args := os.Args[1:]
	// Invoked through /sbin/mount.fuse.drivel (or mount.drivel): mount(8) chose
	// this program by name, and passes an argument shape that has nothing to do
	// with drivel's flags. One binary rather than a second one to keep in version
	// lockstep — the name it was reached by is the whole difference.
	if isMountHelper(os.Args[0]) {
		failHelper(runMountHelper(args))
		return
	}

	cmd := ""
	// Accept a leading subcommand; anything starting with '-' means the default
	// (mount) command with flags, preserving `drivel -mount ... -data ...`.
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	// The subcommands return their errors instead of calling log.Fatal, so the
	// defers they set up — unmounting, closing the state DBs, draining the sync
	// engine — actually run before the process exits.
	switch cmd {
	case "", "mount":
		fail(runMount(args))
	case "login":
		fail(runLogin(args))
	case "mount-helper":
		// The same entry point, reachable without the symlink: it is how the helper
		// is tested, and how you debug an fstab line by hand.
		failHelper(runMountHelper(args))
	case "completion":
		fail(runCompletion(args))
	case "help", "-h", "--help":
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage(os.Stderr)
		os.Exit(2)
	}
}

// failHelper reports a mount-helper failure the way mount(8) expects: the
// program name, the reason, and a non-zero exit. Not log.Fatal, because a
// timestamped line is noise in the middle of `mount`'s own output, and this text
// is what an admin sees when a boot mount does not come up.
func failHelper(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", filepath.Base(os.Args[0]), err)
		os.Exit(1)
	}
}

// fail exits non-zero after a subcommand returns an error. It is the only
// place that terminates the process on a failure, so no defer is ever skipped.
func fail(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func usage(w *os.File) {
	fmt.Fprint(w, `Drivel - a Drive-backed FUSE interceptor filesystem
Copyright (C) 2026 SystemHalted and Jeremy Melanson
License AGPLv3: GNU Affero GPL v3 <https://www.gnu.org/licenses/agpl-3.0.html>
This is free software with NO WARRANTY, to the extent permitted by law.

Usage:
  drivel login [flags]     Interactive Google OAuth setup (credentials.json + token.json)
  drivel mount [flags]     Mount a directory and sync it with Google Drive
  drivel completion SHELL  Print the completion script for bash or zsh

Run 'drivel login -h' or 'drivel mount -h' for command flags.

To try completion in this shell:  eval "$(drivel completion bash)"

As /sbin/mount.fuse.drivel (or 'drivel mount-helper') this is also the mount(8)
helper for /etc/fstab; see drivel(1) and docs/user/fstab.md. Linux only.
`)
}
