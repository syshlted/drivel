#!/usr/bin/env bash
# mc44-throughput.sh — MC-44's throughput half: bulk transfer over each protocol.
#
# Runs TestLiveTransferThroughput against real Drive in alternating pairs — once
# with HTTP/3 available and once with UDP/443 dropped so the HTTP/2 fallback
# carries it — and prints every run plus a median. There is no product knob for
# choosing a protocol, so the environment chooses, and nftables counters say
# which one actually carried the bytes.
#
#   DRIVEL_CREDS=rig/c1/credentials.json \
#   DRIVEL_TOKEN=rig/c1/token.json \
#   DRIVEL_LIVE_ROOT=<FOLDER_ID> \
#     scripts/multiclient/mc44-throughput.sh [-m MiB] [-n REPEATS] [-o RESULTS_DIR]
#
# Needs sudo for nft and for one write into cgroup.procs. It refuses to measure
# anything until it has proved the rule is scoped to this run's cgroup in BOTH
# directions, because this project's dev container shares the host's network
# namespace: an unscoped rule would take QUIC away from the developer's browser
# and from any real mount they have running, and a rule that silently matches
# nothing reads exactly like a transport that never falls back.
#
# Alternating, and more than once, for a measured reason: the first pair run
# found upload times repeatable to within 0.1% and *download* times varying
# twofold between consecutive runs of one protocol, so one sample of each would
# have attributed that spread to the protocol.
set -euo pipefail

mib=64
reps=3
outdir=
while getopts ":m:n:o:h" opt; do
  case $opt in
    m) mib=$OPTARG ;;
    n) reps=$OPTARG ;;
    o) outdir=$OPTARG ;;
    h) sed -n '2,25p' "$0"; exit 0 ;;
    *) sed -n '2,25p' "$0" >&2; exit 2 ;;
  esac
done

for v in DRIVEL_CREDS DRIVEL_TOKEN DRIVEL_LIVE_ROOT; do
  [[ -n ${!v:-} ]] || { echo "mc44: $v is required" >&2; exit 2; }
done
[[ ${DRIVEL_LIVE_ROOT} != root ]] || { echo "mc44: refusing to run against My Drive root" >&2; exit 2; }

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
work=$(mktemp -d)
CG=/sys/fs/cgroup/drivel-fault
NFT=$(command -v nft || echo /sbin/nft)
TABLE="inet drivel-fault"

cleanup() {
  sudo "$NFT" delete table $TABLE 2>/dev/null || true
  sudo rmdir "$CG" 2>/dev/null || true
  rm -rf "$work"
}
trap cleanup EXIT

say() { printf '\n== %s ==\n' "$*"; }

# A prebuilt binary, so the go toolchain is not inside the cgroup and not inside
# the measurement. Every run executes the identical executable.
say "building the probe"
go test -c -o "$work/mc44.test" ./internal/provider/gdrive/

# ---------------------------------------------------------------------------
# Counters in every phase so the protocol is attributed rather than assumed,
# and the drop added and removed around each HTTP/2 phase. A `counter` rule
# carries no verdict, so traffic falls through it to whatever follows.
# ---------------------------------------------------------------------------
say "installing the scoped rule"
sudo mkdir -p "$CG"
before_tables=$(sudo "$NFT" list tables | wc -l)
sudo "$NFT" add table $TABLE
sudo "$NFT" "add chain $TABLE out { type filter hook output priority 0; }"
sudo "$NFT" "add rule $TABLE out socket cgroupv2 level 1 \"drivel-fault\" udp dport 443 counter comment \"udp443\""
sudo "$NFT" "add rule $TABLE out socket cgroupv2 level 1 \"drivel-fault\" tcp dport 443 counter comment \"tcp443\""

drop_on()  { sudo "$NFT" "add rule $TABLE out socket cgroupv2 level 1 \"drivel-fault\" udp dport 443 drop"; }
drop_off() {
  local h
  h=$(sudo "$NFT" -a list chain $TABLE out | awk '/udp dport 443 drop/ {print $NF}')
  [[ -n $h ]] && sudo "$NFT" delete rule $TABLE out handle "$h"
}

# Join the cgroup in a subshell and exec, so nothing of this script's own
# traffic is ever matched and the probe is in the cgroup before its first packet.
in_cgroup() {
  (
    # Captured before the pipeline, not inside it: $BASHPID expanded in
    # `echo $BASHPID | ...` is the PID of the pipeline's own transient
    # subshell, so the write lands on a process that has already exited
    # ("No such process") and the caller silently runs unscoped.
    local me=$BASHPID
    echo "$me" | sudo tee "$CG/cgroup.procs" >/dev/null
    exec "$@"
  )
}

counters() {
  sudo "$NFT" -a list chain $TABLE out | awk '
    /comment "udp443"/ { for (i=1;i<=NF;i++) if ($i=="bytes") q=$(i+1) }
    /comment "tcp443"/ { for (i=1;i<=NF;i++) if ($i=="bytes") t=$(i+1) }
    END { printf "%s %s\n", q+0, t+0 }'
}
# `reset counters` resets named counter *objects* and leaves an anonymous rule
# counter untouched — verified, after it silently carried one phase's TCP total
# into the next and made HTTP/3 look as though it had sent everything twice.
reset_counters() { sudo "$NFT" reset rules table $TABLE >/dev/null; }

# ---------------------------------------------------------------------------
# Prove the scope in both directions. Fail closed: if the drop does not bite
# inside the cgroup, every "HTTP/2" number below would silently be HTTP/3.
# ---------------------------------------------------------------------------
say "proving the scope"
drop_on
probe_h3() { curl -sS --http3-only -o /dev/null -w '%{http_version}' --max-time 20 https://www.googleapis.com/ 2>/dev/null || true; }
outside=$(probe_h3)
inside=$(in_cgroup bash -c "$(declare -f probe_h3); probe_h3")
echo "  outside the cgroup: HTTP/3 probe -> ${outside:-none}   (want 3)"
echo "  inside  the cgroup: HTTP/3 probe -> ${inside:-none}   (want anything but 3)"
[[ $outside == 3 ]] || { echo "mc44: HTTP/3 does not work here even unblocked; nothing to compare" >&2; exit 1; }
[[ $inside != 3 ]]  || { echo "mc44: the drop rule does not match inside the cgroup; refusing to measure" >&2; exit 1; }
drop_off
echo "  host nft tables: $before_tables before, $(sudo "$NFT" list tables | wc -l) now (one added, ours)"

# ---------------------------------------------------------------------------
# The runs. Alternating, so a drift in Drive's own performance across the
# session cannot land entirely on one protocol.
# ---------------------------------------------------------------------------
run_one() { # proto, iteration
  local proto=$1 i=$2 log="$work/$1.$2.log" q t
  reset_counters
  [[ $proto == h2 ]] && drop_on
  in_cgroup env \
    DRIVEL_LIVE_RIG=1 DRIVEL_LIVE_MB="$mib" \
    DRIVEL_CREDS="$DRIVEL_CREDS" DRIVEL_TOKEN="$DRIVEL_TOKEN" DRIVEL_LIVE_ROOT="$DRIVEL_LIVE_ROOT" \
    "$work/mc44.test" -test.run TestLiveTransferThroughput -test.v -test.timeout=40m \
    > "$log" 2>&1 || { echo "  $proto run $i FAILED:"; grep -E 'FAIL|\.go:' "$log" | head -5; }
  [[ $proto == h2 ]] && drop_off
  read -r q t < <(counters)
  printf '  %s run %d  up:   %-46s  udp443 %6.1f MiB  tcp443 %6.1f MiB\n' "$proto" "$i" \
    "$(grep -o 'MC44 upload: .*' "$log" | sed 's/MC44 upload: //')" \
    "$(awk -v b="$q" 'BEGIN{print b/1048576}')" "$(awk -v b="$t" 'BEGIN{print b/1048576}')"
  printf '  %s run %d  down: %-46s\n' "$proto" "$i" \
    "$(grep -o 'MC44 download: .*' "$log" | sed 's/MC44 download: //')"
  echo "$proto $i $q $t" >> "$work/attrib.txt"
}

say "$reps alternating pairs, $mib MiB each way"
for ((i = 1; i <= reps; i++)); do
  run_one h2 "$i"
  run_one h3 "$i"
done

# ---------------------------------------------------------------------------
# The comparison. Median, because with this few samples a mean is one outlier
# away from being the outlier.
# ---------------------------------------------------------------------------
# Extraction is sed rather than awk's match(): the three-argument form that
# fills a capture array is a GNU extension, and this ran on an awk without it.
med() { # proto, leg, 'rate' | 'cpu'
  local expr='s|.*= \([0-9.]*\) MiB/s.*|\1|p'
  [[ $3 == cpu ]] && expr='s|.*(\([0-9.]*\) ms/MiB).*|\1|p'
  grep -ho "MC44 $2: .*" "$work/$1".*.log | sed -n "$expr" | sort -n | \
    awk '{ v[NR] = $1 } END { if (NR == 0) { print "n/a"; exit }
      printf "%.2f", (NR % 2 ? v[(NR + 1) / 2] : (v[NR / 2] + v[NR / 2 + 1]) / 2) }'
}
say "MC-44 medians of $reps, $mib MiB each way"
printf '%-10s %-18s %-18s\n' "" "HTTP/2" "HTTP/3"
printf '%-10s %-18s %-18s\n' "upload"   "$(med h2 upload rate) MiB/s"    "$(med h3 upload rate) MiB/s"
printf '%-10s %-18s %-18s\n' "download" "$(med h2 download rate) MiB/s"  "$(med h3 download rate) MiB/s"
printf '%-10s %-18s %-18s\n' "cpu up"   "$(med h2 upload cpu) ms/MiB"    "$(med h3 upload cpu) ms/MiB"
printf '%-10s %-18s %-18s\n' "cpu down" "$(med h2 download cpu) ms/MiB"  "$(med h3 download cpu) ms/MiB"
echo
echo "Attribution is of EGRESS only — the counters sit on the output hook — so a"
echo "run's figure is ~the payload for the upload leg and ~nothing for the"
echo "download leg. An HTTP/2 run must read ~0 on udp443; an HTTP/3 run must read"
echo "~0 on tcp443 beyond the token refresh."

if [[ -n $outdir ]]; then
  mkdir -p "$outdir"
  cp "$work"/*.log "$work/attrib.txt" "$outdir/" 2>/dev/null || true
  { echo "commit: $(git -C "$repo" rev-parse HEAD)"; echo "mib: $mib"; echo "reps: $reps"; } > "$outdir/meta.txt"
  echo "logs and meta in $outdir"
fi
