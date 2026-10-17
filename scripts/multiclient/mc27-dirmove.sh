#!/usr/bin/env bash
# mc27-dirmove.sh — does a directory rename reach the other clients as a RENAME?
#
#   scripts/multiclient/mc27-dirmove.sh -d RIGDIR [-n FILES] [-r REPO] [-o OUT]
#
# The live check for MC-26b/MC-27 and for the seam change that fixed them
# (DESIGN.md §2.5, provider.RemoteChange.MovedFrom). Three eager clients: c1
# renames a directory of FILES entries, c2 and c3 are told about it by the change
# feed alone.
#
# What MC-27 measured at 1100 files, before the fix: c1 pays one request, and
# each peer is told only that an empty directory appeared under the new name. It
# keeps the whole old subtree as orphans present nowhere remotely, its repair
# sweep downloads all 1100 files a second time, and its delete pass then proposes
# 1101 removals against a -max-deletes of 100 — so it refuses the whole pass,
# logs `reconcile complete`, and nothing retries. 2200 files, permanently.
#
# FILES defaults to 150 because that is the smallest size that reproduces the
# STUCK state rather than merely the duplication: the repair has to propose more
# deletions than the default cap of 100 for the pass to be abandoned. Below that
# a sweep tidies it up and the run only shows a bandwidth cost.
#
# Four assertions, and the first is the fix while the other three are the shapes
# it was supposed to remove:
#
#   move      each peer logs exactly one `[pull] move wide -> wide-renamed`
#             carrying the number of records it re-keyed. This is the ONLY
#             positive evidence; everything else below is an absence.
#   download  zero on each peer. Pre-fix this is FILES.
#   tree      the old directory is gone and the new one holds every file, on
#             every client, from the FEED alone — these runs set
#             sweep-interval = "0", so nothing else can have done it.
#   sweep     a forced -resync afterwards proposes no deletions and no pushes,
#             on the peers AND on c1. c1 is not a formality: the renaming client
#             had the same defect from the other side, because an outbound
#             rename dropped the record for the renamed path only and left every
#             descendant's baseline keyed under a path that no longer existed.
#             Pre-fix c1 refuses its own delete pass at FILES > 100.
#
# As with every script here, each client's PID is captured at start: stopping ONE
# client cannot be done by matching a command line, because the patterns match
# the shell doing the matching (README, "Stopping clients").
set -euo pipefail

rig=; repo=; n=150; out=
while getopts ":d:n:r:o:h" opt; do
  case $opt in
    d) rig=$OPTARG ;; n) n=$OPTARG ;; r) repo=$OPTARG ;; o) out=$OPTARG ;;
    h) sed -n '2,42p' "$0"; exit 0 ;;
    *) sed -n '2,42p' "$0" >&2; exit 2 ;;
  esac
done
[[ -n $rig ]] || { echo "mc27: -d RIGDIR is required" >&2; exit 2; }
rig=$(cd "$rig" && pwd)
repo=${repo:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}
[[ -x $repo/bin/drivel ]] || { echo "mc27: no $repo/bin/drivel — pass -r REPO" >&2; exit 2; }

work=${out:-$rig/results/mc27}; mkdir -p "$work"
report=$work/report.txt; : > "$report"
fleet=(c1 c2 c3)
declare -A pid mark
OLD=wide
NEW=wide-renamed

log() { printf '%s  %s\n' "$(date +%H:%M:%S)" "$*"; }
say() { printf '%-26s %s\n' "$1" "$2" | tee -a "$report"; }

mkcfg() { # mkcfg CLIENT SUFFIX [LINE...]
  local c=$1 suffix=$2; shift 2
  local extra=""
  if (( $# )); then extra=$(printf '%s\n' "$@"); fi
  awk -v extra="$extra" '
    /^\[mount\.provider\]/ && !ins { if (extra != "") printf "%s\n\n", extra; ins = 1 }
    { print }' "$rig/$c/config.toml" > "$rig/$c/config-$suffix.toml"
}

start() { # start SUFFIX CLIENT...
  local suffix=$1; shift
  local c
  for c in "$@"; do
    nohup "$repo/bin/drivel" mount -config "$rig/$c/config-$suffix.toml" \
      >> "$rig/$c/logs/mc27.log" 2>&1 &
    pid[$c]=$!
  done
  for c in "$@"; do
    local up=0 _
    for _ in $(seq 1 90); do
      if mount | grep -q " $rig/$c/mnt "; then up=1; break; fi
      sleep 1
    done
    [[ $up == 1 ]] || { echo "mc27: $c never mounted — see $rig/$c/logs/mc27.log" >&2; exit 1; }
  done
  sleep 3
}

stop_one() { # stop_one CLIENT
  local c=$1 _
  # Never fall back to 0: `kill -TERM 0` signals the whole process group, which
  # includes this script. An unknown PID means there is nothing of ours to stop.
  [[ -n ${pid[$c]:-} && ${pid[$c]} -gt 0 ]] || return 0
  kill -TERM "${pid[$c]}" 2>/dev/null || true
  for _ in $(seq 1 90); do
    if ! mount | grep -q " $rig/$c/mnt "; then return 0; fi
    sleep 1
  done
  echo "mc27: $c did not unmount" >&2
}

stop_all() { local c; for c in "${fleet[@]}"; do stop_one "$c"; done; }
trap 'stop_all' EXIT

# Counters are read from a MARK, never from the whole log: a client's log
# accumulates across its own restarts, so "has this ever happened" cannot answer
# "did it happen again".
set_mark() { local c; for c in "${fleet[@]}"; do mark[$c]=$(wc -c < "$rig/$c/logs/mc27.log"); done; }
since() { # since CLIENT PATTERN
  tail -c "+$(( ${mark[$1]:-0} + 1 ))" "$rig/$1/logs/mc27.log" 2>/dev/null | grep -cF -- "$2" || true
}
wait_for() { # wait_for CLIENT PATTERN WANT TIMEOUT
  local c=$1 pat=$2 want=$3 t=${4:-300} _
  for _ in $(seq 1 "$t"); do
    if [[ $(since "$c" "$pat") -ge $want ]]; then return 0; fi
    sleep 1
  done
  return 1
}
count_in() { # count_in CLIENT DIR — files in a client's BACKING dir
  find "$rig/$1/data/$2" -type f 2>/dev/null | wc -l
}

# --- phase 0: a clean rig -----------------------------------------------------

log "purging the rig folder and every client's local state"
# Every drivel at once, by exact name. `pgrep -f`/`ps | grep` would match the
# shell doing the matching (README, "Stopping clients"), and a loop killing
# `head -1` three times races its own signals.
leftover=$(ps -eo pid,comm | awk '$2=="drivel"{print $1}')
if [[ -n $leftover ]]; then kill -TERM $leftover 2>/dev/null || true; fi
for _ in $(seq 1 60); do
  [[ $(mount | grep -c fuse.drivel) == 0 ]] && break
  sleep 1
done
python3 "$repo/scripts/multiclient/rig-purge.py" "$rig/c1" >/dev/null
for c in "${fleet[@]}"; do
  find "$rig/$c/data" -mindepth 1 -delete 2>/dev/null || true
  rm -f "$rig/$c/drivel-state.db" "$rig/$c/drivel-index.db"
  mkdir -p "$rig/$c/data" "$rig/$c/logs"
  : > "$rig/$c/logs/mc27.log"
  mkcfg "$c" mc27
  mkcfg "$c" mc27resync 'resync = true'
done

log "starting the fleet"
start mc27 "${fleet[@]}"

# --- phase 1: seed the directory ---------------------------------------------

set_mark
log "seeding $OLD/ with $n files on c1"
mkdir -p "$rig/c1/mnt/$OLD"
for j in $(seq 1 "$n"); do echo "entry $j" > "$rig/c1/mnt/$OLD/e$j.txt"; done
wait_for c1 '[sync] upload' "$n" 1800 || log "WARNING: c1 did not finish its $n uploads"
for c in c2 c3; do
  wait_for "$c" '[pull] download' "$n" 1800 || log "WARNING: $c did not receive all $n files"
done
say "seeded (files per client)" "c1=$(count_in c1 $OLD) c2=$(count_in c2 $OLD) c3=$(count_in c3 $OLD)"

# --- phase 2: the rename ------------------------------------------------------

set_mark
log "renaming $OLD -> $NEW on c1"
mv "$rig/c1/mnt/$OLD" "$rig/c1/mnt/$NEW"
wait_for c1 '[sync] rename' 1 300 || log "WARNING: c1 never pushed the rename"

log "waiting for the peers to apply it (feed only; sweep-interval = 0)"
for c in c2 c3; do
  wait_for "$c" '[pull] move' 1 600 || log "WARNING: $c never logged a move"
done
sleep 20   # let anything ELSE the feed wanted to do arrive, so the zeros mean something

echo | tee -a "$report"
say "=== from the feed alone ===" ""
printf '%-6s %-6s %-10s %-7s %-10s %-10s %s\n' client move download mkdir "old dir" "new dir" "refusals" | tee -a "$report"
for c in "${fleet[@]}"; do
  printf '%-6s %-6s %-10s %-7s %-10s %-10s %s\n' "$c" \
    "$(since "$c" '[pull] move')" "$(since "$c" '[pull] download')" \
    "$(since "$c" '[pull] mkdir')" \
    "$(count_in "$c" $OLD)" "$(count_in "$c" $NEW)" \
    "$(since "$c" 'REFUSING to delete')" | tee -a "$report"
done
echo | tee -a "$report"
for c in c2 c3; do
  grep -F -- '[pull] move' "$rig/$c/logs/mc27.log" | tail -1 | sed "s/^/  $c: /" | tee -a "$report"
done

# --- phase 3: the forced sweep, on every client -------------------------------

echo | tee -a "$report"
log "restarting every client with -resync, to see what a sweep now proposes"
stop_all
set_mark
start mc27resync "${fleet[@]}"
for c in "${fleet[@]}"; do
  wait_for "$c" '[sweep] reconcile complete' 1 1800 || log "WARNING: $c did not complete a sweep"
done
sleep 5

say "=== after a forced sweep ===" ""
printf '%-6s %-10s %-10s %-8s %-10s %-10s %s\n' client download "sweep del" rmdir refusals "old dir" "new dir" | tee -a "$report"
for c in "${fleet[@]}"; do
  printf '%-6s %-10s %-10s %-8s %-10s %-10s %s\n' "$c" \
    "$(since "$c" '[pull] download')" "$(since "$c" '[sweep] delete  ')" \
    "$(since "$c" '[sweep] rmdir')" \
    "$(since "$c" 'REFUSING to delete')" \
    "$(count_in "$c" $OLD)" "$(count_in "$c" $NEW)" | tee -a "$report"
done
echo | tee -a "$report"
for c in "${fleet[@]}"; do
  grep -F -- '[sweep] reconcile complete' "$rig/$c/logs/mc27.log" | tail -1 | sed "s/^/  $c: /" | tee -a "$report"
done

echo | tee -a "$report"
say "report" "$report"
