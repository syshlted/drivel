#!/usr/bin/env bash
# mc51-scaling.sh — MC-51: how the cost of one change scales with fleet size.
#
#   scripts/multiclient/mc51-scaling.sh -d RIGDIR [-N "1 2 3 5"] [-c CASE]
#                                       [-k KiB] [-m MiB] [-n FILES] [-r REPO]
#
# For each fleet size it starts that many clients cold, makes ONE change from the
# first, and measures what the fleet did about it. The prediction under test is
# `1 upload + (N-1) downloads` per change, a request rate linear in N, and a
# convergence time that stays flat until throttling bends it.
#
# The case asks for three shapes at each size, and -c picks one:
#
#   mc03  one 1 MiB file (default), i.e. the law on a cheap change.
#   mc20  a 4 KiB edit to a MiB-sized file. The same law, except every transfer
#         is the WHOLE file — Drive cannot patch byte ranges and gdrive therefore
#         implements no RangePutter — so this is where "a change costs N
#         transfers" turns into gigabytes, and the likeliest place in the plan to
#         meet a real quota. The create is set up and not measured; the row is
#         the edit.
#   mc23  rm -rf a FILES-entry subtree. Deletes take a different path from
#         content — through the feed, uncapped by -max-deletes — so the law has
#         to be measured separately rather than assumed from mc03.
#
# Each size starts from an empty remote folder and empty local state, because a
# cold client enumerates before it tails and the previous size's leftovers would
# be enumerated too — which is the same reason §2.2.1 gives for one rig per
# scenario. The trash is emptied with it: drivel's own deletes trash by default,
# so mc23 leaves its whole subtree counting against the account's quota.
#
# It drives the clients directly rather than through scenario.sh because the
# fleet size is the variable: the point is to add and remove clients, and a
# scenario is a fixed rig.
set -euo pipefail

rig=; sizes="1 2 3 5"; kib=1024; mib=1024; files=150; case_name=mc03; repo=; out=
while getopts ":d:N:k:m:n:c:r:o:h" opt; do
  case $opt in
    d) rig=$OPTARG ;; N) sizes=$OPTARG ;; k) kib=$OPTARG ;; m) mib=$OPTARG ;;
    n) files=$OPTARG ;; c) case_name=$OPTARG ;; r) repo=$OPTARG ;; o) out=$OPTARG ;;
    h) sed -n '2,36p' "$0"; exit 0 ;;
    *) sed -n '2,36p' "$0" >&2; exit 2 ;;
  esac
done
[[ -n $rig ]] || { echo "mc51: -d RIGDIR is required" >&2; exit 2; }
rig=$(cd "$rig" && pwd)
repo=${repo:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}
[[ -x $repo/bin/drivel ]] || { echo "mc51: no $repo/bin/drivel — pass -r REPO" >&2; exit 2; }
work=${out:-$rig/results/mc51-$case_name}; mkdir -p "$work"

# Each size starts by clearing every client's log, so an anomaly in one row
# cannot be investigated once the next row has started — which is exactly what
# happened to MC-23's N=1 row, whose "timeout" had to be reproduced from scratch
# to be explained. The logs are copied out at the end of every size instead.
archive() { # archive N
  local n=$1 c
  for c in "${clients[@]}"; do
    cp "$rig/$c/logs/run.log" "$work/N$n-$c.log" 2>/dev/null || true
  done
}

# Stopping is by process *name*, never by a pattern that could match this
# script's own command line — see the note in scripts/multiclient/README.md.
stop_all() {
  local pids
  pids=$(ps -eo pid,comm | awk '$2=="drivel"{print $1}')
  [[ -n $pids ]] && kill -TERM $pids 2>/dev/null || true
  for _ in $(seq 1 30); do
    [[ $(mount | grep -c fuse.drivel) == 0 ]] && return
    sleep 1
  done
}

clients=()
fleet_of() { # fleet_of N
  clients=()
  local i
  for ((i = 1; i <= $1; i++)); do clients+=("c$i"); done
}

cold_start() { # empty the remote, clear local state, start this size's clients
  stop_all
  python3 "$repo/scripts/multiclient/rig-purge.py" "$rig/c1" --trash >/dev/null
  local c
  for c in "${clients[@]}"; do
    find "$rig/$c/data" "$rig/$c/logs" -mindepth 1 -delete 2>/dev/null || true
    rm -f "$rig/$c/drivel-state.db" "$rig/$c/drivel-index.db"
    mkdir -p "$rig/$c/data" "$rig/$c/logs"
  done
  for c in "${clients[@]}"; do
    nohup "$repo/bin/drivel" mount -config "$rig/$c/config.toml" > "$rig/$c/logs/run.log" 2>&1 &
  done
  sleep 12
}

# Counting happens after a SETTLE, never at the moment a condition goes true. At
# N=1 convergence is trivially true the instant the file is written — the
# debounce window alone is 300ms — so counting immediately reported zero uploads
# for a push that had not happened yet, and at N=2 the cold-start enumeration
# was still landing and showed up as extra downloads. This wait is why the rows
# mean anything.
settle=15

# ...and 15 seconds is not enough once a single transfer takes four minutes.
# At N=1 there is no peer to confirm anything, so "converged" is true the instant
# the local write lands and MC-20's upload has barely started; counting there
# reports zero uploads for a gigabyte still going out. So the big cases wait for
# the fleet to go QUIET instead: no client's log has grown for `quiet` seconds.
# An idle fleet writes nothing — the poll loop logs only failures and every case
# runs with sweep-interval = "0" — so silence here means the work finished rather
# than that nothing was happening.
# ...and quiet is not the same as DONE. One 1 GiB upload logs nothing at all
# until it completes — the `[sync] upload` line is written on success — so the
# fleet is silent for four minutes in the middle of the very transfer being
# measured, and a 20-second quiet window fires inside it. At N >= 2 the
# content-convergence wait hides this, because no peer can hold the new bytes
# before the upload finishes; at N = 1 there is no peer and the row read zero
# uploads for a gigabyte still going out. So the writer's own push is waited for
# explicitly, by count, and `wait_quiet` is left to cover the peers afterwards.
wait_logged() { # wait_logged CLIENT PATTERN WANT TIMEOUT
  local c=$1 pat=$2 want=$3 t=${4:-3600} _ n
  for _ in $(seq 1 "$t"); do
    n=$(grep -cF -- "$pat" "$rig/$c/logs/run.log" || true)
    if [[ $n -ge $want ]]; then return 0; fi
    sleep 1
  done
  echo "mc51: $c never logged $want × '$pat'" >&2
  return 1
}

logged() { # logged CLIENT PATTERN -> count
  grep -cF -- "$2" "$rig/$1/logs/run.log" || true
}

wait_quiet() { # wait_quiet SECONDS TIMEOUT
  local quiet=${1:-20} t=${2:-3600} c size last="" still=0 _
  for _ in $(seq 1 "$t"); do
    size=0
    for c in "${clients[@]}"; do
      size=$(( size + $(wc -c < "$rig/$c/logs/run.log") ))
    done
    if [[ $size == "$last" ]]; then
      still=$((still + 1))
      if [[ $still -ge $quiet ]]; then return 0; fi
    else
      still=0; last=$size
    fi
    sleep 1
  done
  echo "mc51: fleet never went quiet" >&2
}

count() { # count PATTERN  -> fleet-wide total
  local pat=$1 c n total=0
  for c in "${clients[@]}"; do
    n=$(grep -cF -- "$pat" "$rig/$c/logs/run.log" || true)
    total=$((total + n))
  done
  echo "$total"
}

# Hashing every client's copy on every poll is what the mc03-only version did,
# and at MC-20's gigabyte that is N GiB of reads per second — more I/O than the
# transfers being measured, charged to the thing measuring them. Each client's
# (size, mtime) is polled instead, and only a client whose signature moved is
# hashed; a download lands by rename, so that happens once per client.
wait_all_md5() { # wait_all_md5 REL WANT TIMEOUT -> seconds, or "timeout"
  local rel=$1 want=$2 t=${3:-300} t0 c ok _ sig
  declare -A last agreed
  t0=$(date +%s)
  for _ in $(seq 1 "$t"); do
    ok=1
    for c in "${clients[@]}"; do
      if [[ ${agreed[$c]:-} == yes ]]; then continue; fi
      sig=$(stat -c '%s %Y' "$rig/$c/data/$rel" 2>/dev/null || echo absent)
      if [[ $sig != "${last[$c]:-}" ]]; then
        last[$c]=$sig
        if [[ -e $rig/$c/data/$rel && $(md5sum < "$rig/$c/data/$rel" | cut -d' ' -f1) == "$want" ]]; then
          agreed[$c]=yes; continue
        fi
      fi
      ok=0
    done
    if [[ $ok == 1 ]]; then echo $(( $(date +%s) - t0 )); return 0; fi
    sleep 1
  done
  echo timeout
}

wait_all_count() { # wait_all_count RELDIR WANT TIMEOUT -> seconds, or "timeout"
  local rel=$1 want=$2 t=${3:-1800} t0 c ok _
  t0=$(date +%s)
  for _ in $(seq 1 "$t"); do
    ok=1
    for c in "${clients[@]}"; do
      if [[ $(find "$rig/$c/data/$rel" -type f 2>/dev/null | wc -l) -lt $want ]]; then ok=0; break; fi
    done
    if [[ $ok == 1 ]]; then echo $(( $(date +%s) - t0 )); return 0; fi
    sleep 1
  done
  echo timeout
}

wait_all_gone() { # wait_all_gone REL TIMEOUT -> seconds, or "timeout"
  local rel=$1 t=${2:-1800} t0 c ok _
  t0=$(date +%s)
  for _ in $(seq 1 "$t"); do
    ok=1
    for c in "${clients[@]}"; do
      if [[ -e $rig/$c/data/$rel ]]; then ok=0; break; fi
    done
    if [[ $ok == 1 ]]; then echo $(( $(date +%s) - t0 )); return 0; fi
    sleep 1
  done
  echo timeout
}

row() { printf '%-3s %-10s %-9s %-10s %-12s %s\n' "$@"; }

case $case_name in
  mc03)
    echo "MC-51 / MC-03 at N = $sizes: one ${kib} KiB file"
    row N converge_s uploads downloads transfers per_client
    for n in $sizes; do
      fleet_of "$n"; cold_start
      head -c $((kib * 1024)) /dev/urandom > "$rig/c1/mnt/scale.bin"
      want=$(md5sum < "$rig/c1/data/scale.bin" | cut -d' ' -f1)
      conv=$(wait_all_md5 scale.bin "$want" 120)
      sleep "$settle"
      up=$(count '[sync] upload'); dn=$(count '[pull] download')
      archive "$n"
      row "$n" "$conv" "$up" "$dn" "$((up + dn))" \
        "$(awk -v t=$((up + dn)) -v n="$n" 'BEGIN{printf "%.2f", t/n}')"
    done
    ;;

  mc20)
    echo "MC-51 / MC-20 at N = $sizes: 4 KiB into the middle of a ${mib} MiB file"
    echo "(the create is setup; the row is the edit. Every transfer is the whole file.)"
    row N converge_s uploads downloads MiB_moved per_client
    for n in $sizes; do
      fleet_of "$n"; cold_start
      "$repo/scripts/multiclient/workload.sh" large "$rig/c1/mnt" "$mib" >/dev/null 2>&1
      want=$(md5sum < "$rig/c1/data/large/blob.bin" | cut -d' ' -f1)
      wait_logged c1 '[sync] upload' 1 3600 || true
      seeded=$(wait_all_md5 large/blob.bin "$want" 3600)
      if [[ $seeded == timeout ]]; then row "$n" "seed-timeout" - - - -; continue; fi
      wait_quiet 20 1800
      # The edit's own counters are the difference from here, so the create's are
      # read and subtracted rather than cleared: truncating a live mount's log
      # loses whatever it writes between the truncate and the next line.
      up0=$(count '[sync] upload'); dn0=$(count '[pull] download')
      c1up0=$(logged c1 '[sync] upload')
      "$repo/scripts/multiclient/workload.sh" partial "$rig/c1/mnt" "$mib" >/dev/null 2>&1
      want2=$(md5sum < "$rig/c1/data/large/blob.bin" | cut -d' ' -f1)
      wait_logged c1 '[sync] upload' $((c1up0 + 1)) 3600 || true
      conv=$(wait_all_md5 large/blob.bin "$want2" 3600)
      wait_quiet 20 1800
      up=$(( $(count '[sync] upload') - up0 )); dn=$(( $(count '[pull] download') - dn0 ))
      archive "$n"
      row "$n" "$conv" "$up" "$dn" "$(( (up + dn) * mib ))" \
        "$(awk -v t=$((up + dn)) -v n="$n" 'BEGIN{printf "%.2f", t/n}')"
    done
    ;;

  mc23)
    echo "MC-51 / MC-23 at N = $sizes: rm -rf a $files-entry subtree"
    echo "(deletes go through the feed and are NOT bounded by -max-deletes)"
    row N converge_s out_del in_del refusals per_client
    for n in $sizes; do
      fleet_of "$n"; cold_start
      "$repo/scripts/multiclient/workload.sh" wide "$rig/c1/mnt" "$files" >/dev/null
      wait_logged c1 '[sync] upload' "$files" 3600 || true
      seeded=$(wait_all_count wide "$files" 3600)
      if [[ $seeded == timeout ]]; then row "$n" "seed-timeout" - - - -; continue; fi
      wait_quiet 20 3600
      "$repo/scripts/multiclient/workload.sh" bulk-delete "$rig/c1/mnt" wide >/dev/null
      # rm -rf is complete locally the moment it returns, so at N = 1 "gone
      # everywhere" is true before the first delete has been sent.
      wait_logged c1 '[sync] delete' $((files + 1)) 3600 || true
      conv=$(wait_all_gone wide 3600)
      wait_quiet 20 3600
      od=$(count '[sync] delete'); id=$(count '[pull] delete'); rf=$(count 'REFUSING to delete')
      archive "$n"
      row "$n" "$conv" "$od" "$id" "$rf" \
        "$(awk -v t=$((od + id)) -v n="$n" 'BEGIN{printf "%.2f", t/n}')"
    done
    ;;

  *)
    echo "mc51: unknown case $case_name (want mc03, mc20 or mc23)" >&2
    exit 2
    ;;
esac
stop_all
