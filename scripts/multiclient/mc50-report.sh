#!/usr/bin/env bash
# mc50-report.sh WORKDIR — turn one MC-50 run's samples into the result table.
#
# Split out from the driver so a run can be re-reported without being re-run,
# and so the arithmetic is inspectable on its own. Per cell it prints, for each
# sampled process: mean and peak RSS, the high-water mark, CPU seconds spent in
# the cell, bytes read and written in the cell, and the fd and thread counts at
# its end. CPU, read and write are *deltas* across the cell, because the raw
# counters are cumulative since exec and a cumulative number says nothing about
# a workload.
set -euo pipefail
work=${1:?usage: mc50-report.sh WORKDIR}
cells=$work/cells.txt
[[ -r $cells && -r $work/proc.csv ]] || { echo "mc50-report: no samples in $work" >&2; exit 1; }

hz=$(getconf CLK_TCK)
for cell in idle light heavy; do
  s=$(awk -v c="$cell-start" '$2==c{print $1}' "$cells")
  e=$(awk -v c="$cell-end"   '$2==c{print $1}' "$cells")
  [[ -n $s && -n $e ]] || continue
  printf '\n== %s cell: %ds ==\n' "$cell" "$((e - s))"
  printf '%-14s %9s %9s %9s %9s %9s %9s %6s %7s\n' \
    process rss_mean_kb rss_peak_kb hwm_kb cpu_s read_MB write_MB fds threads
  awk -F, -v s="$s" -v e="$e" -v hz="$hz" '
    NR>1 && $1>=s && $1<=e {
      n[$2]++; rss[$2]+=$4; if ($5+0>hwm[$2]) hwm[$2]=$5+0
      if ($4+0>peak[$2]) peak[$2]=$4+0
      if (!(($2) in c0)) { c0[$2]=$6; r0[$2]=$7; w0[$2]=$8 }
      c1[$2]=$6; r1[$2]=$7; w1[$2]=$8; fd[$2]=$9; th[$2]=$10
    }
    END { for (p in n) printf "%-14s %9.0f %9d %9d %9.1f %9.1f %9.1f %6d %7d\n",
            p, rss[p]/n[p], peak[p], hwm[p], (c1[p]-c0[p])/hz,
            (r1[p]-r0[p])/1048576, (w1[p]-w0[p])/1048576, fd[p], th[p] }' \
    "$work/proc.csv" | sort
  if [[ -r $work/goroutines.csv ]]; then
    awk -F, -v s="$s" -v e="$e" '$1>=s && $1<=e && $3!="" {
        n[$2]++; sum[$2]+=$3; if ($3+0>mx[$2]) mx[$2]=$3+0; last[$2]=$3 }
      END { printf "  goroutines:"; for (c in n) printf " %s mean=%.0f peak=%d end=%s", c, sum[c]/n[c], mx[c], last[c]; print "" }' \
      "$work/goroutines.csv"
  fi
done
