#!/usr/bin/env bash
# workload.sh CASE MOUNTDIR [args] — the payload for one case from the plan.
#
# Everything is written THROUGH a client's mountpoint, because that is the path
# under test: a write into the backing dir generates no mount event and is a
# different scenario (the offline edit in MC-31b).
#
#   many DIR [N]           MC-11  N small files across 100 subdirs (default 10000)
#   large DIR [MiB]        MC-10  one incompressible file (default 1024)
#   partial DIR [MiB] [at] MC-20  4 KiB in place, mid-file — the whole-file re-upload
#   deep DIR [DEPTH]       MC-12  nesting (default 40)
#   wide DIR [N]           MC-12  N entries in one directory (default 5000)
#   names DIR              MC-12  the names Drive and POSIX disagree about
#   bulk-delete DIR        MC-23  rm -rf a subtree
set -euo pipefail

case_name=${1:?usage: workload.sh CASE MOUNTDIR [args]}
dir=${2:?usage: workload.sh CASE MOUNTDIR [args]}
shift 2
[[ -d $dir ]] || { echo "workload: no such directory: $dir" >&2; exit 1; }

case $case_name in
  many)
    n=${1:-10000}
    for i in $(seq 1 "$n"); do
      d=$dir/many/$(printf '%02d' $((i % 100)))
      mkdir -p "$d"
      printf 'file %d of %d\n' "$i" "$n" > "$d/f$i.txt"
    done
    echo "wrote $n files under $dir/many"
    ;;

  large)
    mib=${1:-1024}
    mkdir -p "$dir/large"
    # Incompressible, so nothing downstream can quietly make the transfer cheaper
    # than the number the case is measuring.
    dd if=/dev/urandom of="$dir/large/blob.bin" bs=1M count="$mib" status=progress
    echo "wrote ${mib} MiB to $dir/large/blob.bin"
    ;;

  partial)
    mib=${1:-1024}
    at=${2:-$((mib / 2))}
    f=$dir/large/blob.bin
    [[ -f $f ]] || { echo "workload: run 'large' first" >&2; exit 1; }
    # 4 KiB in the middle of a 1 GiB file. gdrive implements no RangePutter — Drive
    # cannot patch byte ranges — so this is a whole-file Put: one upload and N-1
    # downloads of the entire file for four kilobytes of change. That number is
    # the case.
    dd if=/dev/urandom of="$f" bs=4096 count=1 seek=$((at * 256)) conv=notrunc,fsync
    echo "rewrote 4 KiB at ${at} MiB in $f"
    ;;

  deep)
    depth=${1:-40}
    p=$dir/deep
    for _ in $(seq 1 "$depth"); do p=$p/n; done
    mkdir -p "$p"
    echo "bottom" > "$p/leaf.txt"
    echo "nested $depth deep under $dir/deep"
    ;;

  wide)
    n=${1:-5000}
    mkdir -p "$dir/wide"
    for i in $(seq 1 "$n"); do echo "$i" > "$dir/wide/e$i.txt"; done
    echo "wrote $n entries into one directory"
    ;;

  names)
    d=$dir/names
    mkdir -p "$d"
    echo a > "$d/plain.txt"
    echo b > "$d/with space.txt"
    echo c > "$d/quote'apostrophe.txt"      # escapeQuery, single quote
    echo d > "$d/back\\slash.txt"           # escapeQuery, backslash
    echo e > "$d/percent%20encoded.txt"
    echo f > "$d/únïcodé-Ω-🙂.txt"
    echo g > "$d/$(printf 'new\nline').txt" || echo "  (newline name rejected by the FS)"
    printf 'h' > "$d/$(head -c 250 < /dev/zero | tr '\0' 'x').txt" || echo "  (250-byte name rejected)"
    # Two names the sweep's local walk skips on purpose. A real user file that
    # happens to match either is never pushed by a reconcile — worth knowing
    # whether the change-feed path pushes it, because the two disagreeing is
    # worse than either answer.
    echo i > "$d/report (conflict 2024-01-01 00-00-00).pdf"
    echo j > "$d/.drivel-notes"
    echo "wrote the hostile-name set into $d"
    ;;

  bulk-delete)
    target=${1:-many}
    rm -rf "${dir:?}/$target"
    echo "removed $dir/$target"
    ;;

  *)
    sed -n '2,18p' "$0" >&2
    exit 2
    ;;
esac
