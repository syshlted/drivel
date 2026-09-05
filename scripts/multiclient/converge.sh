#!/usr/bin/env bash
# converge.sh [-t SECONDS] [-q] DIR...
#
# Blocks until every named backing dir holds identical content, then prints how
# long that took. Exits non-zero on timeout, after printing the first difference.
#
# "Converged" is one definition used by every case, or results are not comparable
# across cases or across runs (docs/multiclient-test-plan.md §2.6):
#
#   - content equality, not log silence. The poll cadence backs off to 30s, so a
#     quiet log is ambiguous; agreement on bytes is not.
#   - sustained across two samples 15s apart. A fleet caught mid-ping-pong agrees
#     for an instant every time the two sides cross.
#   - §6 conflict copies excluded, because divergence about them is the policy
#     working rather than failing. -q also reports how many exist.
#   - mtime excluded, because a drivel fleet never agrees on it and is not trying
#     to. A downloaded file carries the remote's modifiedTime (which is what makes
#     two *pulling* peers agree, and what stops a freshly downloaded file looking
#     newer than the remote it came from), but the peer that originated the write
#     keeps its own local write time and nothing writes the provider's stamp back
#     down over it. So one client's mtime differs from everyone else's for every
#     file, permanently. Comparing column 4 here would mean no case ever converges.
#     manifest.sh still reports it — it is diagnostic, and MC-13 wants it.
#
# Include the oracle's dir as one of the arguments to check against what Drive
# actually holds rather than only against the other clients.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
timeout=300
hold=15
report=0

while getopts ":t:qh" opt; do
  case $opt in
    t) timeout=$OPTARG ;;
    q) report=1 ;;
    h) sed -n '2,20p' "$0"; exit 0 ;;
    *) sed -n '2,20p' "$0"; exit 2 ;;
  esac
done
shift $((OPTIND - 1))
[[ $# -ge 2 ]] || { echo "converge.sh: need at least two dirs" >&2; exit 2; }

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

snapshot() {
  local i=0
  for d in "$@"; do
    "$here/manifest.sh" "$d" \
      | awk -F'\t' -v OFS='\t' '$5 != "conflict" { print $1, $2, $3, $5 }' > "$tmp/m$i"
    i=$((i + 1))
  done
}

# difference prints the first disagreement, or nothing.
difference() {
  local i=1
  while [[ -f $tmp/m$i ]]; do
    if ! cmp -s "$tmp/m0" "$tmp/m$i"; then
      printf '%s vs %s:\n' "$1" "${@:$((i + 1)):1}"
      diff "$tmp/m0" "$tmp/m$i" | head -20
      return 0
    fi
    i=$((i + 1))
  done
  return 1
}

start=$SECONDS
agreed_at=

while (( SECONDS - start < timeout )); do
  snapshot "$@"
  if difference "$@" > "$tmp/why" 2>&1; then
    agreed_at=          # still moving
  elif [[ -z $agreed_at ]]; then
    agreed_at=$SECONDS  # first agreement; it has to hold
  elif (( SECONDS - agreed_at >= hold )); then
    echo "converged in $((agreed_at - start))s (held ${hold}s)"
    if (( report )); then
      for d in "$@"; do
        m=$("$here/manifest.sh" "$d")
        n=$(printf '%s\n' "$m" | awk -F'\t' '$5 == "conflict"' | wc -l)
        files=$(printf '%s\n' "$m" | awk -F'\t' '$5 == "file"' | wc -l)
        printf '  %-40s %5s file(s)  %3s conflict copy(ies)\n' "$d" "$files" "$n"
      done
    fi
    exit 0
  fi
  sleep 5
done

echo "NOT converged after ${timeout}s:" >&2
cat "$tmp/why" >&2
exit 1
