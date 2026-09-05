#!/usr/bin/env bash
# sample.sh [-i SECONDS] [-o FILE] PID[:NAME]...
#
# Per-client resource cost, sampled from /proc into a CSV. Ctrl-C to stop; the
# summary at the end is the row a result table wants.
#
#   rss_kb / hwm_kb   steady-state footprint, and whether a 5 GiB file is
#                     streamed or buffered (hwm is the whole answer to that)
#   cpu_ticks         utime+stime; the delta across a case is that case's cost
#   read_b / write_b  disk amplification — a 4 KiB edit that rewrites 1 GiB
#                     shows up here and nowhere else
#   fds / threads     handle leaks under the M6 write capture, go-fuse pool growth
#
# Goroutines and heap need in-process instrumentation: start each client with its
# own -pprof port and scrape /debug/pprof alongside this. See README.md.
set -euo pipefail

interval=1
out=/dev/stdout
while getopts ":i:o:h" opt; do
  case $opt in
    i) interval=$OPTARG ;;
    o) out=$OPTARG ;;
    h) sed -n '2,18p' "$0"; exit 0 ;;
    *) sed -n '2,18p' "$0"; exit 2 ;;
  esac
done
shift $((OPTIND - 1))
[[ $# -ge 1 ]] || { echo "usage: sample.sh PID[:NAME]..." >&2; exit 2; }

echo "ts,name,pid,rss_kb,hwm_kb,cpu_ticks,read_b,write_b,fds,threads" > "$out"

field() { awk -v k="$1" '$1 == k":" { print $2 }' "$2" 2>/dev/null || true; }

while :; do
  ts=$(date +%s)
  for spec in "$@"; do
    pid=${spec%%:*}
    name=${spec#*:}; [[ $name == "$spec" ]] && name=pid$pid
    st=/proc/$pid/status
    [[ -r $st ]] || continue

    rss=$(field VmRSS "$st"); hwm=$(field VmHWM "$st"); thr=$(field Threads "$st")
    # utime + stime, fields 14 and 15 of /proc/PID/stat, after the comm field
    # which may itself contain spaces — hence the split on ") ".
    cpu=$(awk -F') ' '{split($2,a," "); print a[12]+a[13]}' "/proc/$pid/stat" 2>/dev/null || echo 0)
    rd=$(awk '/^read_bytes:/{print $2}'  "/proc/$pid/io" 2>/dev/null || echo 0)
    wr=$(awk '/^write_bytes:/{print $2}' "/proc/$pid/io" 2>/dev/null || echo 0)
    fds=$(ls "/proc/$pid/fd" 2>/dev/null | wc -l)

    echo "$ts,$name,$pid,${rss:-0},${hwm:-0},${cpu:-0},${rd:-0},${wr:-0},$fds,${thr:-0}" >> "$out"
  done
  sleep "$interval"
done
