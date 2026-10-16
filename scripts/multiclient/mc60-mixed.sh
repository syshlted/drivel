#!/usr/bin/env bash
# mc60-mixed.sh — MC-60's remainder: MC-20, MC-23 and MC-26 against a MIXED fleet.
#
#   scripts/multiclient/mc60-mixed.sh -d RIGDIR [-m MiB] [-n FILES] [-c CASES]
#                                     [-r REPO] [-o OUT]
#
# MC-60 is "A eager, B eager, C -lazy", and its first run covered MC-03 and
# MC-31 against that fleet. The three left are the ones where lazy mode changes
# what the fleet *costs* rather than what it holds:
#
#   mc20  a 4 KiB edit to a MiB-sized file. The base row's headline is 3 GiB of
#         traffic for 4 KiB at three eager clients. A lazy peer takes a
#         placeholder instead, so the question is what the fleet pays and what
#         that peer holds afterwards — including the case the base row cannot
#         reach, where the peer had already hydrated the file and an inbound
#         change arrives over real local content.
#   mc23  rm -rf a subtree of FILES entries. Feed-driven deletes are not bounded
#         by -max-deletes, and the lazy peer is deleting placeholders rather than
#         content, which is the same decision over a file whose bytes it never had.
#   mc26  a file rename, then a directory rename. The directory half is left to
#         the sweep (MC-26b), so on the lazy peer the orphaned subtree is made of
#         placeholders and the repair has to work over them.
#
# The fleet's own config.toml files are NOT edited: each client gets a derived
# config-mc60.toml (c3's with `lazy = true`), and a second config-mc60resync.toml
# for the sweep that repairs the directory rename. Deriving rather than mutating
# is what lets this be re-run after a failed attempt without the rig drifting.
#
# Every client's PID is captured at start, because stopping ONE client cannot be
# done by matching a command line — `pgrep -f`/`ps | awk` match the shell running
# them (README, "Stopping clients"), and `comm` cannot tell two clients apart.
set -euo pipefail

rig=; mib=1024; files=150; cases="mc20 mc23 mc26"; out=; repo=
while getopts ":d:m:n:c:r:o:h" opt; do
  case $opt in
    d) rig=$OPTARG ;; m) mib=$OPTARG ;; n) files=$OPTARG ;; c) cases=$OPTARG ;;
    r) repo=$OPTARG ;; o) out=$OPTARG ;;
    h) sed -n '2,33p' "$0"; exit 0 ;;
    *) sed -n '2,33p' "$0" >&2; exit 2 ;;
  esac
done
[[ -n $rig ]] || { echo "mc60: -d RIGDIR is required" >&2; exit 2; }
rig=$(cd "$rig" && pwd)
repo=${repo:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}
# Checked before anything is torn down: a frozen copy of this script resolves
# its own path to wherever it was copied, which mc40b-drive.sh discovered by
# writing 80 MiB into an unmounted mountpoint and carrying on.
[[ -x $repo/bin/drivel ]] || { echo "mc60: no $repo/bin/drivel — pass -r REPO" >&2; exit 2; }
work=${out:-$rig/results/mc60}; mkdir -p "$work"
report=$work/report.txt
: > "$report"

fleet=(c1 c2 c3)
declare -A pid mark
MARKER=user.drivel.placeholder   # hydrate.XattrName on linux

log() { printf '%s  %s\n' "$(date +%H:%M:%S)" "$*"; }
say() { printf '%-40s %s\n' "$1" "$2" | tee -a "$report"; }

stop_all() {
  local pids
  pids=$(ps -eo pid,comm | awk '$2=="drivel"{print $1}')
  if [[ -n $pids ]]; then kill -TERM $pids 2>/dev/null || true; fi
  for _ in $(seq 1 60); do
    if [[ $(mount | grep -c fuse.drivel) == 0 ]]; then return 0; fi
    sleep 1
  done
  echo "mc60: a mount is still up after 60s" >&2
}
trap 'stop_all' EXIT

# A config derived from the client's own, with lines inserted ahead of the
# [mount.provider] table — appending would put them INSIDE it.
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
      >> "$rig/$c/logs/mc60.log" 2>&1 &
    pid[$c]=$!
  done
  for c in "$@"; do
    local up=0 _
    for _ in $(seq 1 90); do
      if mount | grep -q " $rig/$c/mnt "; then up=1; break; fi
      sleep 1
    done
    if [[ $up != 1 ]]; then
      echo "mc60: $c never mounted — see $rig/$c/logs/mc60.log" >&2
      exit 1
    fi
  done
  sleep 3
}

stop_one() { # stop_one CLIENT
  local c=$1 _
  kill -TERM "${pid[$c]}" 2>/dev/null || true
  for _ in $(seq 1 60); do
    if ! mount | grep -q " $rig/$c/mnt "; then return 0; fi
    sleep 1
  done
  echo "mc60: $c did not unmount" >&2
}

# Counters are read from a MARK, never from the whole log: a client's log
# accumulates across its own restarts, so "has this ever happened" cannot answer
# "did it happen again" (Group V's testLog.saw lesson, in bash).
set_mark() { local c; for c in "${fleet[@]}"; do mark[$c]=$(wc -c < "$rig/$c/logs/mc60.log"); done; }
since() { # since CLIENT PATTERN
  tail -c "+$(( ${mark[$1]:-0} + 1 ))" "$rig/$1/logs/mc60.log" 2>/dev/null | grep -cF -- "$2" || true
}

md5of() { md5sum < "$1" | cut -d' ' -f1; }

# apparent-size  on-disk-bytes  marker, for one backing file.
pstate() {
  python3 - "$1" <<'PY'
import os, sys
p = sys.argv[1]
try:
    st = os.stat(p)
except FileNotFoundError:
    print("absent absent absent"); raise SystemExit
try:
    os.getxattr(p, "user.drivel.placeholder"); m = "marker"
except OSError:
    m = "no-marker"
print(f"{st.st_size} {st.st_blocks * 512} {m}")
PY
}

# Hashing the file on every poll is what a naive version of this does, and at a
# GiB it costs more reads per second than the transfer it is waiting for — on
# every client at once. Poll (size, mtime), which is free, and hash only when one
# of them moves. A download lands by rename, so the signature changes once.
wait_md5() { # wait_md5 FILE WANT TIMEOUT
  local f=$1 want=$2 t=${3:-900} _ sig last=""
  for _ in $(seq 1 "$t"); do
    sig=$(stat -c '%s %Y' "$f" 2>/dev/null || echo absent)
    if [[ $sig != "$last" ]]; then
      last=$sig
      if [[ -e $f && $(md5of "$f") == "$want" ]]; then return 0; fi
    fi
    sleep 1
  done
  return 1
}
wait_size() { # wait_size FILE WANT TIMEOUT
  local f=$1 want=$2 t=${3:-900} _
  for _ in $(seq 1 "$t"); do
    if [[ $(stat -c %s "$f" 2>/dev/null || echo -1) == "$want" ]]; then return 0; fi
    sleep 1
  done
  return 1
}
wait_count() { # wait_count DIR WANT TIMEOUT
  local d=$1 want=$2 t=${3:-1800} _
  for _ in $(seq 1 "$t"); do
    if [[ $(find "$d" -type f 2>/dev/null | wc -l) -ge $want ]]; then return 0; fi
    sleep 1
  done
  return 1
}
wait_gone() { # wait_gone PATH TIMEOUT
  local p=$1 t=${2:-900} _
  for _ in $(seq 1 "$t"); do
    if [[ ! -e $p ]]; then return 0; fi
    sleep 1
  done
  return 1
}

mc20() {
  echo | tee -a "$report"
  say "== MC-20 on a mixed fleet" "${mib} MiB, 4 KiB at its middle"
  set_mark
  log "writing ${mib} MiB through c1"
  "$repo/scripts/multiclient/workload.sh" large "$rig/c1/mnt" "$mib" >/dev/null 2>&1
  local want size t0
  want=$(md5of "$rig/c1/data/large/blob.bin")
  size=$(stat -c %s "$rig/c1/data/large/blob.bin")
  t0=$(date +%s)
  if ! wait_md5 "$rig/c2/data/large/blob.bin" "$want" 1800; then say "c2 never got the file" FAIL; return 1; fi
  say "c2 (eager) holds the create" "$(( $(date +%s) - t0 ))s, whole"
  if ! wait_size "$rig/c3/data/large/blob.bin" "$size" 900; then say "c3 never got the path" FAIL; return 1; fi
  sleep 5
  say "c3 (lazy) after the create" "$(pstate "$rig/c3/data/large/blob.bin") of $size B"

  log "hydrating c3 by reading through its mount"
  local h0 c3md5 hyd
  h0=$(date +%s); c3md5=$(md5of "$rig/c3/mnt/large/blob.bin"); hyd=$(( $(date +%s) - h0 ))
  say "c3 hydrates on read" "${hyd}s, md5 $([[ $c3md5 == "$want" ]] && echo matches || echo "DIFFERS"), ${mib} MiB on demand"
  say "c3 after hydrating" "$(pstate "$rig/c3/data/large/blob.bin")"

  # The create's traffic and the edit's are separate numbers and only the second
  # one is MC-20: three eager clients move 3 GiB for 4 KiB, and the question here
  # is what that becomes when one of them is lazy. Counted from a second mark so
  # the create is not folded in.
  local c cu cd
  for c in "${fleet[@]}"; do
    cu=$(since "$c" '[sync] upload'); cd=$(since "$c" '[pull] download')
    say "$c create: uploads / downloads" "$cu / $cd"
  done
  say "c3 create: placeholders" "$(since c3 '[pull] placeholder')"
  # A dd of a GiB takes far longer than the 300ms debounce window, so OpCreate
  # and OpWrite cannot coalesce and the path is pushed twice (MC-40 found the
  # same shape on SFTP, where it cost two full uploads). On Drive the second push
  # meets M6 gate 3, which asks what the remote already holds — so the duplicate
  # costs one hash of the local file instead of a second upload. Counted, because
  # the difference between those two outcomes is a gigabyte.
  say "c1 create: gate-3 skips" "$(since c1 'remote already holds')"
  set_mark

  log "4 KiB into the middle, on c1 — the whole-file Put"
  local want2 e0
  e0=$(date +%s)
  "$repo/scripts/multiclient/workload.sh" partial "$rig/c1/mnt" "$mib" >/dev/null 2>&1
  want2=$(md5of "$rig/c1/data/large/blob.bin")
  if ! wait_md5 "$rig/c2/data/large/blob.bin" "$want2" 1800; then say "c2 never got the edit" FAIL; return 1; fi
  say "c2 holds the edit" "$(( $(date +%s) - e0 ))s, whole file again"

  # The case the base row cannot reach: an inbound change over content this peer
  # had already hydrated. Either it is re-placeholdered (the local copy was
  # synced, so there is nothing for §6 to protect) or it is downloaded whole.
  local _ st="" sig last=""
  last=$(stat -c '%s %Y' "$rig/c3/data/large/blob.bin" 2>/dev/null || echo absent)
  for _ in $(seq 1 900); do
    st=$(pstate "$rig/c3/data/large/blob.bin")
    if [[ $st == *marker* ]]; then break; fi
    sig=$(stat -c '%s %Y' "$rig/c3/data/large/blob.bin" 2>/dev/null || echo absent)
    if [[ $sig != "$last" ]]; then
      last=$sig
      if [[ $(md5of "$rig/c3/data/large/blob.bin") == "$want2" ]]; then break; fi
    fi
    sleep 1
  done
  say "c3 after the edit" "$st"
  local c3md5b
  c3md5b=$(md5of "$rig/c3/mnt/large/blob.bin")
  say "c3 reads the NEW bytes" "$([[ $c3md5b == "$want2" ]] && echo yes || echo "NO — $c3md5b")"
  say "conflict copies anywhere" "$(( $(since c1 '[pull] conflict') + $(since c2 '[pull] conflict') + $(since c3 '[pull] conflict') ))"
  local eu ed total=0
  for c in "${fleet[@]}"; do
    eu=$(since "$c" '[sync] upload'); ed=$(since "$c" '[pull] download')
    say "$c edit: uploads / downloads" "$eu / $ed"
    total=$(( total + eu + ed ))
  done
  say "c3 edit: placeholders / restamps" "$(since c3 '[pull] placeholder') / $(since c3 '[pull] restamp')"
  say "c1 edit: gate-3 skips" "$(since c1 'remote already holds')"
  say "whole-file transfers for 4 KiB" "$total × ${mib} MiB = $(( total * mib )) MiB"
  say "range writes (must be 0)" "$(( $(since c1 '[sync] range write') + $(since c2 '[sync] range write') + $(since c3 '[sync] range write') ))"
}

mc23() {
  echo | tee -a "$report"
  say "== MC-23 on a mixed fleet" "rm -rf a $files-entry subtree"
  set_mark
  log "seeding $files entries through c1"
  "$repo/scripts/multiclient/workload.sh" wide "$rig/c1/mnt" "$files" >/dev/null
  local t0; t0=$(date +%s)
  if ! wait_count "$rig/c2/data/wide" "$files" 2400; then say "c2 never got the subtree" FAIL; return 1; fi
  if ! wait_count "$rig/c3/data/wide" "$files" 2400; then say "c3 never got the subtree" FAIL; return 1; fi
  sleep 15
  say "seeded and propagated" "$(( $(date +%s) - t0 ))s"
  local sample; sample=$(find "$rig/c3/data/wide" -type f -print -quit)
  say "c3's entries are placeholders" "$(pstate "$sample")"

  log "rm -rf on c1"
  local d0; d0=$(date +%s)
  "$repo/scripts/multiclient/workload.sh" bulk-delete "$rig/c1/mnt" wide >/dev/null
  local c ok=1
  for c in "${fleet[@]}"; do
    if ! wait_gone "$rig/$c/data/wide" 2400; then say "$c still holds the subtree" FAIL; ok=0; fi
  done
  if [[ $ok == 1 ]]; then say "all three dropped the subtree" "$(( $(date +%s) - d0 ))s"; fi
  sleep 15
  say "c1 outbound deletes" "$(since c1 '[sync] delete')"
  for c in c2 c3; do say "$c inbound deletes" "$(since "$c" '[pull] delete')"; done
  say "max-deletes refusals (must be 0)" "$(( $(since c1 'REFUSING to delete') + $(since c2 'REFUSING to delete') + $(since c3 'REFUSING to delete') ))"
  say "downloads on c3 (lazy, must be 0)" "$(since c3 '[pull] download')"
}

mc26() {
  echo | tee -a "$report"
  say "== MC-26 on a mixed fleet" "one file, then a directory"
  set_mark
  echo "rename me" > "$rig/c1/mnt/rename-me.txt"
  local want c
  want=$(md5of "$rig/c1/data/rename-me.txt")
  for c in c2 c3; do
    if ! wait_size "$rig/$c/data/rename-me.txt" "$(stat -c %s "$rig/c1/data/rename-me.txt")" 600; then
      say "$c never got rename-me.txt" FAIL; return 1
    fi
  done
  mv "$rig/c1/mnt/rename-me.txt" "$rig/c1/mnt/renamed.txt"
  local ok=1
  for c in "${fleet[@]}"; do
    if ! wait_size "$rig/$c/data/renamed.txt" "$(stat -c %s "$rig/c1/data/renamed.txt")" 600; then ok=0; fi
    if ! wait_gone "$rig/$c/data/rename-me.txt" 600; then ok=0; fi
  done
  say "file rename propagates" "$([[ $ok == 1 ]] && echo "yes, all three" || echo FAIL)"
  say "c3's renamed file" "$(pstate "$rig/c3/data/renamed.txt")"

  log "directory rename — the half left to the sweep"
  mkdir -p "$rig/c1/mnt/dirA"
  local i
  for i in $(seq 1 10); do echo "entry $i" > "$rig/c1/mnt/dirA/e$i.txt"; done
  for c in c2 c3; do
    if ! wait_count "$rig/$c/data/dirA" 10 900; then say "$c never got dirA" FAIL; return 1; fi
  done
  sleep 10
  "$repo/scripts/multiclient/workload.sh" rename-dir "$rig/c1/mnt" dirA dirB >/dev/null
  sleep 60
  for c in c2 c3; do
    say "$c right after the dir rename" \
        "dirA=$(find "$rig/$c/data/dirA" -type f 2>/dev/null | wc -l) dirB=$(find "$rig/$c/data/dirB" -type f 2>/dev/null | wc -l)"
  done

  log "repairing both peers with a forced sweep"
  stop_one c2; stop_one c3
  start mc60resync c2 c3
  local repaired=1
  for c in c2 c3; do
    if ! wait_count "$rig/$c/data/dirB" 10 900; then repaired=0; fi
    if ! wait_gone "$rig/$c/data/dirA" 900; then repaired=0; fi
  done
  say "a forced sweep repairs it" "$([[ $repaired == 1 ]] && echo "yes, both peers" || echo FAIL)"
  if [[ -d $rig/c3/data/dirB ]]; then
    say "c3's repaired entries" "$(pstate "$(find "$rig/c3/data/dirB" -type f -print -quit)")"
  fi
  say "max-deletes refusals (must be 0)" "$(( $(since c2 'REFUSING to delete') + $(since c3 'REFUSING to delete') ))"
  stop_one c2; stop_one c3
  start mc60 c2 c3
}

log "MC-60 remainder: c1 eager, c2 eager, c3 lazy — cases: $cases"
stop_all
python3 "$repo/scripts/multiclient/rig-purge.py" "$rig/c1" --trash
for c in "${fleet[@]}"; do
  find "$rig/$c/data" "$rig/$c/logs" -mindepth 1 -delete 2>/dev/null || true
  rm -f "$rig/$c/drivel-state.db" "$rig/$c/drivel-index.db"
  mkdir -p "$rig/$c/data" "$rig/$c/logs"
  : > "$rig/$c/logs/mc60.log"
done
mkcfg c1 mc60;        mkcfg c2 mc60;        mkcfg c3 mc60        "lazy = true"
mkcfg c1 mc60resync 'resync = true'
mkcfg c2 mc60resync 'resync = true'
mkcfg c3 mc60resync 'lazy = true' 'resync = true'
start mc60 "${fleet[@]}"
say "fleet" "c1 eager, c2 eager, c3 lazy (lazy = true)"

for case_name in $cases; do
  case $case_name in
    mc20) mc20 ;;
    mc23) mc23 ;;
    mc26) mc26 ;;
    *) echo "mc60: unknown case $case_name" >&2; exit 2 ;;
  esac
done

echo | tee -a "$report"
log "done — report in $report"
cat "$report"
