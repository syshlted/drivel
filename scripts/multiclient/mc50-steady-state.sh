#!/usr/bin/env bash
# mc50-steady-state.sh — MC-50: what one client costs, per workload.
#
#   scripts/multiclient/mc50-steady-state.sh -d RIGDIR [-n FILES] [-m MINUTES] [-o OUT]
#
# Drives three cells against a live fleet and prints a row per client per cell:
#
#   idle    nothing happening at all
#   light   one write per client per minute
#   heavy   FILES files per client, written at once (MC-36's shape, scaled)
#
# The fleet must already be running, one `-pprof` port per client, and the ports
# are assumed to be 6061.. in client order. Each client is TWO processes since
# M9 — the host holds the mount, the engine and the state DB, the backend holds
# the path index — and both are sampled, because sampling only the host
# under-reports disk and CPU by the backend's whole share (sample.sh's header
# has the longer version of this).
#
# What it cannot measure, and the gap is the product's rather than the rig's:
# **Drive requests per minute.** Nothing counts HTTP requests — uploads and
# downloads are logged one line apiece, but a resumable upload is several
# requests and the poll loop's `changes.list` calls are logged only when they
# fail. So the transfer counts below are transfers, not requests, and an exact
# request rate needs instrumentation that does not exist (M14's territory).
set -euo pipefail

rig=; files=300; minutes=5; out=
while getopts ":d:n:m:o:h" opt; do
  case $opt in
    d) rig=$OPTARG ;; n) files=$OPTARG ;; m) minutes=$OPTARG ;; o) out=$OPTARG ;;
    h) sed -n '2,28p' "$0"; exit 0 ;;
    *) sed -n '2,28p' "$0" >&2; exit 2 ;;
  esac
done
[[ -n $rig ]] || { echo "mc50: -d RIGDIR is required" >&2; exit 2; }

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
work=${out:-$(mktemp -d)}; mkdir -p "$work"
clients=(c1 c2 c3)

# PIDs: the host by its config path, the backend as its only child.
specs=(); port=6061; ports=()
for c in "${clients[@]}"; do
  h=$(pgrep -x drivel -a | awk -v c="$c/config.toml" '$0 ~ c {print $1}')
  [[ -n $h ]] || { echo "mc50: $c is not running" >&2; exit 1; }
  b=$(pgrep -P "$h" | head -1)
  specs+=("$h:$c-host" "${b:-$h}:$c-backend")
  ports+=("$port"); port=$((port+1))
done
echo "sampling: ${specs[*]}"

"$repo/scripts/multiclient/sample.sh" -i 2 -o "$work/proc.csv" "${specs[@]}" &
sampler=$!
# Goroutines are not in /proc; sample.sh's header says to scrape them alongside.
( while :; do
    ts=$(date +%s)
    for i in "${!clients[@]}"; do
      n=$(curl -s --max-time 3 "http://127.0.0.1:${ports[$i]}/debug/pprof/goroutine?debug=1" \
          | awk 'NR==1{print $NF}')
      echo "$ts,${clients[$i]},${n:-}" >> "$work/goroutines.csv"
    done
    sleep 10
  done ) &
scraper=$!
trap 'kill $sampler $scraper 2>/dev/null || true' EXIT

mark() { echo "$(date +%s) $1" >> "$work/cells.txt"; echo "== $1 =="; }

mark idle-start
sleep $((minutes * 60))
mark idle-end

mark light-start
for ((m = 0; m < minutes; m++)); do
  for c in "${clients[@]}"; do printf 'light %s %s\n' "$m" "$c" > "$rig/$c/mnt/light-$c.txt"; done
  sleep 60
done
mark light-end

mark heavy-start
# Collect the workload PIDs and wait for exactly those: a bare `wait` also waits
# for the sampler and the goroutine scraper, which never exit, so the drain loop
# below is never reached. (Found by it not being reached.)
writers=()
for c in "${clients[@]}"; do
  ( "$repo/scripts/multiclient/workload.sh" shared "$rig/$c/mnt" "$c" "$files" ) &
  writers+=($!)
done
wait "${writers[@]}"
echo "  writes issued; waiting for the pushes to drain"
# Drained = every client has logged `files` uploads and nothing new for 60s.
last=0
while :; do
  n=0
  for c in "${clients[@]}"; do n=$((n + $(grep -c 'upload   shared36' "$rig/$c/logs/run.log" || true))); done
  [[ $n -ge $((files * ${#clients[@]})) ]] && break
  [[ $n == "$last" ]] && stall=$((${stall:-0} + 1)) || stall=0
  [[ ${stall:-0} -ge 6 ]] && { echo "  no progress for 60s at $n uploads; stopping"; break; }
  last=$n; sleep 10
done
mark heavy-end

kill $sampler $scraper 2>/dev/null || true
sleep 1
echo
"$repo/scripts/multiclient/mc50-report.sh" "$work"
echo "raw data in $work"
