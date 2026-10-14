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
#   shared DIR TAG [N]     MC-36  N files into ONE shared dir, named by TAG
#   touch DIR FILE         MC-21  mtime only, no content change
#   rewrite DIR FILE       MC-21  the same bytes again — M6 gate 3
#   append DIR [MiB] [N]   MC-22  N appends, each closing the file, 1s apart
#   specials DIR           MC-13  what Drive has no representation for
#   rename-dir DIR SRC DST MC-26b/MC-27  rename a directory
#   move DIR SRC DST       MC-28  move a file between directories
set -euo pipefail

case_name=${1:?usage: workload.sh CASE MOUNTDIR [args]}
dir=${2:?usage: workload.sh CASE MOUNTDIR [args]}
shift 2
[[ -d $dir ]] || { echo "workload: no such directory: $dir" >&2; exit 1; }

case $case_name in
  many)
    n=${1:-10000}
    # 4 KiB apiece, which is what MC-11 specifies. This wrote ~19-byte files until
    # 2026-10-10, and the recorded run's "190 KB of content" is that: it made the
    # case's write-amplification ratio a function of the rig rather than of the
    # product (see the plan, §4.17).
    #
    # The pad is built once — 10 000 subshells to generate it per file is slower
    # than the uploads. The per-file header keeps every file's content distinct, so
    # nothing downstream can make the transfer cheaper than the case measures.
    pad=$(head -c 4096 /dev/zero | tr '\0' x)
    for i in $(seq 1 "$n"); do
      d=$dir/many/$(printf '%02d' $((i % 100)))
      mkdir -p "$d"
      hdr=$(printf 'file %d of %d\n' "$i" "$n")
      printf '%s%s' "$hdr" "${pad:${#hdr}}" > "$d/f$i.txt"
    done
    echo "wrote $n files of 4 KiB under $dir/many"
    ;;

  shared)
    # MC-36: three clients writing concurrently into one directory. The names
    # must be distinct per client — `many` would have all three write the same
    # paths, which is MC-30 a thousand times over and a different case
    # entirely. 4 KiB apiece, as `many` uses, so the two are comparable.
    tag=${1:?usage: workload.sh shared DIR TAG [N]}
    n=${2:-1000}
    pad=$(head -c 4096 /dev/zero | tr '\0' x)
    mkdir -p "$dir/shared36"
    for i in $(seq 1 "$n"); do
      hdr=$(printf 'file %d of %d from %s\n' "$i" "$n" "$tag")
      printf '%s%s' "$hdr" "${pad:${#hdr}}" > "$dir/shared36/$tag-$i.txt"
    done
    echo "wrote $n files of 4 KiB as $tag-* under $dir/shared36"
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

  touch)
    f=${1:?usage: workload.sh touch DIR FILE}
    touch "$dir/$f"
    echo "touched $dir/$f"
    ;;

  rewrite)
    # The same bytes again, through the mount. This is the case MC-21 is really
    # about: a `touch` changes no content and so emits no content event at all
    # (vfs emits OpSetattr, which the engine drops), whereas a rewrite does reach
    # the push path and is where M6's unchanged-content gate either fires or does
    # not. The copy is staged outside the mount because a write into the backing
    # dir emits nothing.
    f=${1:?usage: workload.sh rewrite DIR FILE}
    tmp=$(mktemp)
    cp "$dir/$f" "$tmp"
    cp "$tmp" "$dir/$f"
    rm -f "$tmp"
    echo "rewrote $dir/$f with its own bytes ($(stat -c %s "$dir/$f") B)"
    ;;

  append)
    # MC-22: the amplification an append-style workload pays. Each iteration
    # CLOSES the file, which is what emits the event — an appender that holds it
    # open emits nothing until it closes, at any -push-delay.
    mib=${1:-1}
    n=${2:-20}
    gap=${3:-1}
    mkdir -p "$dir/append"
    f=$dir/append/log.bin
    : > "$f"
    for i in $(seq 1 "$n"); do
      dd if=/dev/urandom bs=1M count="$mib" status=none >> "$f"
      [[ $i -lt $n ]] && sleep "$gap"
    done
    echo "appended ${mib} MiB × $n to $f ($(stat -c %s "$f") B, $(( mib * n * (n + 1) / 2 )) MiB of cumulative content)"
    ;;

  specials)
    # MC-13: the file kinds a path-addressed cloud store has no representation
    # for, plus the two metadata cases. M15 items 1-3 decided what happens; this
    # is that decision meeting a real mount and a real Drive.
    d=$dir/specials
    mkdir -p "$d"
    : > "$d/empty.txt"                                   # zero bytes
    truncate -s 8M "$d/sparse.bin"                       # all hole
    dd if=/dev/urandom of="$d/sparse.bin" bs=4096 count=1 seek=1024 conv=notrunc status=none
    echo setuid > "$d/setuid.sh"; chmod 4755 "$d/setuid.sh"
    echo tz > "$d/non-utc.txt"; touch -d '2001-02-03 04:05:06 +1030' "$d/non-utc.txt"
    ln -s plain-target "$d/relative.link"  2>&1 || echo "  (relative symlink refused)"
    ln -s /etc/hostname "$d/absolute.link" 2>&1 || echo "  (absolute symlink refused)"
    mkfifo "$d/pipe" 2>&1 || echo "  (fifo refused)"
    echo hard > "$d/hardlink-src.txt"
    ln "$d/hardlink-src.txt" "$d/hardlink-dst.txt" 2>&1 || echo "  (hard link refused — M15 item 2)"
    echo "wrote the special-file set into $d"
    ;;

  rename-dir)
    src=${1:?usage: workload.sh rename-dir DIR SRC DST}
    dst=${2:?usage: workload.sh rename-dir DIR SRC DST}
    mv "$dir/$src" "$dir/$dst"
    echo "renamed $dir/$src -> $dir/$dst"
    ;;

  move)
    src=${1:?usage: workload.sh move DIR SRC DST}
    dst=${2:?usage: workload.sh move DIR SRC DST}
    mkdir -p "$(dirname "$dir/$dst")"
    mv "$dir/$src" "$dir/$dst"
    echo "moved $dir/$src -> $dir/$dst"
    ;;

  bulk-delete)
    target=${1:-many}
    rm -rf "${dir:?}/$target"
    echo "removed $dir/$target"
    ;;

  *)
    sed -n '2,21p' "$0" >&2
    exit 2
    ;;
esac
