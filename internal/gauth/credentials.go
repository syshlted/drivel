// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

// Package gauth handles Google OAuth for Drivel: reading/writing the client
// secret (credentials.json), the interactive login flow (loopback redirect with a
// manual paste fallback, à la rclone), and token persistence. It is transport-
// agnostic — internal/provider/gdrive composes it with the HTTP/3 transport.
package gauth

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// Drive OAuth scopes offered by the login wizard.
const (
	ScopeDrive         = "https://www.googleapis.com/auth/drive"          // full access
	ScopeDriveReadonly = "https://www.googleapis.com/auth/drive.readonly" // read-only
	ScopeDriveFile     = "https://www.googleapis.com/auth/drive.file"     // app-created files only
)

// DefaultLoopbackPort matches rclone's well-known port, so the docker tip is
// simply: -p 127.0.0.1:53682:53682.
const DefaultLoopbackPort = 53682

const (
	authURI = "https://accounts.google.com/o/oauth2/auth"
	tokURI  = "https://oauth2.googleapis.com/token"
	certURL = "https://www.googleapis.com/oauth2/v1/certs"
)

// Credentials is a Google OAuth client (the id/secret from the Cloud console).
type Credentials struct {
	ClientID     string
	ClientSecret string
	ProjectID    string
}

// on-disk shapes: Google client secret JSON wraps the block in "installed"
// (desktop app) or "web".
type credFile struct {
	Installed *clientBlock `json:"installed,omitempty"`
	Web       *clientBlock `json:"web,omitempty"`
}

type clientBlock struct {
	ClientID                string   `json:"client_id"`
	ProjectID               string   `json:"project_id,omitempty"`
	AuthURI                 string   `json:"auth_uri"`
	TokenURI                string   `json:"token_uri"`
	AuthProviderX509CertURL string   `json:"auth_provider_x509_cert_url,omitempty"`
	ClientSecret            string   `json:"client_secret"`
	RedirectURIs            []string `json:"redirect_uris,omitempty"`
}

// LoadCredentials reads client id/secret from a Google credentials.json, accepting
// either the "installed" or "web" variant.
func LoadCredentials(path string) (Credentials, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Credentials{}, err
	}
	var cf credFile
	if err := json.Unmarshal(b, &cf); err != nil {
		return Credentials{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	blk := cf.Installed
	if blk == nil {
		blk = cf.Web
	}
	if blk == nil || blk.ClientID == "" {
		return Credentials{}, fmt.Errorf("no client_id in %s (expected an \"installed\" or \"web\" block)", path)
	}
	return Credentials{ClientID: blk.ClientID, ClientSecret: blk.ClientSecret, ProjectID: blk.ProjectID}, nil
}

// WriteCredentials writes c as an installed-app credentials.json with 0600 perms.
func WriteCredentials(path string, c Credentials) error {
	cf := credFile{Installed: &clientBlock{
		ClientID:                c.ClientID,
		ProjectID:               c.ProjectID,
		AuthURI:                 authURI,
		TokenURI:                tokURI,
		AuthProviderX509CertURL: certURL,
		ClientSecret:            c.ClientSecret,
		RedirectURIs:            []string{"http://127.0.0.1"},
	}}
	b, err := json.MarshalIndent(cf, "", "  ")
	if err != nil {
		return err
	}
	return writeSecret(path, func(w io.Writer) error {
		_, err := w.Write(b)
		return err
	})
}

// Config builds an oauth2.Config. redirectURL may be "" for plain API/refresh use.
func (c Credentials) Config(redirectURL string, scopes ...string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     c.ClientID,
		ClientSecret: c.ClientSecret,
		Endpoint:     google.Endpoint,
		RedirectURL:  redirectURL,
		Scopes:       scopes,
	}
}
