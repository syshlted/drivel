// Command dedupfs mounts a loopback FUSE filesystem that proxies operations to an
// underlying directory and syncs that directory with Google Drive.
//
// Subcommands:
//
//	dedupfs login  [flags]   # interactive OAuth setup (writes credentials.json + token.json)
//	dedupfs mount  [flags]   # mount and sync (default if no subcommand given)
//
// See DESIGN.md for the architecture.
package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	args := os.Args[1:]
	cmd := ""
	// Accept a leading subcommand; anything starting with '-' means the default
	// (mount) command with flags, preserving `dedupfs -mount ... -data ...`.
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "", "mount":
		runMount(args)
	case "login":
		runLogin(args)
	case "help", "-h", "--help":
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage(os.Stderr)
		os.Exit(2)
	}
}

func usage(w *os.File) {
	fmt.Fprint(w, `dedupfs - a Drive-backed FUSE interceptor filesystem

Usage:
  dedupfs login [flags]   Interactive Google OAuth setup (credentials.json + token.json)
  dedupfs mount [flags]   Mount a directory and sync it with Google Drive

Run 'dedupfs login -h' or 'dedupfs mount -h' for command flags.
`)
}
