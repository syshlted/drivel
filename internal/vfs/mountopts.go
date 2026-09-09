package vfs

// withCompulsory returns the options for a mount: whatever the caller asked for,
// followed by the ones drivel does not let anyone decline.
//
// The order is the whole point. Backend options reach here from places drivel
// does not control the wording of — an `-o` list in /etc/fstab, written by an
// administrator, forwarded verbatim by the mount(8) helper. `dev` and `suid` are
// real options that mean the opposite of what compulsoryOptions requests, and
// go-fuse's direct-mount path resolves the list by walking it in order and
// setting or clearing a bit per entry, so whichever mention comes last decides.
// Putting the compulsory list first would therefore let an fstab line turn off a
// guarantee DESIGN.md §9's M15 item 1 says has no flag to turn it off.
//
// Appending rather than filtering keeps one rule instead of two: a caller may say
// anything it likes, and drivel's answer is simply the last word. The caller's
// options are copied rather than appended to in place, because a caller that
// reused its slice would otherwise find drivel's options in it.
func withCompulsory(extra []string) []string {
	out := make([]string, 0, len(extra)+2)
	out = append(out, extra...)
	return append(out, compulsoryOptions()...)
}
