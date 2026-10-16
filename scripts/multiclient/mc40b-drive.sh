#!/usr/bin/env bash
# mc40b-drive.sh — MC-40 (b) against real Drive: the two halves SFTP cannot show.
#
#   scripts/multiclient/mc40b-drive.sh -d RIGDIR [-r REPO] [-m MiB] [-w SECS]
#
# The SFTP leg (mc40b-lostpush.sh) is the A/B for the lost-push row itself. Two
# of its consequences are Drive's alone, and this is where they are measured.
#
#   D1  A lost rewrite on an ID-ADDRESSED backend. The repair re-pushes a path,
#       and a path on Drive is resolved rather than addressed, so the question
#       this answers is whether the re-push UPDATES the object or grows a second
#       one beside it — the duplicate half of MC-40, which a path-addressed
#       backend cannot even pose. rig-ls.py is the oracle, because two objects
#       at one path collapse into one local file on every client (MC-30).
#
#   D2  A metadata-only divergence SETTLING. gdrive is the tree's only
#       ContentHasher, so M6 gate 3 runs here and nowhere else. A touch moves the
#       mtime without moving a byte: the row proposes a push, the gate declines
#       it, and unless the gate RECORDS the baseline it just verified the
#       mismatch survives — one full-file hash per mount per sweep, forever. The
#       assertion is therefore about the SECOND sweep, not the first.
#
# It also carries its own negative control: a file whose content really did
# change while the client was down must be uploaded, in the same run, or D2
# passes merely by doing nothing at all.
#
# Wants RIGDIR laid out as scripts/multiclient/README.md describes, with
# config-mc40b.toml beside each client's config.toml, differing from it only in
# a short sweep-interval. Both clients point at one already-empty rig folder
# (rig-purge.py), because a cold client enumerates before it tails.
set -euo pipefail

rig=; repo=; mib=64; watch=240
while getopts ":d:r:m:w:h" opt; do
  case $opt in
    d) rig=$OPTARG ;; r) repo=$OPTARG ;; m) mib=$OPTARG ;; w) watch=$OPTARG ;;
    h) sed -n '2,30p' "$0"; exit 0 ;;
    *) sed -n '2,30p' "$0" >&2; exit 2 ;;
  esac
done
[[ -n $rig ]] || { echo "mc40b-drive: -d RIGDIR is required" >&2; exit 2; }
# -r is needed when this file is run from a copy outside the repo, which is the
# right way to run it: bash reads a script incrementally, so editing the original
# mid-run resumes the run at a stale offset.
repo=${repo:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}
[[ -x $repo/bin/drivel ]] || { echo "mc40b-drive: no $repo/bin/drivel — pass -r REPO" >&2; exit 2; }
bin=$repo/bin/drivel
ls=$repo/scripts/multiclient/rig-ls.py

# D2's file has to clear hashSkipMinSize (4 MiB, ranges.DefaultBlockSize) or the
# gate is never attempted and the case measures nothing.
small=8

stop_all() {
  local pids
  pids=$(ps -eo pid,comm | awk '$2=="drivel"{print $1}')
  [[ -n $pids ]] && kill -TERM $pids 2>/dev/null || true
  for _ in $(seq 1 40); do
    [[ $(mount | grep -c fuse.drivel) == 0 ]] && return
    sleep 1
  done
  for m in "$rig"/c1/mnt "$rig"/c2/mnt; do fusermount3 -u "$m" 2>/dev/null || true; done
}
md5of() { [[ -e $1 ]] && md5sum < "$1" | cut -d' ' -f1 || echo "ABSENT"; }
remote() { timeout 120 python3 "$ls" "$rig/c1" | awk -v n="$1" '$3==n{print $1}'; }
objects() { timeout 120 python3 "$ls" "$rig/c1" | awk -v n="$1" '$3==n' | wc -l; }
say() { printf '%s  %s\n' "$(date -u +%H:%M:%S)" "$*"; }
start() { # start CLIENT LOGNAME -> echoes the pid
  nohup "$bin" mount -config "$rig/$1/config-mc40b.toml" > "$rig/$1/logs/$2.log" 2>&1 &
  echo $!
}

stop_all
say "clearing local state (the remote folder must already be empty)"
for c in c1 c2; do
  find "$rig/$c/data" -mindepth 1 -delete 2>/dev/null || true
  rm -f "$rig/$c/drivel-state.db" "$rig/$c/drivel-index.db"
  mkdir -p "$rig/$c/data" "$rig/$c/logs"
done

c1=$(start c1 mc40b-1); _=$(start c2 mc40b-1)
# Wait for the filesystems to be LIVE, never a sleep. A client that failed to
# start leaves its mountpoint an ordinary directory, and every write below then
# lands in it and is never synced by anything — a rig failure that reads exactly
# like a product one, and the first run of this script produced it.
for c in c1 c2; do
  up=0
  for _ in $(seq 1 60); do
    mount | grep -q " $rig/$c/mnt " && { up=1; break; }
    sleep 1
  done
  [[ $up == 1 ]] || { echo "mc40b-drive: $c never mounted — see $rig/$c/logs/mc40b-1.log" >&2; stop_all; exit 1; }
done

# ---- seed all three files, and converge --------------------------------------
head -c $((mib * 1024 * 1024)) /dev/urandom > "$rig/c1/mnt/rewrite.bin"
head -c $((small * 1024 * 1024)) /dev/urandom > "$rig/c1/mnt/touched.bin"
head -c $((small * 1024 * 1024)) /dev/urandom > "$rig/c1/mnt/edited.bin"
r1=$(md5of "$rig/c1/data/rewrite.bin")
t1=$(md5of "$rig/c1/data/touched.bin")
e1=$(md5of "$rig/c1/data/edited.bin")
say "seeded: rewrite=$r1 touched=$t1 edited=$e1"
for _ in $(seq 1 "$watch"); do
  [[ $(md5of "$rig/c2/data/rewrite.bin") == "$r1" &&
     $(md5of "$rig/c2/data/touched.bin") == "$t1" &&
     $(md5of "$rig/c2/data/edited.bin")  == "$e1" ]] && break
  sleep 2
done
[[ $(remote rewrite.bin) == "$r1" ]] || { echo "mc40b-drive: seed never landed remotely" >&2; stop_all; exit 1; }
say "all three on Drive and on c2 — every path now has a baseline on c1"

# The in-flight figure below is a DELTA, so its baseline is taken here: rchar is
# cumulative over the process lifetime, and the seed's own three uploads are
# already in it. The backend is identified by PARENTAGE, not by a pattern — the
# peer has a healthy plugin-serve of its own, and a pattern over command lines
# also matches the shell running this script.
child=$(ps -eo pid,ppid,args | awk -v p="$c1" '$2==p && /plugin-serve/ {print $1}' | head -1)
rbase=$(awk '/^rchar/{print $2}' "/proc/${child:-0}/io" 2>/dev/null || echo 0)

# ---- D1: kill c1 inside the rewrite's upload ---------------------------------
head -c $((mib * 1024 * 1024)) /dev/urandom > "$rig/c1/mnt/rewrite.bin"
r2=$(md5of "$rig/c1/data/rewrite.bin")
say "v2 written locally: $r2 — waiting 5s, then SIGKILL inside the upload"
# No temporary to watch for: a resumable Drive session leaves nothing visible.
# The write is complete before the push starts (OpWrite is emitted on close), so
# this is a timer against a transfer whose duration the seed just measured.
sleep 5
# Evidence that the transfer really was in flight, which a Drive rig cannot show
# the way a temporary file does. It has to be the bytes the backend has READ: it
# receives the file from the host over the plugin socket and sends it to Drive
# over HTTP/3, and `wchar` cannot see a QUIC send at all — `sendmsg` is not in
# the write family /proc/<pid>/io counts, so a COMPLETE 64 MiB upload moves it
# 0.10 MiB (measured 2026-10-16). rchar tracks the transfer because the stream
# is flow-controlled end to end.
now=$(ps -eo pid,ppid,args | awk -v p="$c1" '$2==p && /plugin-serve/ {print $1}' | head -1)
if [[ -n $child && $now == "$child" ]]; then
  inflight=$(awk -v b="$rbase" '/^rchar/{printf "%.1f", ($2 - b) / 1048576}' "/proc/$child/io" 2>/dev/null || echo "?")
else
  # A relaunched backend voids the baseline rather than producing a wrong delta.
  inflight="? backend changed, ${child:-none} ->  ${now:-gone}, so no"
  child=$now
fi
kill -9 "$c1" 2>/dev/null || true
sleep 3
orphan=no
if [[ -n $child ]] && kill -0 "$child" 2>/dev/null; then orphan="YES (pid $child)"; fi
d1_kill_remote=$(remote rewrite.bin)
fusermount3 -u "$rig/c1/mnt" 2>/dev/null || true
say "after the kill: c1 local=$r2  Drive=$([[ $d1_kill_remote == "$r1" ]] && echo 'v1 (previous)' || echo "$d1_kill_remote")"

say "restarting c1"
c1=$(start c1 mc40b-2)
d1_repaired=never; t0=$(date +%s)
for _ in $(seq 1 $((watch / 10))); do
  [[ $(remote rewrite.bin) == "$r2" ]] && { d1_repaired=$(($(date +%s) - t0)); break; }
  sleep 10
done
sleep 20
d1_objects=$(objects rewrite.bin)
d1_peer=$(md5of "$rig/c2/data/rewrite.bin")

# ---- D2: a metadata-only change, and a real one, both made while down --------
say "stopping c1 cleanly for the metadata half"
kill -TERM "$c1" 2>/dev/null || true
for _ in $(seq 1 40); do mount | grep -q "$rig/c1/mnt" || break; sleep 1; done
fusermount3 -u "$rig/c1/mnt" 2>/dev/null || true

touch -d '2001-02-03 04:05:06' "$rig/c1/data/touched.bin"   # same bytes, new mtime
head -c $((small * 1024 * 1024)) /dev/urandom > "$rig/c1/data/edited.bin"
e2=$(md5of "$rig/c1/data/edited.bin")
say "touched.bin re-stamped (content unchanged); edited.bin rewritten out of band: $e2"

c1=$(start c1 mc40b-3)
# Wait for the control to prove the row fired at all, then keep sweeping so the
# question "does the touch come back?" has several sweeps to answer in.
for _ in $(seq 1 $((watch / 10))); do
  [[ $(remote edited.bin) == "$e2" ]] && break
  sleep 10
done
sleep 70
log=$rig/c1/logs/mc40b-3.log
d2_sweeps=$(grep -c 'reconcile complete' "$log" || true)
d2_touch_repush=$(grep -c 'repush  touched.bin' "$log" || true)
d2_edit_repush=$(grep -c 'repush  edited.bin' "$log" || true)
d2_gate3=$(grep -c 'remote already holds these bytes' "$log" || true)
d2_uploads=$(grep -c '\[sync\] upload' "$log" || true)
d2_remote_touch=$(remote touched.bin)
d2_remote_edit=$(remote edited.bin)
d2_peer_edit=$(md5of "$rig/c2/data/edited.bin")
stop_all

# ---- report ------------------------------------------------------------------
echo
echo "== MC-40 (b) against real Drive"
printf '%-36s %s\n' \
 "D1 file"                     "rewrite.bin, $mib MiB" \
 "D1 backend had taken"        "$inflight MiB of the ${mib} MiB rewrite when killed" \
 "D1 backend outlived its host" "$orphan" \
 "D1 Drive after the kill"     "$([[ $d1_kill_remote == "$r1" ]] && echo 'v1, whole' || echo "$d1_kill_remote")" \
 "D1 repaired to v2"           "$([[ $d1_repaired == never ]] && echo 'NO' || echo "yes, ${d1_repaired}s after the restart")" \
 "D1 objects at that path"     "$d1_objects (a duplicate would be 2)" \
 "D1 peer converged"           "$([[ $d1_peer == "$r2" ]] && echo yes || echo "no ($d1_peer)")" \
 "D2 sweeps"                   "$d2_sweeps" \
 "D2 repush of touched.bin"    "$d2_touch_repush (1 = proposed once and settled)" \
 "D2 gate-3 skips"             "$d2_gate3" \
 "D2 uploads in that run"      "$d2_uploads" \
 "D2 touched.bin on Drive"     "$([[ $d2_remote_touch == "$t1" ]] && echo 'unchanged' || echo "CHANGED: $d2_remote_touch")" \
 "D2 repush of edited.bin"     "$d2_edit_repush (the control: must be >= 1)" \
 "D2 edited.bin on Drive"      "$([[ $d2_remote_edit == "$e2" ]] && echo 'the out-of-band content' || echo "stale: $d2_remote_edit")" \
 "D2 peer has the new content" "$([[ $d2_peer_edit == "$e2" ]] && echo yes || echo "no ($d2_peer_edit)")"
echo
