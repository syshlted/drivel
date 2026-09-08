//go:build darwin

package vfs

// compulsoryOptions returns the mount options drivel always requests. See the
// linux file for the reasoning; the flags mean the same thing here.
//
// Both exist on darwin — the kernel defines MNT_NODEV and MNT_NOSUID, and
// macFUSE's mount helper takes the standard option table — so this list matches
// linux's. Like everything else macOS-specific in this tree it is written but not
// run: no maintainer has a Mac (DESIGN.md §2.9). If a live run ever shows the
// helper refusing one of these, the fix is to drop that string here, not to make
// the mount fall back silently to unsafe options.
func compulsoryOptions() []string { return []string{"nodev", "nosuid"} }
