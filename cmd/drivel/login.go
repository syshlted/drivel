// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/syshlted/drivel/internal/config"
	"github.com/syshlted/drivel/internal/gauth"
)

// loginCLI is everything `drivel login` accepts.
type loginCLI struct {
	account      string
	configPath   string
	credPath     string
	tokenPath    string
	clientID     string
	clientSecret string
	projectID    string
	scopeName    string
	port         int
	openBrowser  bool
}

// loginFlagSet defines the login command's flags, binding them into c. Separate
// from runLogin for the reason mountFlagSet is separate from runMount: the
// completion generator walks this set, so there is one description of these
// flags rather than one in the program and one in a shell script.
func loginFlagSet(c *loginCLI) *flag.FlagSet {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	fs.StringVar(&c.account, "account", "", "name this login as an account: store its files under $XDG_CONFIG_HOME/drivel/NAME and add [account.NAME] to the config file")
	fs.StringVar(&c.configPath, "config", "", "config file to add the account to (default: $XDG_CONFIG_HOME/drivel/config.toml)")
	fs.StringVar(&c.credPath, "credentials", "credentials.json", "path to read/write the OAuth client secret JSON")
	fs.StringVar(&c.tokenPath, "token", "token.json", "path to write the OAuth token")
	fs.StringVar(&c.clientID, "client-id", "", "OAuth client ID (else read from -credentials or prompted)")
	fs.StringVar(&c.clientSecret, "client-secret", "", "OAuth client secret (else read from -credentials or prompted)")
	fs.StringVar(&c.projectID, "project-id", "", "GCP project ID (optional)")
	fs.StringVar(&c.scopeName, "scope", "", "Drive scope: drive | drive.readonly | drive.file (else prompted)")
	fs.IntVar(&c.port, "port", gauth.DefaultLoopbackPort, "loopback port for the OAuth redirect (0 = auto)")
	fs.BoolVar(&c.openBrowser, "open", false, "attempt to open the auth URL with the OS browser handler")
	return fs
}

// runLogin is an rclone-style interactive OAuth wizard: it collects the client
// id/secret (from flags, an existing credentials.json, or prompts), writes
// credentials.json, runs the loopback/paste login flow, and caches the token.
func runLogin(args []string) error {
	var c loginCLI
	fs := loginFlagSet(&c)
	_ = fs.Parse(args)

	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })

	// An account keeps its secrets in a directory of its own. One shared
	// token.json is not a degraded multi-account experience — it is each login
	// silently overwriting the previous one's credentials.
	if c.account != "" {
		if err := config.ValidName(c.account); err != nil {
			return fmt.Errorf("-account: %w", err)
		}
		dir, err := config.AccountDir(c.account)
		if err != nil {
			return err
		}
		// 0700: this directory holds a client secret and a refresh token.
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
		if !given["credentials"] {
			c.credPath = filepath.Join(dir, "credentials.json")
		}
		if !given["token"] {
			c.tokenPath = filepath.Join(dir, "token.json")
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	in := bufio.NewReader(os.Stdin)

	creds, err := resolveCredentials(in, c.credPath, c.clientID, c.clientSecret, c.projectID)
	if err != nil {
		return err
	}
	if err := gauth.WriteCredentials(c.credPath, creds); err != nil {
		return fmt.Errorf("writing %s: %w", c.credPath, err)
	}
	fmt.Printf("Wrote client credentials to %s\n", c.credPath)

	scope := resolveScope(in, c.scopeName)
	rootFolder := prompt(in, "\nRoot folder ID to sync (leave blank for My Drive root): ")

	tok, err := gauth.Login(ctx, creds, []string{scope}, gauth.LoginOptions{
		Port:        c.port,
		OpenBrowser: c.openBrowser,
		// Share the one stdin reader: a separate reader here would race the
		// prompt reader above, which may have buffered the pasted line already.
		In: in,
	})
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}
	if err := gauth.SaveToken(c.tokenPath, tok); err != nil {
		return fmt.Errorf("saving token to %s: %w", c.tokenPath, err)
	}
	fmt.Printf("\nSaved token to %s\n", c.tokenPath)

	if who, err := gauth.WhoAmI(ctx, creds, scope, tok); err == nil && who != "" {
		fmt.Printf("Authenticated as %s\n", who)
	} else if err != nil {
		fmt.Printf("(token saved, but identity check failed: %v)\n", err)
	}

	driveRoot := rootFolder
	if driveRoot == "" {
		driveRoot = "root"
	}
	if c.account == "" {
		fmt.Printf("\nDone. Mount with:\n\n  drivel mount -mount ./mnt -data ./data \\\n    -credentials %s -token %s -drive-root %s\n\n",
			c.credPath, c.tokenPath, driveRoot)
		return nil
	}
	return recordAccount(c.configPath, c.account, c.credPath, c.tokenPath, scope, driveRoot)
}

// recordAccount adds the account to the config file and shows the mount block to
// go with it.
//
// It appends and never rewrites: the config file is hand-edited and commented,
// and a round trip through a TOML encoder would drop every comment in it. An
// account that already exists is therefore printed rather than replaced — the
// credentials on disk have been refreshed either way, which is the part that
// actually needed doing.
func recordAccount(configPath, name, credPath, tokenPath, scope, driveRoot string) error {
	if configPath == "" {
		p, err := config.DefaultPath()
		if err != nil {
			return err
		}
		configPath = p
	}
	settings := []config.Setting{
		{Key: "provider", Value: driveKind},
		{Key: "credentials", Value: absOr(credPath)},
		{Key: "token", Value: absOr(tokenPath)},
		{Key: "scope", Value: scope},
	}

	err := config.AppendAccount(configPath, name, settings)
	switch {
	case errors.Is(err, config.ErrAccountExists):
		fmt.Printf("\n%s already defines [account.%s]; the credentials on disk are updated either way.\nIf anything below differs, change it by hand:\n\n%s\n",
			configPath, name, indent(config.AccountBlock(name, settings)))
	case err != nil:
		return err
	default:
		fmt.Printf("\nAdded [account.%s] to %s\n", name, configPath)
	}

	mount := fmt.Sprintf("[[mount]]\naccount = %q\npath    = \"~/drive-%s\"\ndata    = \"~/.cache/drivel/%s\"\n\n[mount.provider]\nroot = %q\n",
		name, name, name, driveRoot)
	fmt.Printf("\nAdd a mount for it:\n\n%s\nThen run: drivel mount\n\n", indent(mount))
	return nil
}

// absOr makes a path absolute, since the config file resolves relative paths
// against its own directory rather than the one login happened to run in.
func absOr(p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return a
}

// indent shifts a block two spaces right so it reads as output rather than as
// something already in the file.
func indent(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		if line == "" {
			b.WriteString("\n")
			continue
		}
		b.WriteString("  " + line + "\n")
	}
	return b.String()
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
// loginScopeNames are the scope names -scope accepts, in menu order. Named here
// so the shell completions offer exactly what resolveScope resolves rather than
// a copy of it, and so the refusal below can list them rather than leaving
// someone who typed one wrong to guess. TestLoginScopeNamesAllResolve is what
// keeps the list and the switch honest.
var loginScopeNames = []string{"drive", "drive.readonly", "drive.file"}

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
			fmt.Printf("unrecognised scope %q (want %s)\n", name, strings.Join(loginScopeNames, ", "))
		}
		fmt.Print(`
Choose a scope:
  1 / Full access to all files (recommended for a sync mount)   "drive"
  2 / Read-only access to file metadata and contents            "drive.readonly"
  3 / Access to files created by Drivel only                   "drive.file"
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
