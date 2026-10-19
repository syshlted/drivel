#!/usr/bin/env bash
# mc31-policy.sh — where does a conflict's loser go, against real Drive?
#
#   scripts/multiclient/mc31-policy.sh -d RIGDIR [-m "MODE..."] [-r REPO] [-o OUT]
#
# The live check for the conflict policy (CLAUDE.md "Conflict policy", DESIGN.md
# §6). MC-31's interleaving, made deterministic with a pause rather than a race:
# two eager clients hold one synced file, both are paused, each edits it, c1 is
# resumed and pushes, then c2 is resumed and meets c1's version as the loser of
# its own push — or c2's pull meets c1's version first and decides by mtime.
# Which of the two goes first decides the winner, and either is correct; what
# is not is the two deciding at once, which this run found on 2026-10-19 (both
# versions kept, the winner's among them, and the loser briefly at the path).
#
# MODE is a conflicts setting, `dir:PLACE` for conflicts = "dir" with that
# conflicts-dir. Default: "remote-trash dir:both dir:remote" — the three that go
# through provider.Copier on Drive, which no fake can vouch for.
#
# Per mode it asserts, from Drive itself (rig-ls.py --trashed) and the backing
# trees rather than from the logs:
#
#   winner     f.txt holds one client's edit on c1, on c2 and on the remote.
#   loser      exactly one object holding the OTHER edit exists under
#              .drivel-conflicts/ on the remote — trashed for remote-trash,
#              live for dir:* — none at any other path, and no copy of the
#              winner anywhere.
#   local      dir:both: c2 holds one local copy of the loser, and so does c1
#              once the feed delivers it. remote-trash and dir:remote: no local
#              copy anywhere.
#   copier     c2's log shows no "sending it from here instead", i.e. the copy
#              was Drive's files.copy and not a download and an upload.
set -euo pipefail

rig=; repo=; out=; modes="remote-trash dir:both dir:remote"
while getopts ":d:m:r:o:h" opt; do
  case $opt in
    d) rig=$OPTARG ;; m) modes=$OPTARG ;; r) repo=$OPTARG ;; o) out=$OPTARG ;;
    h) sed -n '2,30p' "$0"; exit 0 ;;
    *) sed -n '2,30p' "$0" >&2; exit 2 ;;
  esac
done
[[ -n $rig ]] || { echo "mc31: -d RIGDIR is required" >&2; exit 2; }
rig=$(cd "$rig" && pwd)
repo=${repo:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}
[[ -x $repo/bin/drivel ]] || { echo "mc31: no $repo/bin/drivel — pass -r REPO" >&2; exit 2; }

work=${out:-$rig/results/mc31}; mkdir -p "$work"
report=$work/report.txt; : > "$report"
fleet=(c1 c2)
declare -A pid mark
F=f.txt
BASE="base version"
C1="c1 edited this"
C2="c2 edited this, longer"
fail=0

log() { printf '%s  %s\n' "$(date +%H:%M:%S)" "$*"; }
say() { printf '%-26s %s\n' "$1" "$2" | tee -a "$report"; }
check() { # check LABEL OK DETAIL
  local verdict=PASS
  if [[ $2 != 1 ]]; then verdict=FAIL; fail=1; fi
  say "  $1" "$verdict  $3"
}
md5() { printf '%s' "$1" | md5sum | cut -d' ' -f1; }

mkcfg() { # mkcfg CLIENT SUFFIX [LINE...]
  local c=$1 suffix=$2; shift 2
  local extra=""
  if (( $# )); then extra=$(printf '%s\n' "$@"); fi
  awk -v extra="$extra" '
    /^\[mount\.provider\]/ && !ins { if (extra != "") printf "%s\n\n", extra; ins = 1 }
    { print }' "$rig/$c/config.toml" > "$rig/$c/config-$suffix.toml"
}

sock() { echo "$rig/$1/ctl/control.sock"; }

start() { # start CLIENT...
  local c
  for c in "$@"; do
    mkdir -p "$rig/$c/ctl"; chmod 700 "$rig/$c/ctl"
    nohup "$repo/bin/drivel" mount -config "$rig/$c/config-mc31.toml" \
      -control -control-socket "$(sock "$c")" >> "$rig/$c/logs/mc31.log" 2>&1 &
    pid[$c]=$!
  done
  for c in "$@"; do
    local up=0 _
    for _ in $(seq 1 90); do
      if mount | grep -q " $rig/$c/mnt " && [[ -S $(sock "$c") ]]; then up=1; break; fi
      sleep 1
    done
    [[ $up == 1 ]] || { echo "mc31: $c never mounted — see $rig/$c/logs/mc31.log" >&2; exit 1; }
  done
  sleep 3
}

stop_one() {
  local c=$1 _
  [[ -n ${pid[$c]:-} && ${pid[$c]} -gt 0 ]] || return 0
  kill -TERM "${pid[$c]}" 2>/dev/null || true
  for _ in $(seq 1 90); do
    if ! mount | grep -q " $rig/$c/mnt "; then pid[$c]=0; return 0; fi
    sleep 1
  done
  echo "mc31: $c did not unmount" >&2
}
stop_all() { local c; for c in "${fleet[@]}"; do stop_one "$c"; done; }
trap 'stop_all' EXIT

set_mark() { local c; for c in "${fleet[@]}"; do mark[$c]=$(wc -c < "$rig/$c/logs/mc31.log"); done; }
# PATTERN is an extended regex: log lines pad the operation ("upload   f.txt").
since() { tail -c "+$(( ${mark[$1]:-0} + 1 ))" "$rig/$1/logs/mc31.log" 2>/dev/null | grep -cE -- "$2" || true; }
wait_for() { # wait_for CLIENT PATTERN WANT TIMEOUT
  local c=$1 pat=$2 want=$3 t=${4:-300} _
  for _ in $(seq 1 "$t"); do
    if [[ $(since "$c" "$pat") -ge $want ]]; then return 0; fi
    sleep 1
  done
  return 1
}
wait_content() { # wait_content CLIENT WANT TIMEOUT — backing f.txt holds WANT
  local _
  for _ in $(seq 1 "$3"); do
    if [[ $(cat "$rig/$1/data/$F" 2>/dev/null) == "$2" ]]; then return 0; fi
    sleep 1
  done
  return 1
}
local_copies() { # local_copies CLIENT — files under the backing tree's conflicts folders
  find "$rig/$1/data" -path '*/.drivel-conflicts/*' -type f 2>/dev/null
}

stopped_everything() {
  local leftover _
  leftover=$(ps -eo pid,comm | awk '$2=="drivel"{print $1}')
  if [[ -n $leftover ]]; then kill -TERM $leftover 2>/dev/null || true; fi
  for _ in $(seq 1 60); do
    [[ $(mount | grep -c fuse.drivel) == 0 ]] && return 0
    sleep 1
  done
}

run_mode() { # run_mode MODE
  local mode=$1 action place lines=()
  action=${mode%%:*}
  lines+=("conflicts = \"$action\"")
  if [[ $mode == *:* ]]; then place=${mode#*:}; lines+=("conflicts-dir = \"$place\""); fi

  echo | tee -a "$report"
  say "=== conflicts = $mode ===" ""
  log "[$mode] purging the rig (folder and account trash) and local state"
  stopped_everything
  python3 "$repo/scripts/multiclient/rig-purge.py" "$rig/c1" --trash >/dev/null
  local c
  for c in "${fleet[@]}"; do
    find "$rig/$c/data" -mindepth 1 -delete 2>/dev/null || true
    rm -f "$rig/$c/drivel-state.db" "$rig/$c/drivel-index.db"
    mkdir -p "$rig/$c/data" "$rig/$c/logs"
    : > "$rig/$c/logs/mc31.log"
    mkcfg "$c" mc31 "${lines[@]}"
  done
  start "${fleet[@]}"

  set_mark
  log "[$mode] seeding $F on c1"
  printf '%s' "$BASE" > "$rig/c1/mnt/$F"
  wait_content c2 "$BASE" 180 || { log "c2 never received the seed"; fail=1; return; }
  sleep 5

  log "[$mode] pausing both, editing both"
  for c in "${fleet[@]}"; do "$repo/bin/drivel" pause -socket "$(sock "$c")" -for 10m >/dev/null; done
  printf '%s' "$C1" > "$rig/c1/mnt/$F"
  printf '%s' "$C2" > "$rig/c2/mnt/$F"
  sleep 2

  set_mark
  log "[$mode] resuming c1; it pushes first"
  "$repo/bin/drivel" resume -socket "$(sock c1)" >/dev/null
  wait_for c1 "\[sync\] upload +${F//./\\.}" 1 180 || log "WARNING: c1 never pushed"
  sleep 5
  log "[$mode] resuming c2; c1's version is now the loser"
  "$repo/bin/drivel" resume -socket "$(sock c2)" >/dev/null
  wait_for c2 "conflict ${F//./\\.}" 1 180 || log "WARNING: c2 never logged a conflict"
  sleep 15   # c2's retry, the deferred remote half, and the feed's echo of the copy
  local W L
  W=$(cat "$rig/c2/data/$F")
  if [[ $W == "$C1" ]]; then L=$C2; else L=$C1; fi
  wait_content c1 "$W" 180 || log "WARNING: c1 never settled on c2's result"
  sleep 10

  # --- assertions -------------------------------------------------------------
  local remote
  remote=$(python3 "$repo/scripts/multiclient/rig-ls.py" --trashed "$rig/c1")
  printf '%s\n' "$remote" | sed 's/^/    remote: /' | tee -a "$report"
  for c in "${fleet[@]}"; do
    grep -E '\[(sync|pull)\] conflict' "$rig/$c/logs/mc31.log" | sed "s/^/    $c: /" | tee -a "$report" || true
  done

  local hw hl win=1
  hw=$(md5 "$W"); hl=$(md5 "$L")
  [[ $W == "$C1" || $W == "$C2" ]] || win=0
  for c in "${fleet[@]}"; do [[ $(cat "$rig/$c/data/$F") == "$W" ]] || win=0; done
  grep -qE "^$hw .* $F\$" <<<"$remote" || win=0
  check winner "$win" "\"$W\" (c1=\"$(cat "$rig/c1/data/$F")\" c2=\"$(cat "$rig/c2/data/$F")\")"

  local winner_copies
  winner_copies=$(grep -E "^$hw " <<<"$remote" | grep -c '\.drivel-conflicts/' || true)
  check "no winner copy" "$([[ $winner_copies == 0 ]] && echo 1)" "copies of the winner kept as a loser: $winner_copies"

  local losers trashed_losers live_losers elsewhere
  losers=$(grep -E "^$hl " <<<"$remote" || true)
  trashed_losers=$(grep -c '\[trashed\]' <<<"$losers" || true)
  live_losers=$(grep -vc '\[trashed\]' <<<"$losers" || true)
  [[ -z $losers ]] && live_losers=0
  elsewhere=$(grep -v '\.drivel-conflicts/' <<<"$losers" | grep -c . || true)
  if [[ $action == remote-trash ]]; then
    check loser "$([[ $trashed_losers == 1 && $live_losers == 0 && $elsewhere == 0 ]] && echo 1)" \
      "trashed=$trashed_losers live=$live_losers outside-folder=$elsewhere"
  else
    check loser "$([[ $live_losers == 1 && $trashed_losers == 0 && $elsewhere == 0 ]] && echo 1)" \
      "live=$live_losers trashed=$trashed_losers outside-folder=$elsewhere"
  fi

  local l1 l2 ok=1
  if [[ $mode == dir:both ]]; then
    # c1 receives the copy through the feed, as any other remote file.
    for _ in $(seq 1 60); do [[ -n $(local_copies c1) ]] && break; sleep 1; done
  fi
  l1=$(local_copies c1); l2=$(local_copies c2)
  if [[ $mode == dir:both ]]; then
    for f in "$l1" "$l2"; do [[ -n $f && $(cat "$f") == "$L" ]] || ok=0; done
    [[ $(grep -c . <<<"$l2") == 1 ]] || ok=0
  else
    [[ -z $l1 && -z $l2 ]] || ok=0
  fi
  check local "$ok" "c1=[${l1##*/}] c2=[${l2##*/}]"

  local sent
  sent=$(grep -cF 'sending it from here instead' "$rig/c2/logs/mc31.log" || true)
  check copier "$([[ $sent == 0 ]] && echo 1)" "fallbacks to download+upload: $sent"

  stop_all
}

for m in $modes; do run_mode "$m"; done
stopped_everything
python3 "$repo/scripts/multiclient/rig-purge.py" "$rig/c1" --trash >/dev/null

echo | tee -a "$report"
say "result" "$([[ $fail == 0 ]] && echo PASS || echo FAIL)"
say "report" "$report"
exit $fail
