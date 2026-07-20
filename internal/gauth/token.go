package gauth

import (
	"context"
	"encoding/json"
	"fmt"
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

// SaveToken writes tok to path with 0600 perms.
func SaveToken(path string, tok *oauth2.Token) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(tok)
}

// WhoAmI does a best-effort Drive about.get to confirm the token works and report
// the account. Works with any Drive scope (no extra profile scope needed).
func WhoAmI(ctx context.Context, c Credentials, scope string, tok *oauth2.Token) (string, error) {
	client := c.Config("", scope).Client(ctx, tok)
	resp, err := client.Get("https://www.googleapis.com/drive/v3/about?fields=user(emailAddress,displayName)")
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
