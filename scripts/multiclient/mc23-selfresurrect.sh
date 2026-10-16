#!/usr/bin/env bash
# mc23-selfresurrect.sh — does a client's own `rm -rf` come back through its own
# change feed? Found by MC-51's N=1 sweep; reproduced here; unfixed.
#
#   scripts/multiclient/mc23-selfresurrect.sh -d RIGDIR [-r REPO] [-n N] [-c CYCLES]
#
# ONE client, so nothing else can be the source of any inbound work. Each cycle
# seeds N files in a fresh directory, waits for the pushes, `rm -rf`s it, waits
# for the deletes, and then reports what the client's own feed handed back.
#
# The defect: Drive's trashed flag on a child lags its parent's, so a feed page
# can still report an object the client has just deleted — and the delete push
# has already cleared that path's echo, so §4's loop breaker has nothing to match
# and the change reads as an ordinary remote create. The file is re-downloaded,
# which re-creates its directory; the next page reports the removal and deletes
# the FILE only, leaving an empty directory that exists nowhere remotely. At
# `sweep-interval = 0` that is permanent; with a sweep the local walk sees a
# local-only directory and pushes it back up, re-creating remotely a directory
# the user deleted.
#
# It is a race, so it does not fire every cycle: the poll has to land inside the
# window. Cycle 1 is the likeliest, because the inbound cadence has not yet
# backed off. A run where every cycle reads `gone` has not disproved anything —
# which is the whole reason this is a loop and not a single pass.
set -euo pipefail

rig=; repo=; n=20; cycles=8
while getopts ":d:r:n:c:h" opt; do
  case $opt in
    d) rig=$OPTARG ;; r) repo=$OPTARG ;; n) n=$OPTARG ;; c) cycles=$OPTARG ;;
    h) sed -n '2,30p' "$0"; exit 0 ;;
    *) sed -n '2,30p' "$0" >&2; exit 2 ;;
  esac
done
[[ -n $rig ]] || { echo "mc23-selfresurrect: -d RIGDIR is required" >&2; exit 2; }
rig=$(cd "$rig" && pwd)
repo=${repo:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}
[[ -x $repo/bin/drivel ]] || { echo "mc23-selfresurrect: no $repo/bin/drivel — pass -r REPO" >&2; exit 2; }
log=$rig/c1/logs/run.log

stop_all() {
  local pids
  pids=$(ps -eo pid,comm | awk '$2=="drivel"{print $1}')
  if [[ -n $pids ]]; then kill -TERM $pids 2>/dev/null || true; fi
  for _ in $(seq 1 30); do
    if [[ $(mount | grep -c fuse.drivel) == 0 ]]; then return 0; fi
    sleep 1
  done
}
trap 'stop_all' EXIT

since() { tail -c "+$(( $1 + 1 ))" "$log" | grep -cF -- "$2" || true; }
wait_for() { # wait_for MARK PATTERN WANT TIMEOUT
  local mark=$1 pat=$2 want=$3 t=${4:-300} _
  for _ in $(seq 1 "$t"); do
    if [[ $(since "$mark" "$pat") -ge $want ]]; then return 0; fi
    sleep 1
  done
  return 1
}

stop_all
python3 "$repo/scripts/multiclient/rig-purge.py" "$rig/c1" >/dev/null
find "$rig/c1/data" "$rig/c1/logs" -mindepth 1 -delete 2>/dev/null || true
rm -f "$rig/c1/drivel-state.db" "$rig/c1/drivel-index.db"
mkdir -p "$rig/c1/data" "$rig/c1/logs"
nohup "$repo/bin/drivel" mount -config "$rig/c1/config.toml" > "$log" 2>&1 &
for _ in $(seq 1 60); do
  if mount | grep -q " $rig/c1/mnt "; then break; fi
  sleep 1
done
sleep 5

printf '%-8s %-7s %-7s %-10s %-8s %s\n' cycle pull mkdir download delete "local dir"
for i in $(seq 1 "$cycles"); do
  mark=$(wc -c < "$log")
  mkdir -p "$rig/c1/mnt/sub$i"
  for j in $(seq 1 "$n"); do echo "$j" > "$rig/c1/mnt/sub$i/e$j.txt"; done
  wait_for "$mark" '[sync] upload' "$n" 600 || echo "  (not every push landed)" >&2
  sleep 5
  dmark=$(wc -c < "$log")
  rm -rf "$rig/c1/mnt/sub$i"
  wait_for "$dmark" '[sync] delete' $((n + 1)) 600 || echo "  (not every delete landed)" >&2
  sleep 25   # the feed has to get a chance to report our own removals back
  left=gone
  if [[ -e $rig/c1/data/sub$i ]]; then left="PRESENT <-- the defect"; fi
  printf '%-8s %-7s %-7s %-10s %-8s %s\n' "$i" \
    "$(since "$dmark" '[pull]')" "$(since "$dmark" '[pull] mkdir')" \
    "$(since "$dmark" '[pull] download')" "$(since "$dmark" '[pull] delete')" "$left"
done
