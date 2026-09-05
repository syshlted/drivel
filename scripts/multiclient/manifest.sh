#!/usr/bin/env bash
# manifest.sh DIR — what a client holds: path, size, sha256, mtime, kind; one TSV
# row per file, sorted, so two manifests diff line for line.
#
# Read the BACKING dir, not the mountpoint. In lazy mode, hashing through the
# mount hydrates every placeholder — which downloads the whole tree and destroys
# the thing the manifest was measuring.
#
# §6 conflict copies are marked in the fifth column rather than dropped: they are
# local-only by policy, so a fleet that has resolved a conflict is *supposed* to
# disagree about them, and a convergence check filters them out (converge.sh
# does). Losing them entirely would hide the case where a peer manufactures
# copies in a loop.
#
# Paths arrive NUL-delimited and leave with tabs and newlines escaped, because
# MC-12 deliberately creates a file with a newline in its name and a line-oriented
# manifest turns that one file into two broken rows. Both sides escape the same
# way, so the manifests stay comparable.
set -euo pipefail

dir=${1:?usage: manifest.sh DIR}
cd "$dir"

find . -type f -printf '%P\0' \
  | LC_ALL=C sort -z \
  | while IFS= read -r -d '' p; do
      case $p in
        .drivel-*|*/.drivel-*) continue ;;
      esac
      kind=file
      case $p in *' (conflict '*) kind=conflict ;; esac

      esc=${p//$'\t'/'\t'}
      esc=${esc//$'\n'/'\n'}
      printf '%s\t%s\t%s\t%s\t%s\n' \
        "$esc" "$(stat -c%s "$p")" "$(sha256sum < "$p" | cut -d' ' -f1)" "$(stat -c%Y "$p")" "$kind"
    done
