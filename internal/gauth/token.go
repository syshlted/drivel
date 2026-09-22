// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package gauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"

	"golang.org/x/oauth2"
)

// LoadToken reads a cached OAuth token from path.
func LoadToken(path string) (*oauth2.Token, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tok := &oauth2.Token{}
	if err := json.NewDecoder(f).Decode(tok); err != nil {
		return nil, fmt.Errorf("decoding token %s: %w", path, err)
	}
	return tok, nil
}

// SaveToken writes tok to path with 0600 perms, tightening them if the file was
// already there with something wider.
func SaveToken(path string, tok *oauth2.Token) error {
	return writeSecret(path, func(w io.Writer) error {
		// The whole purpose of this function is to persist the token; the 0600
		// mode writeSecret enforces is the control that matters, not withholding
		// the field.
		return json.NewEncoder(w).Encode(tok) //nolint:gosec // G117: intentional, see above
	})
}

// WhoAmI does a best-effort Drive about.get to confirm the token works and report
// the account. Works with any Drive scope (no extra profile scope needed).
func WhoAmI(ctx context.Context, c Credentials, scope string, tok *oauth2.Token) (string, error) {
	const aboutURL = "https://www.googleapis.com/drive/v3/about?fields=user(emailAddress,displayName)"
	client := c.Config("", scope).Client(ctx, tok)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, aboutURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("about.get: %s", resp.Status)
	}
	var out struct {
		User struct {
			EmailAddress string `json:"emailAddress"`
			DisplayName  string `json:"displayName"`
		} `json:"user"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.User.DisplayName != "" && out.User.EmailAddress != "" {
		return fmt.Sprintf("%s <%s>", out.User.DisplayName, out.User.EmailAddress), nil
	}
	return out.User.EmailAddress, nil
}
