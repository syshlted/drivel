// Command drivel mounts a loopback FUSE filesystem that proxies operations to an
// underlying directory and syncs that directory with Google Drive.
//
// Subcommands:
//
//	drivel login  [flags]   # interactive OAuth setup (writes credentials.json + token.json)
//	drivel mount  [flags]   # mount and sync (default if no subcommand given)
//
// See DESIGN.md for the architecture.
package main

import (
	"fmt"
	"log"
	"os"
	"strings"
)

func main() {
	args := os.Args[1:]
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
	case "help", "-h", "--help":
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage(os.Stderr)
		os.Exit(2)
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

Usage:
  drivel login [flags]   Interactive Google OAuth setup (credentials.json + token.json)
  drivel mount [flags]   Mount a directory and sync it with Google Drive

Run 'drivel login -h' or 'drivel mount -h' for command flags.
`)
}
