# Sourced by the generated git hook scripts (the `rc:` key in lefthook.yml)
# before they call lefthook. Checked in, because its whole job is to fail loudly.
#
# lefthook's hook script searches PATH, node_modules, bundler, yarn, mise and
# half a dozen other package managers, and when it finds none of them it prints
# "Can't find lefthook in PATH" and exits 0 — a hook that silently passes, which
# is worse than no hook at all: it manufactures confidence nothing earned. The rc
# is sourced in the hook's own shell, so exiting here aborts the commit or push.
#
# The leading ./ on the `rc:` path is required, not cosmetic: sh's `.` builtin
# searches PATH when its argument contains no slash, so a bare filename is never
# found in dash.

if [ -z "$LEFTHOOK_BIN" ] && ! command -v lefthook >/dev/null 2>&1; then
	echo "lefthook not found on PATH — install it from your OS package manager" >&2
	echo "(https://lefthook.dev/installation/), then re-run: make hooks" >&2
	exit 1
fi
