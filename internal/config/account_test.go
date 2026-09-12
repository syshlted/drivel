package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppendAccountRoundTrips(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	path := filepath.Join(dir, AppName, "config.toml")

	settings := []Setting{
		{Key: "provider", Value: "gdrive"},
		{Key: "credentials", Value: "/abs/personal/credentials.json"},
		{Key: "token", Value: "/abs/personal/token.json"},
	}
	if err := AppendAccount(path, "personal", settings); err != nil {
		t.Fatalf("AppendAccount: %v", err)
	}

	// The file login leaves behind has no mounts yet, so Load rejects it — but
	// HasAccount must still work, since that is what the next login consults.
	ok, err := HasAccount(path, "personal")
	if err != nil || !ok {
		t.Fatalf("HasAccount = %v, %v; want true", ok, err)
	}
	if ok, _ := HasAccount(path, "other"); ok {
		t.Error("HasAccount invented an account")
	}

	// Add a mount by hand, the way the printed block tells the user to, and the
	// whole thing has to resolve.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\n[[mount]]\naccount = \"personal\"\npath = \"./mnt\"\n"); err != nil {
		t.Fatal(err)
	}
	f.Close() //nolint:errcheck // test scaffolding

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load after append: %v", err)
	}
	specs, err := c.Specs()
	if err != nil {
		t.Fatalf("Specs: %v", err)
	}
	if specs[0].Provider != "gdrive" {
		t.Errorf("provider = %q", specs[0].Provider)
	}
	var got struct {
		Credentials string `toml:"credentials"`
		Token       string `toml:"token"`
	}
	if err := specs[0].ProviderConfig.Decode(&got); err != nil {
		t.Fatalf("decoding provider config: %v", err)
	}
	if got.Credentials != "/abs/personal/credentials.json" {
		t.Errorf("credentials = %q", got.Credentials)
	}
}

func TestAppendAccountRefusesToReplace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	s := []Setting{{Key: "provider", Value: "gdrive"}}
	if err := AppendAccount(path, "a", s); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	err = AppendAccount(path, "a", []Setting{{Key: "provider", Value: "other"}})
	if !errors.Is(err, ErrAccountExists) {
		t.Fatalf("err = %v; want ErrAccountExists", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Not merely refused: the file must be untouched. A hand-edited, commented
	// config is the thing this whole append-only design exists to protect.
	if string(before) != string(after) {
		t.Errorf("file changed on a refused append:\n%s", after)
	}
}

// Appending must not fuse onto a last line that has no newline of its own, or
// the new table header becomes part of the previous value.
func TestAppendAccountToFileWithoutTrailingNewline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[[mount]]\npath = \"./m\""), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AppendAccount(path, "a", []Setting{{Key: "provider", Value: "gdrive"}}); err != nil {
		t.Fatalf("AppendAccount: %v", err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("appended file no longer parses: %v", err)
	}
	if _, ok := c.accounts["a"]; !ok {
		t.Error("account missing after append")
	}
}

// Comments and formatting in an existing file must survive, since losing them is
// exactly what appending rather than re-serializing is for.
func TestAppendAccountPreservesComments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := "# a comment worth keeping\n[[mount]]\npath = \"./m\" # trailing note\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AppendAccount(path, "a", []Setting{{Key: "provider", Value: "gdrive"}}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(got), original) {
		t.Errorf("original content was rewritten:\n%s", got)
	}
}

// ValidName allows a dot, and an unquoted [account.a.b] would define an account
// "b" nested under "a" — a table nothing would ever match.
func TestAccountBlockQuotesTheName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := AppendAccount(path, "a.b", []Setting{{Key: "provider", Value: "gdrive"}}); err != nil {
		t.Fatal(err)
	}
	ok, err := HasAccount(path, "a.b")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		body, _ := os.ReadFile(path)
		t.Errorf("dotted account name did not round-trip:\n%s", body)
	}
}

func TestHasAccountOnMissingFile(t *testing.T) {
	ok, err := HasAccount(filepath.Join(t.TempDir(), "absent.toml"), "a")
	if err != nil {
		t.Errorf("missing file should not be an error: %v", err)
	}
	if ok {
		t.Error("missing file reported an account")
	}
}

func TestAppendAccountRejectsBadName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := AppendAccount(path, "../escape", nil); err == nil {
		t.Fatal("a traversing account name was accepted")
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("a rejected name still created the config file")
	}
}
