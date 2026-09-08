#!/usr/bin/env bash
# scenario.sh — one isolated multi-client rig per test scenario.
#
#   ./scenario.sh new NAME -r FOLDER_ID [-p overlay|bulk] [-n 3] [-w WHY]
#   ./scenario.sh start NAME
#   ./scenario.sh stop  NAME
#   ./scenario.sh list
#
# A scenario is a whole rig — N clients, an oracle, its own Drive folder — that
# exists for one group of cases and is then left alone. The alternative is to
# keep reusing one rig, and that costs more than it looks: the log arithmetic a
# case depends on ("10000 files in, how many upload lines out?") stops being
# subtraction the moment the folder already holds someone else's objects, and
# Store.Remove on Drive is permanent, so clearing a folder to get a clean start
# is 1000 irreversible deletes to save nothing.
#
# Each scenario therefore gets a NEW Drive folder, created by a human, per
# safety precondition 1 in docs/dev/multiclient-test-plan.md §2.1. This script will
# not create one: -drive-root root is the documented catastrophic configuration
# and nothing here should be able to reach for it by accident.
#
# Placement is a first-class choice because the hardware is not uniform:
#
#   overlay  the container's root filesystem — fast, and only ~40 GB free.
#            Anything whose result is a latency, a throughput or a convergence
#            time belongs here, because that is where the MC-01/02/03 baselines
#            were measured and a number is only comparable to numbers taken on
#            the same disk.
#   bulk     the external disk at $BULK_ROOT — large, and as of this writing a
#            magnetic USB drive doing 61 MB/s sequential and *19 synchronous
#            4 KiB writes per second*. Capacity-bound cases only (MC-10's 5 GiB
#            trees, MC-52's 100k files). Treat wall-clock from a bulk scenario
#            as a property of the disk, not of drivel.
#
# The disk each scenario actually sat on is recorded in its SCENARIO.md at
# creation time, because that drive is expected to be replaced by an SSD and a
# result that does not say which disk produced it becomes uninterpretable the
# moment the hardware changes.
set -euo pipefail

RIG_ROOT=${DRIVEL_RIG_ROOT:-$HOME/drivel-rig}
BULK_ROOT=${DRIVEL_BULK_ROOT:-$HOME/extra_space/drivel-rig}
SCEN_HOME=$RIG_ROOT/scenarios
REGISTRY=$SCEN_HOME/REGISTRY.md
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo=$(cd "$here/../.." && pwd)

usage() { sed -n '2,6p' "$0"; exit "${1:-0}"; }
die()   { echo "scenario: $*" >&2; exit 1; }

# scenario_dir NAME — where a scenario lives, whichever root holds it. A scenario
# is one physical directory and never a symlink into another: drivel's own
# cardinal rule is about backing-tree and mountpoint overlap, app.Validate
# compares paths to enforce it, and a symlinked path is exactly how you would
# defeat that guard without noticing.
scenario_dir() {
  local n=$1
  [[ -d $SCEN_HOME/$n ]] && { echo "$SCEN_HOME/$n"; return; }
  [[ -d $BULK_ROOT/scenarios/$n ]] && { echo "$BULK_ROOT/scenarios/$n"; return; }
  return 1
}

cmd_new() {
  local name=$1; shift
  local folder= placement=overlay n=3 why=
  while getopts ":r:p:n:w:" opt; do
    case $opt in
      r) folder=$OPTARG ;;
      p) placement=$OPTARG ;;
      n) n=$OPTARG ;;
      w) why=$OPTARG ;;
      *) usage 2 ;;
    esac
  done

  [[ -n $folder ]] || die "new: -r FOLDER_ID is required (create the folder yourself; see §2.1)"
  [[ $folder == root ]] && die "new: refusing 'root' — that is the whole of My Drive"
  scenario_dir "$name" >/dev/null 2>&1 && die "new: scenario '$name' already exists at $(scenario_dir "$name")"

  local base
  case $placement in
    overlay) base=$SCEN_HOME ;;
    bulk)    base=$BULK_ROOT/scenarios ;;
    *)       die "new: placement must be 'overlay' or 'bulk', not '$placement'" ;;
  esac
  mkdir -p "$base"
  local dir=$base/$name

  # Tokens: one per client, never shared, per §2.1 precondition 4. Copy them from
  # the original rig when it has them — the account is the same, and a refresh
  # rewrites each copy independently.
  local creds=$RIG_ROOT/credentials.json
  [[ -f $creds ]] || die "new: no credentials at $creds"

  "$here/rig-init.sh" -r "$folder" -c "$creds" -n "$n" -d "$dir" >/dev/null
  local i
  for i in $(seq 1 "$n"); do
    if [[ -f $RIG_ROOT/rig/c$i/token.json ]]; then
      cp "$RIG_ROOT/rig/c$i/token.json" "$dir/c$i/token.json"
    fi
  done
  [[ -f $RIG_ROOT/rig/c1/token.json ]] && cp "$RIG_ROOT/rig/c1/token.json" "$dir/oracle/token.json"

  # Record the disk in the scenario rather than in a person's memory. This is the
  # line that stops a magnetic-disk result being read as an SSD one later.
  local dev fstype
  dev=$(findmnt -T "$dir" -no SOURCE 2>/dev/null || echo unknown)
  fstype=$(findmnt -T "$dir" -no FSTYPE 2>/dev/null || echo unknown)

  cat > "$dir/SCENARIO.md" <<MD
# Scenario: $name

${why:-(no purpose recorded — pass -w next time)}

| | |
|---|---|
| Created | $(date -Is) |
| Drivel commit | $(cd "$repo" && git rev-parse --short HEAD) |
| Drive folder | \`$folder\` |
| Clients | $n + oracle |
| Placement | $placement |
| Path | \`$dir\` |
| Device | \`$dev\` ($fstype) |

## Hardware caveat

Every wall-clock number from this scenario belongs to the device above. The bulk
disk is being replaced by an SSD, so a duration recorded here is **not**
comparable to one recorded after that swap, and a bulk-disk duration was never
comparable to an overlay one. Sizes, byte counts, hashes and log-line counts are
hardware-independent and travel fine.

## Knobs

Defaults from rig-init.sh: \`sweep-interval = "0"\`, \`max-deletes = 100\`.
Override per case in each client's config.toml and record the override in the
result, per §2.3.

## Runbook

    scripts/multiclient/scenario.sh start $name
    scripts/multiclient/scenario.sh stop  $name

Results go in \`$dir/results/<case>/<timestamp>/\` with the logs and the commit
SHA, per §4.11.
MD

  [[ -f $REGISTRY ]] || cat > "$REGISTRY" <<'MD'
# Scenario registry

One rig per scenario, each with its own Drive folder. See
`docs/dev/multiclient-test-plan.md` §2.2 and `scripts/multiclient/scenario.sh`.

Never run two fleets at once: they share a Drive account, so the second one's
traffic lands in the first one's quota and request-rate measurements.

| Scenario | Placement | Drive folder | Created | Purpose |
|---|---|---|---|---|
MD
  printf '| `%s` | %s | `%s` | %s | %s |\n' \
    "$name" "$placement" "$folder" "$(date +%F)" "${why:-—}" >> "$REGISTRY"

  echo "scenario '$name' created at $dir"
  echo "  folder $folder, $n clients + oracle, $placement ($dev, $fstype)"
  [[ -f $dir/c1/token.json ]] || echo "  NOTE: no tokens copied — run 'drivel login -account cN' per client"
}

# fleet_procs prints "PID cmdline" for every live drivel mount, and only for the
# binary itself. A client is started as a pipeline into ts, so the shell that owns
# the pipeline carries the same text in its own cmdline and pgrep matches it too —
# counting it would double every client in the listing, and signalling it in stop
# would hit the pipeline parent instead of the process that needs to unmount.
fleet_procs() {
  pgrep -af 'drivel mount' 2>/dev/null | grep -v ' -c ' || true
}

# running_fleet prints the config path of every live client. Used as a guard: two
# fleets against one Drive account contend for the same quota, so a request-rate
# or 403 measurement taken while another scenario is live is not measuring what it
# says it is.
# The trailing `|| true` is load-bearing under `set -euo pipefail`: with no fleet
# running there is nothing on stdin, grep exits 1, pipefail propagates it and the
# script dies at the guard that was supposed to say "all clear". It fails only in
# the case the guard exists to permit, which is why it survived the first test —
# a fleet was running at the time.
running_fleet() {
  fleet_procs | grep -o -- '-config [^ ]*' | awk '{print $2}' | sort -u || true
}

cmd_start() {
  local name=$1
  local dir; dir=$(scenario_dir "$name") || die "start: no scenario '$name'"

  local live; live=$(running_fleet)
  if [[ -n $live ]]; then
    echo "scenario: drivel is already running against:" >&2
    echo "$live" | sed 's/^/  /' >&2
    die "start: stop the other fleet first — two fleets share one Drive quota"
  fi

  [[ -x $repo/bin/drivel ]] || die "start: no binary at $repo/bin/drivel (make build)"

  local port=6061 c
  for c in "$dir"/c*/; do
    c=${c%/}
    local n; n=$(basename "$c")
    [[ -f $c/config.toml ]] || continue
    mkdir -p "$c/logs"
    [[ -f $c/logs/run.log ]] && mv "$c/logs/run.log" "$c/logs/run-prev.log"
    # ts timestamps every line; the convergence detector and every latency number
    # in §2.7 are computed from those stamps, so a log without them is unusable.
    ( cd "$repo" && exec ./bin/drivel mount -config "$c/config.toml" -pprof "localhost:$port" ) 2>&1 \
      | ts '%.T' > "$c/logs/run.log" &
    echo "  $n started, pprof localhost:$port"
    port=$((port + 1))
  done
  echo "scenario '$name' started from $dir"
}

cmd_stop() {
  local name=$1
  local dir; dir=$(scenario_dir "$name") || die "stop: no scenario '$name'"
  local pids
  pids=$(fleet_procs | grep -F "$dir" | awk '{print $1}' || true)
  [[ -z $pids ]] && { echo "scenario '$name' is not running"; return; }
  # SIGINT, not SIGKILL: the mount unmounts on it, which makes server.Wait return
  # and lets the three-phase Close run its bounded drain. Killing skips the drain
  # and leaves a stale mountpoint behind.
  echo "$pids" | xargs -r kill -INT
  local i
  for i in $(seq 1 30); do
    fleet_procs | grep -qF "$dir" || { echo "scenario '$name' stopped"; return; }
    sleep 1
  done
  echo "scenario: still running after 30s; mounts may need 'fusermount3 -u'" >&2
}

cmd_list() {
  [[ -f $REGISTRY ]] || { echo "no scenarios yet"; return; }
  cat "$REGISTRY"
  local live; live=$(running_fleet)
  echo
  if [[ -n $live ]]; then
    echo "RUNNING:"; echo "$live" | sed 's/^/  /'
  else
    echo "RUNNING: nothing"
  fi
}

[[ $# -ge 1 ]] || usage 2
sub=$1; shift
case $sub in
  new)   [[ $# -ge 1 ]] || usage 2; name=$1; shift; cmd_new "$name" "$@" ;;
  start) [[ $# -eq 1 ]] || usage 2; cmd_start "$1" ;;
  stop)  [[ $# -eq 1 ]] || usage 2; cmd_stop "$1" ;;
  list)  cmd_list ;;
  -h|help) usage 0 ;;
  *)     usage 2 ;;
esac
