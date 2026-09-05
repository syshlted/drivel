#!/usr/bin/env bash
# mutate.sh [-s SEED] [-i SECONDS] [-n OPS] MOUNTDIR TAG
#
# A seeded random mutation generator: create, modify, rename, delete, one op every
# SECONDS. Run one per client for the soak (MC-53).
#
# Seeded because a soak failure is rare and there is no second chance at the
# evidence: the seed and the op log are what make a 19-hour divergence
# reproducible. TAG namespaces this client's files so two generators racing on the
# same path is a case you opt into rather than one you get by accident.
set -euo pipefail

seed=1
interval=10
ops=0    # 0 = until killed

while getopts ":s:i:n:h" opt; do
  case $opt in
    s) seed=$OPTARG ;;
    i) interval=$OPTARG ;;
    n) ops=$OPTARG ;;
    h) sed -n '2,12p' "$0"; exit 0 ;;
    *) sed -n '2,12p' "$0"; exit 2 ;;
  esac
done
shift $((OPTIND - 1))

dir=${1:?usage: mutate.sh MOUNTDIR TAG}
tag=${2:?usage: mutate.sh MOUNTDIR TAG}
work=$dir/soak/$tag
mkdir -p "$work"

RANDOM=$seed
echo "# mutate.sh seed=$seed interval=${interval}s tag=$tag dir=$work"

i=0
while (( ops == 0 || i < ops )); do
  i=$((i + 1))
  existing=("$work"/*.dat)
  have=0
  [[ -e ${existing[0]:-} ]] && have=${#existing[@]}

  # Weighted toward create and modify: a tree that only churns never grows, and
  # the cases that matter (sweep cost, index size) need it to.
  roll=$((RANDOM % 100))
  if (( have == 0 )) || (( roll < 40 )); then
    f=$work/$tag-$i.dat
    head -c $(( (RANDOM % 64 + 1) * 1024 )) /dev/urandom > "$f"
    echo "$(date +%s) create $f"
  elif (( roll < 75 )); then
    f=${existing[$((RANDOM % have))]}
    head -c $(( (RANDOM % 64 + 1) * 1024 )) /dev/urandom > "$f"
    echo "$(date +%s) modify $f"
  elif (( roll < 90 )); then
    f=${existing[$((RANDOM % have))]}
    mv "$f" "${f%.dat}-r$i.dat"
    echo "$(date +%s) rename $f -> ${f%.dat}-r$i.dat"
  else
    f=${existing[$((RANDOM % have))]}
    rm -f "$f"
    echo "$(date +%s) delete $f"
  fi
  sleep "$interval"
done
