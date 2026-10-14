#!/usr/bin/env bash
# mc51-scaling.sh — MC-51: how the cost of one change scales with fleet size.
#
#   scripts/multiclient/mc51-scaling.sh -d RIGDIR [-N "1 2 3 5"] [-k KiB]
#
# For each fleet size it starts that many clients cold, writes ONE file from the
# first, and measures what the fleet did about it: uploads, downloads,
# convergence time, and the transfers the change cost in total. The prediction
# under test is `1 upload + (N-1) downloads` per change, a request rate linear
# in N, and a convergence time that stays flat until throttling bends it.
#
# Each size starts from an empty remote folder and empty local state, because a
# cold client enumerates before it tails and the previous size's leftovers would
# be enumerated too — which is the same reason §2.2.1 gives for one rig per
# scenario.
#
# It drives the clients directly rather than through scenario.sh because the
# fleet size is the variable: the point is to add and remove clients, and a
# scenario is a fixed rig.
set -euo pipefail

rig=; sizes="1 2 3 5"; kib=1024
while getopts ":d:N:k:h" opt; do
  case $opt in
    d) rig=$OPTARG ;; N) sizes=$OPTARG ;; k) kib=$OPTARG ;;
    h) sed -n '2,20p' "$0"; exit 0 ;;
    *) sed -n '2,20p' "$0" >&2; exit 2 ;;
  esac
done
[[ -n $rig ]] || { echo "mc51: -d RIGDIR is required" >&2; exit 2; }
repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)

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

printf '%-3s %-10s %-9s %-10s %-12s %s\n' N converge_s uploads downloads transfers per_client
for n in $sizes; do
  stop_all
  python3 "$repo/scripts/multiclient/rig-purge.py" "$rig/c1" >/dev/null
  clients=()
  for ((i = 1; i <= n; i++)); do clients+=("c$i"); done
  for c in "${clients[@]}"; do
    find "$rig/$c/data" "$rig/$c/logs" -mindepth 1 -delete 2>/dev/null || true
    rm -f "$rig/$c/drivel-state.db" "$rig/$c/drivel-index.db"
    mkdir -p "$rig/$c/data" "$rig/$c/logs"
  done
  for c in "${clients[@]}"; do
    nohup "$repo/bin/drivel" mount -config "$rig/$c/config.toml" > "$rig/$c/logs/run.log" 2>&1 &
  done
  sleep 12

  head -c $((kib * 1024)) /dev/urandom > "$rig/c1/mnt/scale.bin"
  want=$(md5sum < "$rig/c1/data/scale.bin" | cut -d' ' -f1)
  t0=$(date +%s)
  conv=timeout
  for _ in $(seq 1 120); do
    ok=1
    for c in "${clients[@]}"; do
      [[ -e $rig/$c/data/scale.bin ]] || { ok=0; break; }
      [[ $(md5sum < "$rig/$c/data/scale.bin" | cut -d' ' -f1) == "$want" ]] || { ok=0; break; }
    done
    [[ $ok == 1 ]] && { conv=$(( $(date +%s) - t0 )); break; }
    sleep 1
  done

  # Let the log lines land before counting. At N=1 "converged" is true the
  # instant the file is written, so counting immediately reports zero uploads
  # for a push that simply had not happened yet — the debounce window alone is
  # 300ms. This wait is why the N=1 row means anything.
  sleep 15
  up=0; dn=0
  for c in "${clients[@]}"; do
    up=$((up + $(grep -c '\[sync\] upload' "$rig/$c/logs/run.log" || true)))
    dn=$((dn + $(grep -c '\[pull\] download' "$rig/$c/logs/run.log" || true)))
  done
  printf '%-3s %-10s %-9s %-10s %-12s %s\n' "$n" "$conv" "$up" "$dn" "$((up + dn))" \
    "$(awk -v t=$((up + dn)) -v n="$n" 'BEGIN{printf "%.2f", t/n}')"
done
stop_all
