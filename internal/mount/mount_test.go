package mount

import "testing"

// Separate-directory mode returns the data dir verbatim, is not in-place, and its
// Close is a harmless no-op (no fd held).
func TestResolveBackingSeparateDir(t *testing.T) {
	dir := t.TempDir()
	b, err := ResolveBacking("/mnt/whatever", dir)
	if err != nil {
		t.Fatal(err)
	}
	if b.Path != dir {
		t.Fatalf("Path = %q; want %q", b.Path, dir)
	}
	if b.InPlace {
		t.Fatal("separate-dir mode reported InPlace")
	}
	if err := b.Close(); err != nil {
		t.Fatalf("Close on a no-fd backing errored: %v", err)
	}
}
