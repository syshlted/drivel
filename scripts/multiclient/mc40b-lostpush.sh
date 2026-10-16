#!/usr/bin/env bash
# mc40b-lostpush.sh — MC-40 (b): a push LOST to a crash, and whether a sweep finds it.
#
#   scripts/multiclient/mc40b-lostpush.sh -d RIGDIR -b BINARY [-B RESTARTBIN]
#                                          [-m MiB] [-f PCT] [-w SECS]
#
# Seeds one file, lets the fleet converge, rewrites it, kills the writer with
# SIGKILL while that rewrite is in flight, restarts it, and reports whether the
# divergence is ever repaired. Run it twice with -b pointing at two builds: that
# A/B is the measurement, because the shape it looks for is a silence, and a
# silence proves nothing on its own.
#
# It wants the SFTP rig from scripts/multiclient/README.md §MC-47 rather than
# Drive, for the reason that section gives — two real processes, no quota, no
# account — plus one this case adds: the SFTP root is an ordinary directory, so
# "what the remote holds" is a md5sum rather than an API call, and the oracle
# cannot itself be the thing that is broken.
#
# Expected rig layout, all of it on tmpfs (the kill has to land inside an upload
# window, and on a full spinning volume that window moves by a factor of 30):
#
#   RIGDIR/srv                     the SFTP root — read directly, as the oracle
#   RIGDIR/c1/{data,mnt,state}     the client that gets killed
#   RIGDIR/c1/config-seed.toml     ...with a sweep interval long enough that no
#                                  sweep races the push being interrupted
#   RIGDIR/c1/config-run.toml      ...and a short one for after the restart
#   RIGDIR/c2/{data,mnt,state}     a peer, to show the repair reaches the fleet
#   RIGDIR/c2/config.toml          short interval; it is never killed
set -euo pipefail

rig=; bin=; restart=; mib=256; pct=25; watch=150
while getopts ":d:b:B:m:f:w:h" opt; do
  case $opt in
    d) rig=$OPTARG ;; b) bin=$OPTARG ;; B) restart=$OPTARG ;; m) mib=$OPTARG ;;
    f) pct=$OPTARG ;; w) watch=$OPTARG ;;
    h) sed -n '2,29p' "$0"; exit 0 ;;
    *) sed -n '2,29p' "$0" >&2; exit 2 ;;
  esac
done
[[ -n $rig && -n $bin ]] || { echo "mc40b: -d RIGDIR and -b BINARY are required" >&2; exit 2; }
# -B is the build that comes back UP, which is the realistic field shape: the
# crash happens on whatever was installed, and the repair is whatever the user
# upgraded to. It defaults to -b, so one binary means the plain A/B.
restart=${restart:-$bin}

file=rewrite.bin
bytes=$((mib * 1024 * 1024))

# Stopping is by process *name*. A pattern that could match this script's own
# command line kills the run instead of the client — see the note in
# scripts/multiclient/README.md.
stop_all() {
  local pids
  pids=$(ps -eo pid,comm | awk '$2=="drivel"{print $1}')
  [[ -n $pids ]] && kill -TERM $pids 2>/dev/null || true
  for _ in $(seq 1 30); do
    [[ $(mount | grep -c fuse.drivel) == 0 ]] && return
    sleep 1
  done
  for m in "$rig"/c*/mnt; do fusermount3 -u "$m" 2>/dev/null || true; done
}

md5of() { [[ -e $1 ]] && md5sum < "$1" | cut -d' ' -f1 || echo "ABSENT"; }

say() { printf '%s  %s\n' "$(date -u +%H:%M:%S)" "$*"; }

stop_all
say "purging the rig"
find "$rig/srv" -mindepth 1 -delete 2>/dev/null || true
for c in c1 c2; do
  find "$rig/$c/data" -mindepth 1 -delete 2>/dev/null || true
  rm -rf "$rig/$c/state" "$rig/$c/log"
  mkdir -p "$rig/$c/data" "$rig/$c/state" "$rig/$c/log"
done

say "starting c1 (seed config) and c2"
"$bin" mount -config "$rig/c1/config-seed.toml" > "$rig/c1/log/seed.log" 2>&1 &
c1=$!
"$bin" mount -config "$rig/c2/config.toml" > "$rig/c2/log/run.log" 2>&1 &
sleep 6

# ---- 1. seed, and converge on it -------------------------------------------
head -c "$bytes" /dev/urandom > "$rig/c1/mnt/$file"
v1=$(md5of "$rig/c1/data/$file")
say "v1 written through c1's mount: $v1"
for _ in $(seq 1 120); do
  [[ $(md5of "$rig/srv/$file") == "$v1" && $(md5of "$rig/c2/data/$file") == "$v1" ]] && break
  sleep 2
done
[[ $(md5of "$rig/srv/$file") == "$v1" ]] || { echo "mc40b: v1 never reached the server" >&2; stop_all; exit 1; }
[[ $(md5of "$rig/c2/data/$file") == "$v1" ]] || { echo "mc40b: v1 never reached c2" >&2; stop_all; exit 1; }
say "v1 on the server and on c2 — the path now has a baseline on c1"

# ---- 2. rewrite it, and kill c1 inside the upload ---------------------------
# The rewrite is complete locally before any of it is sent: OpWrite is emitted
# on close, so this is not a race with the writer, only with the uploader.
head -c "$bytes" /dev/urandom > "$rig/c1/mnt/$file"
v2=$(md5of "$rig/c1/data/$file")
say "v2 written through c1's mount: $v2"

# Trigger on the temporary EXISTING, then let a fraction of it through: the
# destination path never holds a partial file (M18 rule 7), so the temporary is
# the only visible evidence that an upload is in flight.
want=$((bytes * pct / 100))
tmp=; reached=0
for _ in $(seq 1 2000); do
  tmp=$(find "$rig/srv" -name '.drivel-upload.*' -printf '%p\n' -quit 2>/dev/null || true)
  [[ -n $tmp ]] && break
  sleep 0.005
done
[[ -n $tmp ]] || { echo "mc40b: no upload temporary appeared — nothing to interrupt" >&2; stop_all; exit 1; }
for _ in $(seq 1 2000); do
  reached=$(stat -c %s "$tmp" 2>/dev/null || echo 0)
  [[ $reached -ge $want ]] && break
  [[ -e $tmp ]] || break          # the upload finished; kill anyway and say so
  sleep 0.01
done
# c1's backend, identified by PARENTAGE and not by a pattern: every live client
# has a `plugin-serve` of its own, so a count would report the peer's healthy one
# — and a pattern over command lines also matches the shell running this script.
child=$(ps -eo pid,ppid,args | awk -v p="$c1" '$2==p && /plugin-serve/ {print $1}' | head -1)
kill -9 "$c1" 2>/dev/null || true
say "SIGKILL to c1 (backend was pid ${child:-none}) with $reached of $bytes B in the temporary ($((reached * 100 / bytes))%)"
sleep 3

orphan=no
if [[ -n $child ]] && kill -0 "$child" 2>/dev/null; then orphan="YES (pid $child)"; fi
after_kill_srv=$(md5of "$rig/srv/$file")
after_kill_c1=$(md5of "$rig/c1/data/$file")
fusermount3 -u "$rig/c1/mnt" 2>/dev/null || true
say "after the kill: c1 local=$after_kill_c1  server=$after_kill_srv"

# ---- 3. restart, and watch for a repair ------------------------------------
say "restarting c1 (run config) and watching for ${watch}s"
"$restart" mount -config "$rig/c1/config-run.toml" > "$rig/c1/log/run.log" 2>&1 &
t0=$(date +%s)
repaired=never
for _ in $(seq 1 $((watch / 2))); do
  if [[ $(md5of "$rig/srv/$file") == "$v2" ]]; then repaired=$(( $(date +%s) - t0 )); break; fi
  sleep 2
done
# However it ended, let the peer have a chance at it before reading c2.
sleep 20
fanout=$(md5of "$rig/c2/data/$file")
stop_all

# ---- 4. report --------------------------------------------------------------
sweeps=$(grep -c 'reconcile complete' "$rig/c1/log/run.log" || true)
rp_lost=$(grep -c 'repush.*never landed' "$rig/c1/log/run.log" || true)
rp_aband=$(grep -c 'repush.*was abandoned' "$rig/c1/log/run.log" || true)
c2_rp=$(grep -c '\[sweep\] repush' "$rig/c2/log/run.log" || true)
# The crash's temporary is never reclaimed: Enumerate skips the prefix so it is
# invisible to every client, and nothing sweeps it. MC-40's open follow-up.
stranded=$(find "$rig/srv" -name '.drivel-upload.*' | wc -l)
echo
echo "== MC-40 (b): crashed on $bin"
if [[ $restart != "$bin" ]]; then echo "            restarted on $restart"; fi
printf '%-34s %s\n' \
  "file"                       "$file, $mib MiB" \
  "killed at"                  "$reached B of $bytes ($((reached * 100 / bytes))% of the upload)" \
  "c1's backend outlived it"   "$orphan" \
  "c1 local after the kill"    "$([[ $after_kill_c1 == "$v2" ]] && echo "v2 (the rewrite)" || echo "$after_kill_c1")" \
  "server after the kill"      "$([[ $after_kill_srv == "$v1" ]] && echo "v1 (previous content, whole)" || echo "$after_kill_srv")" \
  "sweeps after the restart"   "$sweeps" \
  "repush (lost-push row)"     "$rp_lost" \
  "repush (repair bucket)"     "$rp_aband" \
  "repush on the peer"         "$c2_rp" \
  "stranded upload temporaries" "$stranded" \
  "server repaired to v2"      "$([[ $repaired == never ]] && echo "NO — still diverged after $sweeps sweeps" || echo "yes, ${repaired}s after the restart")" \
  "peer converged on v2"       "$([[ $fanout == "$v2" ]] && echo "yes" || echo "no ($fanout)")"
echo
