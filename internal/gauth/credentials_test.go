package gauth

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The file I/O around the login wizard is what fails on a bad install: a
// credentials.json that was truncated, hand-edited, downloaded as the wrong kind
// of client, or left world-readable. Every case here is one of those.

func writeFile(t *testing.T, dir, name, body string, mode fs.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil { // defeat the umask, so the test means what it says
		t.Fatal(err)
	}
	return path
}

// Every way a credentials.json can fail to identify a client must be an error at
// load time. The alternative is a Credentials{} that looks usable and produces an
// OAuth failure much later, with nothing pointing back at the file.
func TestLoadCredentialsRejectsWhatCannotAuthenticate(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name, body string
	}{
		{"empty file", ""},
		{"not json", "this is not json at all"},
		{"truncated json", `{"installed":{"client_id":"abc"`},
		{"json but not an object", `["installed"]`},
		{"no client block", `{"project_id":"proj"}`},
		{"neither installed nor web", `{"service_account":{"client_id":"abc"}}`},
		{"installed block with no id", `{"installed":{"client_secret":"s"}}`},
		{"empty client id", `{"installed":{"client_id":"","client_secret":"s"}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeFile(t, dir, strings.ReplaceAll(tt.name, " ", "_")+".json", tt.body, 0o600)
			got, err := LoadCredentials(path)
			if err == nil {
				t.Fatalf("LoadCredentials accepted %s and returned %+v", tt.name, got)
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("error does not name the file that is wrong: %v", err)
			}
		})
	}
}

// A missing file must stay recognisable as a missing file: gdrive wraps this
// error into "run 'drivel login' first", and it can only tell the difference
// between "not set up yet" and "set up wrong" if the sentinel survives.
func TestLoadCredentialsOnAMissingFileIsNotExist(t *testing.T) {
	_, err := LoadCredentials(filepath.Join(t.TempDir(), "absent.json"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v; want it to wrap fs.ErrNotExist", err)
	}
}

// The Cloud console hands out both shapes, and some projects have both blocks in
// one file. Desktop ("installed") is the one drivel's loopback flow is
// registered as, so it wins.
func TestLoadCredentialsPrefersTheInstalledBlock(t *testing.T) {
	path := writeFile(t, t.TempDir(), "both.json",
		`{"installed":{"client_id":"desktop","client_secret":"d"},"web":{"client_id":"webapp","client_secret":"w"}}`, 0o600)
	got, err := LoadCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.ClientID != "desktop" {
		t.Fatalf("ClientID = %q; want the installed block's id", got.ClientID)
	}
}

// Google adds fields to these files over time, and drivel reads three of them.
// An unknown key is not a reason to refuse to log in.
func TestLoadCredentialsIgnoresUnknownFields(t *testing.T) {
	path := writeFile(t, t.TempDir(), "future.json", `{
	  "installed": {
	    "client_id": "id-1", "client_secret": "sec-1", "project_id": "proj-1",
	    "auth_uri": "https://accounts.google.com/o/oauth2/auth",
	    "some_field_from_2027": {"nested": true}
	  },
	  "top_level_novelty": 42
	}`, 0o600)

	got, err := LoadCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	want := Credentials{ClientID: "id-1", ClientSecret: "sec-1", ProjectID: "proj-1"}
	if got != want {
		t.Fatalf("got %+v; want %+v", got, want)
	}
}

// The written file has to be the shape Google's own libraries read back, since
// the user may well point rclone or gcloud at it too.
func TestWriteCredentialsProducesAnInstalledAppFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := WriteCredentials(path, Credentials{ClientID: "id-1", ClientSecret: "sec-1", ProjectID: "proj-1"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var raw struct {
		Installed *struct {
			ClientID     string   `json:"client_id"`
			ProjectID    string   `json:"project_id"`
			AuthURI      string   `json:"auth_uri"`
			TokenURI     string   `json:"token_uri"`
			CertURL      string   `json:"auth_provider_x509_cert_url"`
			ClientSecret string   `json:"client_secret"`
			RedirectURIs []string `json:"redirect_uris"`
		} `json:"installed"`
		Web json.RawMessage `json:"web"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("the file we wrote does not parse: %v\n%s", err, b)
	}
	if raw.Installed == nil {
		t.Fatalf("no installed block:\n%s", b)
	}
	if raw.Web != nil {
		t.Errorf("a web block was written too; the file must describe one client\n%s", b)
	}
	if raw.Installed.AuthURI != authURI || raw.Installed.TokenURI != tokURI || raw.Installed.CertURL != certURL {
		t.Errorf("endpoints are not Google's:\n%s", b)
	}
	if len(raw.Installed.RedirectURIs) == 0 || !strings.HasPrefix(raw.Installed.RedirectURIs[0], "http://127.0.0.1") {
		t.Errorf("redirect_uris does not describe the loopback flow: %v", raw.Installed.RedirectURIs)
	}
}

// Rewriting has to leave the file as one JSON document. The tail of a longer
// previous version left behind would make the file unparseable, and the user's
// only clue would be that login stopped working.
func TestWriteCredentialsTruncates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	long := Credentials{ClientID: strings.Repeat("x", 4096), ClientSecret: strings.Repeat("y", 4096)}
	if err := WriteCredentials(path, long); err != nil {
		t.Fatal(err)
	}
	short := Credentials{ClientID: "id-2", ClientSecret: "sec-2"}
	if err := WriteCredentials(path, short); err != nil {
		t.Fatal(err)
	}

	got, err := LoadCredentials(path)
	if err != nil {
		t.Fatalf("re-reading the rewritten file: %v", err)
	}
	if got != short {
		t.Fatalf("got %+v; want %+v", got, short)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > 1024 {
		t.Errorf("the rewritten file is %d bytes; the previous version was not truncated away", len(b))
	}
}

// A client secret must not be readable by other users on the machine — including
// when the file was already there with a wider mode, which is the case
// os.WriteFile silently does not handle.
func TestWriteCredentialsIsAlwaysPrivate(t *testing.T) {
	dir := t.TempDir()

	fresh := filepath.Join(dir, "fresh.json")
	if err := WriteCredentials(fresh, Credentials{ClientID: "id"}); err != nil {
		t.Fatal(err)
	}
	assertMode600(t, fresh)

	wide := writeFile(t, dir, "wide.json", `{"installed":{"client_id":"old"}}`, 0o644)
	if err := WriteCredentials(wide, Credentials{ClientID: "id"}); err != nil {
		t.Fatal(err)
	}
	assertMode600(t, wide)
}

func TestWriteCredentialsReportsAnUnwritableDestination(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-dir", "credentials.json")
	if err := WriteCredentials(path, Credentials{ClientID: "id"}); err == nil {
		t.Fatal("writing into a directory that does not exist reported success")
	}
}

func assertMode600(t *testing.T, path string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("%s is mode %v; a secret must be 0600 whether or not the file already existed", path, got)
	}
}

// Config carries what the flow depends on: the scopes asked for, the redirect
// the loopback server is listening on, and Google's endpoints.
func TestConfigCarriesScopesAndRedirect(t *testing.T) {
	c := Credentials{ClientID: "id-1", ClientSecret: "sec-1"}
	cfg := c.Config("http://127.0.0.1:53682/", ScopeDriveReadonly)

	if cfg.ClientID != c.ClientID || cfg.ClientSecret != c.ClientSecret {
		t.Errorf("client identity not carried: %+v", cfg)
	}
	if cfg.RedirectURL != "http://127.0.0.1:53682/" {
		t.Errorf("RedirectURL = %q", cfg.RedirectURL)
	}
	if len(cfg.Scopes) != 1 || cfg.Scopes[0] != ScopeDriveReadonly {
		t.Errorf("Scopes = %v; want just the read-only scope", cfg.Scopes)
	}
	if !strings.HasPrefix(cfg.Endpoint.AuthURL, "https://accounts.google.com/") {
		t.Errorf("AuthURL = %q; want Google's", cfg.Endpoint.AuthURL)
	}

	// A refresh needs no redirect, and passing one would be a lie about where
	// the token came from.
	if got := c.Config("").RedirectURL; got != "" {
		t.Errorf(`Config("") set RedirectURL = %q`, got)
	}
}
