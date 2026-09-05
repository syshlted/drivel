package gauth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExtractCode(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		want  string
		isErr bool
	}{
		{"bare code", "4/0AeanS0abcDEF", "4/0AeanS0abcDEF", false},
		{"full redirect url", "http://127.0.0.1:53682/?state=xyz&code=4/0Aabc&scope=drive", "4/0Aabc", false},
		{"query string only", "state=xyz&code=4/0Aabc", "4/0Aabc", false},
		{"whitespace trimmed", "  4/0Acode  ", "4/0Acode", false},
		{"url without code", "http://127.0.0.1:53682/?state=xyz&error=access_denied", "", true},
		{"unparseable url", "http://127.0.0.1:53682/%zz", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := extractCode(tt.in)
			if tt.isErr {
				if err == nil {
					t.Fatalf("expected error, got code %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("extractCode(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestCredentialsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	want := Credentials{ClientID: "abc.apps.googleusercontent.com", ClientSecret: "s3cr3t", ProjectID: "proj-1"}
	if err := WriteCredentials(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("round trip: got %+v, want %+v", got, want)
	}
}

// LoadCredentials must also accept a "web" client block, not just "installed".
func TestLoadWebCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web.json")
	const body = `{"web":{"client_id":"web-id","client_secret":"web-secret","auth_uri":"x","token_uri":"y"}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.ClientID != "web-id" || got.ClientSecret != "web-secret" {
		t.Fatalf("web creds parsed wrong: %+v", got)
	}
}
