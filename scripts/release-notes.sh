#!/usr/bin/env bash
# release-notes.sh TAG — the GitHub release notes for TAG, on stdout.
#
# Nothing here is written by hand at release time. The notes are two things
# CHANGELOG.md already holds: its work-in-progress warning (the short pointer at
# docs/user/status.md, which is the warning's one source) and the section headed
# "## TAG". A tag with no such section is refused, which is what makes "update
# the changelog" a step a release cannot skip.
#
# CHANGELOG.md links to the repository's files by relative path, which GitHub
# resolves against the release page and gets wrong, so those links are rewritten
# to the files as they were at TAG.
set -euo pipefail

tag=${1:?usage: release-notes.sh TAG}
repo=https://github.com/syshlted/drivel

warning=$(awk '/^> \[!CAUTION\]/ { on = 1 } on && !/^>/ { exit } on' CHANGELOG.md)
section=$(awk -v tag="$tag" '
	$0 ~ "^## " { if (on) exit; if ($2 == tag) { on = 1; next } }
	on' CHANGELOG.md)

if [ -z "$warning" ]; then
	echo "release-notes: CHANGELOG.md has lost its work-in-progress warning" >&2
	exit 1
fi
if [ -z "$(printf '%s' "$section" | tr -d '[:space:]')" ]; then
	echo "release-notes: CHANGELOG.md has no '## $tag' section; write one before tagging" >&2
	exit 1
fi

# ](path) and ](path#anchor), but not ](https://…), ](#anchor) or ](mailto:…).
printf '%s\n\n%s\n' "$warning" "$section" |
	sed -E "s#\]\(([^):\#][^):]*)\)#](${repo}/blob/${tag}/\1)#g"
