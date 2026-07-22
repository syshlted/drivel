package state

import (
	"path/filepath"
	"testing"
	"time"
)

func TestEchoMatches(t *testing.T) {
	cases := []struct {
		name      string
		rec       Echo
		hash, ver string
		want      bool
	}{
		{"same hash", Echo{Hash: "abc", Version: "1"}, "abc", "9", true},
		{"different hash", Echo{Hash: "abc", Version: "1"}, "xyz", "1", false},
		{"no hashes, same version", Echo{Version: "5"}, "", "5", true},
		{"no hashes, different version", Echo{Version: "5"}, "", "6", false},
		{"record has hash, incoming none", Echo{Hash: "abc"}, "", "1", false},
		{"both empty", Echo{}, "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.rec.Matches(c.hash, c.ver); got != c.want {
				t.Errorf("Matches(%q,%q) = %v, want %v", c.hash, c.ver, got, c.want)
			}
		})
	}
}

func TestCursorAndEchoRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if _, ok, _ := st.Cursor(); ok {
		t.Fatal("fresh store reported a cursor")
	}
	if err := st.SetCursor("tok-1"); err != nil {
		t.Fatal(err)
	}
	if cur, ok, _ := st.Cursor(); !ok || cur != "tok-1" {
		t.Fatalf("cursor = %q ok=%v; want tok-1", cur, ok)
	}

	want := Echo{Hash: "h", Version: "2", At: time.Unix(1700000000, 0)}
	if err := st.SetEcho("p/q.txt", want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := st.GetEcho("p/q.txt")
	if err != nil || !ok || got.Hash != want.Hash || got.Version != want.Version {
		t.Fatalf("GetEcho = %+v ok=%v err=%v; want %+v", got, ok, err, want)
	}
	if err := st.DeleteEcho("p/q.txt"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.GetEcho("p/q.txt"); ok {
		t.Fatal("echo survived delete")
	}
}
