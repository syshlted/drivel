package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/zishmusic/dedupfs/internal/gauth"
)

// runLogin is an rclone-style interactive OAuth wizard: it collects the client
// id/secret (from flags, an existing credentials.json, or prompts), writes
// credentials.json, runs the loopback/paste login flow, and caches the token.
func runLogin(args []string) {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	credPath := fs.String("credentials", "credentials.json", "path to read/write the OAuth client secret JSON")
	tokenPath := fs.String("token", "token.json", "path to write the OAuth token")
	clientID := fs.String("client-id", "", "OAuth client ID (else read from -credentials or prompted)")
	clientSecret := fs.String("client-secret", "", "OAuth client secret (else read from -credentials or prompted)")
	projectID := fs.String("project-id", "", "GCP project ID (optional)")
	scopeName := fs.String("scope", "", "Drive scope: drive | drive.readonly | drive.file (else prompted)")
	port := fs.Int("port", gauth.DefaultLoopbackPort, "loopback port for the OAuth redirect (0 = auto)")
	openBrowser := fs.Bool("open", false, "attempt to open the auth URL with the OS browser handler")
	_ = fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	in := bufio.NewReader(os.Stdin)

	creds, err := resolveCredentials(in, *credPath, *clientID, *clientSecret, *projectID)
	if err != nil {
		log.Fatal(err)
	}
	if err := gauth.WriteCredentials(*credPath, creds); err != nil {
		log.Fatalf("writing %s: %v", *credPath, err)
	}
	fmt.Printf("Wrote client credentials to %s\n", *credPath)

	scope := resolveScope(in, *scopeName)
	rootFolder := prompt(in, "\nRoot folder ID to sync (leave blank for My Drive root): ")

	tok, err := gauth.Login(ctx, creds, []string{scope}, gauth.LoginOptions{
		Port:        *port,
		OpenBrowser: *openBrowser,
		// Share the one stdin reader: a separate reader here would race the
		// prompt reader above, which may have buffered the pasted line already.
		In: in,
	})
	if err != nil {
		log.Fatalf("login: %v", err)
	}
	if err := gauth.SaveToken(*tokenPath, tok); err != nil {
		log.Fatalf("saving token to %s: %v", *tokenPath, err)
	}
	fmt.Printf("\nSaved token to %s\n", *tokenPath)

	if who, err := gauth.WhoAmI(ctx, creds, scope, tok); err == nil && who != "" {
		fmt.Printf("Authenticated as %s\n", who)
	} else if err != nil {
		fmt.Printf("(token saved, but identity check failed: %v)\n", err)
	}

	driveRoot := rootFolder
	if driveRoot == "" {
		driveRoot = "root"
	}
	fmt.Printf("\nDone. Mount with:\n\n  dedupfs mount -mount ./mnt -data ./data \\\n    -credentials %s -token %s -drive-root %s\n\n",
		*credPath, *tokenPath, driveRoot)
}

// resolveCredentials fills the client id/secret from flags, then an existing
// credentials file, then interactive prompts.
func resolveCredentials(in *bufio.Reader, path, id, secret, project string) (gauth.Credentials, error) {
	c := gauth.Credentials{ClientID: id, ClientSecret: secret, ProjectID: project}

	if existing, err := gauth.LoadCredentials(path); err == nil {
		if c.ClientID == "" {
			c.ClientID = existing.ClientID
		}
		if c.ClientSecret == "" {
			c.ClientSecret = existing.ClientSecret
		}
		if c.ProjectID == "" {
			c.ProjectID = existing.ProjectID
		}
		fmt.Printf("Found existing %s (client %s); press Enter to keep prompted values.\n", path, short(c.ClientID))
	}

	if c.ClientID == "" {
		c.ClientID = prompt(in, "Google OAuth client ID: ")
	}
	if c.ClientSecret == "" {
		c.ClientSecret = prompt(in, "Google OAuth client secret: ")
	}
	if c.ClientID == "" || c.ClientSecret == "" {
		return c, fmt.Errorf("client ID and secret are both required")
	}
	return c, nil
}

// resolveScope maps a scope name (flag or prompted menu) to a scope URL.
func resolveScope(in *bufio.Reader, name string) string {
	for {
		switch strings.TrimSpace(name) {
		case "drive", "1":
			return gauth.ScopeDrive
		case "drive.readonly", "2":
			return gauth.ScopeDriveReadonly
		case "drive.file", "3":
			return gauth.ScopeDriveFile
		case "":
			// no value yet — show the menu and prompt below
		default:
			fmt.Printf("unrecognised scope %q\n", name)
		}
		fmt.Print(`
Choose a scope:
  1 / Full access to all files (recommended for a sync mount)   "drive"
  2 / Read-only access to file metadata and contents            "drive.readonly"
  3 / Access to files created by dedupfs only                   "drive.file"
scope [1]> `)
		name = prompt(in, "")
		if name == "" {
			return gauth.ScopeDrive
		}
	}
}

func prompt(in *bufio.Reader, label string) string {
	if label != "" {
		fmt.Print(label)
	}
	line, _ := in.ReadString('\n')
	return strings.TrimSpace(line)
}

func short(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12] + "…"
}
