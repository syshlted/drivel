package syncengine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zishmusic/drivel/internal/state"
	"github.com/zishmusic/drivel/provider"
)

// setMtime stamps rel in dir with mtime t.
func setMtime(t *testing.T, dir, rel string, when time.Time) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.Chtimes(p, when, when); err != nil {
		t.Fatal(err)
	}
}

// findConflictCopy returns the content of the single "(conflict …)" file under
// dir, or ok=false if there is none.
func findConflictCopy(t *testing.T, dir string) (name, content string, ok bool) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "(conflict ") {
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			return e.Name(), string(b), true
		}
	}
	return "", "", false
}

// Both sides diverged and the remote is newer: remote wins the real path; the
// local edit is preserved as a conflict copy; the echo advances to the remote.
func TestConflictRemoteNewerWins(t *testing.T) {
	dir := t.TempDir()
	fs := newFakeStore()
	st := newState(t)
	fs.content["doc.txt"] = []byte("remote-edit")

	// Local file diverged from the last-synced baseline, stamped in the past.
	writeFile(t, dir, "doc.txt", "local-edit")
	setMtime(t, dir, "doc.txt", time.Now().Add(-time.Hour))
	_ = st.SetEcho("doc.txt", state.Echo{Hash: md5hex("baseline"), Version: "1", At: time.Now()})

	d := NewDownloader(nil, fs, dir, st, DefaultCadence)
	remote := &provider.RemoteFile{Path: "doc.txt", Hash: md5hex("remote-edit"), Version: "2", Modified: time.Now()}
	if err := d.apply(context.Background(), provider.RemoteChange{Path: "doc.txt", File: remote}); err != nil {
		t.Fatal(err)
	}

	if got, _ := readBacking(t, dir, "doc.txt"); got != "remote-edit" {
		t.Fatalf("real path = %q; want remote-edit (remote should win)", got)
	}
	name, content, ok := findConflictCopy(t, dir)
	if !ok || content != "local-edit" {
		t.Fatalf("conflict copy = (%q,%q,%v); want the local edit preserved", name, content, ok)
	}
	if e, ok, _ := st.GetEcho("doc.txt"); !ok || e.Hash != md5hex("remote-edit") {
		t.Fatalf("echo not advanced to remote content: %+v ok=%v", e, ok)
	}
}

// Both sides diverged and the local edit is newer: local keeps the real path; the
// remote version is saved alongside as a conflict copy; the local edit is left for
// the uploader to push (echo unchanged).
func TestConflictLocalNewerWins(t *testing.T) {
	dir := t.TempDir()
	fs := newFakeStore()
	st := newState(t)
	fs.content["doc.txt"] = []byte("remote-edit")

	writeFile(t, dir, "doc.txt", "local-edit")
	setMtime(t, dir, "doc.txt", time.Now()) // local is the newer writer
	_ = st.SetEcho("doc.txt", state.Echo{Hash: md5hex("baseline"), Version: "1", At: time.Now()})

	d := NewDownloader(nil, fs, dir, st, DefaultCadence)
	remote := &provider.RemoteFile{Path: "doc.txt", Hash: md5hex("remote-edit"), Version: "2", Modified: time.Now().Add(-time.Hour)}
	if err := d.apply(context.Background(), provider.RemoteChange{Path: "doc.txt", File: remote}); err != nil {
		t.Fatal(err)
	}

	if got, _ := readBacking(t, dir, "doc.txt"); got != "local-edit" {
		t.Fatalf("real path = %q; want local-edit (local should win)", got)
	}
	name, content, ok := findConflictCopy(t, dir)
	if !ok || content != "remote-edit" {
		t.Fatalf("conflict copy = (%q,%q,%v); want the remote version saved", name, content, ok)
	}
	// Echo must NOT advance to the remote — the local edit still needs pushing.
	if e, _, _ := st.GetEcho("doc.txt"); e.Hash == md5hex("remote-edit") {
		t.Fatal("echo advanced to remote content on a local-wins conflict")
	}
}

// Local matches the last-synced baseline (only the remote changed): a clean
// update, NOT a conflict — the remote is applied and no conflict copy appears.
func TestNoConflictWhenLocalUnchanged(t *testing.T) {
	dir := t.TempDir()
	fs := newFakeStore()
	st := newState(t)
	fs.content["doc.txt"] = []byte("remote-edit")

	writeFile(t, dir, "doc.txt", "baseline")
	_ = st.SetEcho("doc.txt", state.Echo{Hash: md5hex("baseline"), Version: "1", At: time.Now()})

	d := NewDownloader(nil, fs, dir, st, DefaultCadence)
	remote := &provider.RemoteFile{Path: "doc.txt", Hash: md5hex("remote-edit"), Version: "2", Modified: time.Now()}
	if err := d.apply(context.Background(), provider.RemoteChange{Path: "doc.txt", File: remote}); err != nil {
		t.Fatal(err)
	}

	if got, _ := readBacking(t, dir, "doc.txt"); got != "remote-edit" {
		t.Fatalf("real path = %q; want remote-edit (clean update)", got)
	}
	if name, _, ok := findConflictCopy(t, dir); ok {
		t.Fatalf("unexpected conflict copy %q on a clean update", name)
	}
}
