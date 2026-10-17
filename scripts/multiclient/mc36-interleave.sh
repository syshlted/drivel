#!/usr/bin/env bash
# mc36-interleave.sh — does inbound sync make progress while a client drains an
# outbound backlog?
#
#   scripts/multiclient/mc36-interleave.sh -d RIGDIR [-n FILES] [-r REPO] [-o OUT]
#
# MC-36 and MC-50 both measured inbound sync as effectively SUSPENDED while a
# client was pushing. In the 23.5 minutes its own uploads took, each writer
# fetched about **20** of its peers' 2000 files, and then took the remaining
# ~1980 in the quarter hour after its last upload landed; an idle peer with 3000
# files to fetch and nothing to push converged twelve minutes earlier. Nothing is
# logged either way, so from outside the process a suspended inbound loop and an
# idle one are indistinguishable.
#
# One mechanism was found on 2026-10-16 by reading `gdrive.Put` rather than by
# the goroutine dump that note asked for: it took the provider's `d.mu` and held
# it with a defer across the whole upload, and `d.mu` is what `Get` needs to
# resolve a path, what `Changes` needs for every entry in a page, and what
# `Enumerate` needs to register a page's folders. So a pushing client could not
# pull, and could not push any other path either — separately why
# -upload-workers bought nothing on Drive. It is a per-path lock now.
#
# What is proven in-tree is the EXCLUSION (a held Put no longer blocks a Stat or
# another path's Put, `internal/provider/gdrive/putlock_test.go`) and not the
# throughput. This measures the throughput, and it is deliberately the smallest
# shape that can: two clients write FILES files each at the same instant, into
# directories of their own — NOT one shared directory, because three clients
# calling Mkdir on one path concurrently produce same-name sibling folders
# (MC-36's other finding) and that would confound the number this is after.
#
# The number that matters is the last column of the summary: **how much of its
# peer's work each client had fetched by the time its own uploads were done.**
# Pre-fix that was ~1%. The arithmetic also does not fully close — one upload's
# wait per lock acquisition predicts ~4 s per inbound file against the ~70 s
# measured — so a result well short of parity is a real finding and means the
# state DB's single writer or the plugin's call serialisation is the rest of it.
set -euo pipefail

rig=; repo=; n=120; out=
while getopts ":d:n:r:o:h" opt; do
  case $opt in
    d) rig=$OPTARG ;; n) n=$OPTARG ;; r) repo=$OPTARG ;; o) out=$OPTARG ;;
    h) sed -n '2,37p' "$0"; exit 0 ;;
    *) sed -n '2,37p' "$0" >&2; exit 2 ;;
  esac
done
[[ -n $rig ]] || { echo "mc36i: -d RIGDIR is required" >&2; exit 2; }
rig=$(cd "$rig" && pwd)
repo=${repo:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}
[[ -x $repo/bin/drivel ]] || { echo "mc36i: no $repo/bin/drivel — pass -r REPO" >&2; exit 2; }

work=${out:-$rig/results/mc36i}; mkdir -p "$work"
report=$work/report.txt; : > "$report"
fleet=(c1 c2)
declare -A pid mark done_at
log() { printf '%s  %s\n' "$(date +%H:%M:%S)" "$*"; }
emit() { printf '%s\n' "$*" | tee -a "$report"; }

mkcfg() {
  local c=$1 suffix=$2; shift 2
  local extra=""
  if (( $# )); then extra=$(printf '%s\n' "$@"); fi
  awk -v extra="$extra" '
    /^\[mount\.provider\]/ && !ins { if (extra != "") printf "%s\n\n", extra; ins = 1 }
    { print }' "$rig/$c/config.toml" > "$rig/$c/config-$suffix.toml"
}
start() {
  local c
  for c in "${fleet[@]}"; do
    nohup "$repo/bin/drivel" mount -config "$rig/$c/config-mc36i.toml" \
      >> "$rig/$c/logs/mc36i.log" 2>&1 &
    pid[$c]=$!
  done
  for c in "${fleet[@]}"; do
    local up=0 _
    for _ in $(seq 1 90); do
      if mount | grep -q " $rig/$c/mnt "; then up=1; break; fi
      sleep 1
    done
    [[ $up == 1 ]] || { echo "mc36i: $c never mounted" >&2; exit 1; }
  done
  sleep 3
}
stop_all() {
  local c
  for c in "${fleet[@]}"; do
    # Never 0: `kill -TERM 0` signals this script's own process group.
    [[ -n ${pid[$c]:-} && ${pid[$c]} -gt 0 ]] || continue
    kill -TERM "${pid[$c]}" 2>/dev/null || true
  done
  for _ in $(seq 1 90); do
    [[ $(mount | grep -c fuse.drivel) == 0 ]] && return 0
    sleep 1
  done
}
trap 'stop_all' EXIT
since() { tail -c "+$(( ${mark[$1]:-0} + 1 ))" "$rig/$1/logs/mc36i.log" 2>/dev/null | grep -cF -- "$2" || true; }

log "cleaning the rig"
pids=$(ps -eo pid,comm | awk '$2=="drivel"{print $1}'); [[ -n $pids ]] && kill -TERM $pids 2>/dev/null || true
sleep 2
python3 "$repo/scripts/multiclient/rig-purge.py" "$rig/c1" >/dev/null
for c in "${fleet[@]}"; do
  find "$rig/$c/data" -mindepth 1 -delete 2>/dev/null || true
  rm -f "$rig/$c/drivel-state.db" "$rig/$c/drivel-index.db"
  mkdir -p "$rig/$c/data" "$rig/$c/logs"; : > "$rig/$c/logs/mc36i.log"
  mkcfg "$c" mc36i
done
start
for c in "${fleet[@]}"; do mark[$c]=$(wc -c < "$rig/$c/logs/mc36i.log"); done

log "both clients writing $n files, at once"
# Bare `wait` would also wait for the two MOUNTS, which are background jobs of
# this same shell and never exit — so the writers' PIDs are collected and waited
# for by name. That hung a whole run before it was noticed, with every number it
# wanted already sitting in the logs.
writers=()
for c in "${fleet[@]}"; do
  ( mkdir -p "$rig/$c/mnt/from-$c"
    for j in $(seq 1 "$n"); do echo "$c entry $j" > "$rig/$c/mnt/from-$c/e$j.txt"; done ) &
  writers+=($!)
done
t0=$(date +%s)

# Sampling runs WHILE the writers do, not after them. Writing through the mount is
# subject to the engine's back-pressure, so a bulk copy's `echo` blocks until a
# worker frees a slot (M27) — the write loop therefore lasts most of the upload
# phase, which is exactly the window this case is about.
emit "elapsed  c1-up  c1-down  c2-up  c2-down"
for _ in $(seq 1 240); do
  u1=$(since c1 '[sync] upload'); d1=$(since c1 '[pull] download')
  u2=$(since c2 '[sync] upload'); d2=$(since c2 '[pull] download')
  printf '%-8s %-6s %-8s %-6s %s\n' "$(( $(date +%s) - t0 ))s" "$u1" "$d1" "$u2" "$d2" | tee -a "$report"
  # The row to keep: the first sample at which a client's own uploads are done.
  # Its download column is the whole measurement — pre-fix it was ~1% of n.
  for c in "${fleet[@]}"; do
    if [[ -z ${done_at[$c]:-} ]]; then
      local_u=$(since "$c" '[sync] upload'); local_d=$(since "$c" '[pull] download')
      if [[ $local_u -ge $n ]]; then done_at[$c]="$local_d"; fi
    fi
  done
  if [[ $u1 -ge $n && $d1 -ge $n && $u2 -ge $n && $d2 -ge $n ]]; then break; fi
  sleep 10
done
# By name, never bare: a bare `wait` also waits for the two MOUNTS, which are
# background jobs of this same shell and never exit. That hung a whole run with
# every number it wanted already sitting in the logs.
wait "${writers[@]}" 2>/dev/null || true

emit ""
emit "=== peer work fetched by the time this client's own uploads finished ==="
emit "client   fetched / $n     own upload phase"
# Derived from the log's own timestamps rather than from the sampling above, which
# can only resolve to its poll interval and misses the moment entirely if a run is
# interrupted. This is the number the case exists to produce.
for c in "${fleet[@]}"; do
  L=$rig/$c/logs/mc36i.log
  # NOT `| head -1`: head exits after the first line, grep takes SIGPIPE, and
  # under `set -o pipefail` that fails the pipeline and `set -e` ends the run.
  # It is a race, so it passes for whichever client grep finished writing first
  # — this printed c1's row and then died on c2's. The README has the general
  # form of this landmine; awk reads to EOF and cannot trigger it.
  first=$(grep -F '[sync] upload' "$L" | awk 'NR==1{print $2}')
  last=$(grep -F '[sync] upload' "$L" | tail -1 | awk '{print $1" "$2}')
  if [[ -z $last ]]; then emit "$(printf '%-8s %s' "$c" '(no uploads at all)')"; continue; fi
  by=$(awk -v t="$last" '{ts=$1" "$2} ts<=t && /\[pull\] download/ {n++} END{print n+0}' "$L")
  emit "$(printf '%-8s %-17s %s -> %s' "$c" "$by" "$first" "${last#* }")"
done
emit ""
emit "A pushing client used to fetch about 1% of its peer's work while it pushed"
emit "(MC-36, MC-50). Anything near parity is the per-path upload lock working."
emit ""
emit "report: $report"
