#!/usr/bin/env bash
# logcount.sh LOGFILE... — the log-derived counters for one case.
#
# Transfer volume and API pressure are visible in drivel's own output, which is
# cheaper than packet capture and is per mount by construction (M8 prefixes every
# line with the mount name). The rows that are not counters but alarms are marked.
set -euo pipefail
[[ $# -ge 1 ]] || { echo "usage: logcount.sh LOGFILE..." >&2; exit 2; }

# -F, and it is not optional: drivel prefixes its lines with "[sync]", "[pull]",
# "[drive]" and "[sweep]", and to a regular expression "[sync]" is a bracket
# expression matching one character out of {s,y,n,c}. Without -F every counter
# whose pattern carries a prefix silently reports 0 — which is every volume
# number this script exists to produce.
count() { grep -cF -- "$1" "$log" 2>/dev/null || true; }

for log in "$@"; do
  echo "== $log"
  # "[sync] upload", not "[sync] push": a push that succeeds logs the former, and
  # the only "[sync] push" lines in a log are failures and retries. Counting those
  # as uploads reported 0 for every working run — which is how the first live
  # MC-03 found that a successful upload used to log nothing at all.
  printf '  %-34s %s\n' "uploads"                  "$(count '[sync] upload')"
  printf '  %-34s %s\n' "downloads"                "$(count '[pull] download')"
  printf '  %-34s %s\n' "outbound deletes"         "$(count '[sync] delete')"
  printf '  %-34s %s\n' "outbound renames"         "$(count '[sync] rename')"
  printf '  %-34s %s\n' "inbound deletes"          "$(count '[pull] delete')"
  printf '  %-34s %s\n' "M6 unchanged-content skips" "$(count 'remote already holds')"
  printf '  %-34s %s\n' "M5 placeholder skips"     "$(count 'unhydrated placeholder')"
  printf '  %-34s %s\n' "conflict resolutions"     "$(count '[pull] conflict')"
  printf '  %-34s %s\n' "retries (transient/quota)" "$(count 'failed, retrying')"
  printf '  %-34s %s\n' "sweeps completed"         "$(count '[sweep] reconcile complete')"

  # These are not volume. Any non-zero value is a finding.
  for pat in \
    'range write:[sync] range write' \
    'same-name siblings (DATA LOSS):share the name' \
    'max-deletes refusals:REFUSING to delete' \
    'cursor expiries:cursor expired'
  do
    label=${pat%%:*}; needle=${pat#*:}
    n=$(count "$needle")
    if [[ $n -gt 0 ]]; then
      printf '  %-34s %s  <-- investigate\n' "$label" "$n"
    else
      printf '  %-34s %s\n' "$label" "$n"
    fi
  done
done
