# Multi-client rig (Tier B)

Three real `drivel` processes against one Drive folder. The plan these implement
is [`docs/multiclient-test-plan.md`](../../docs/dev/multiclient-test-plan.md); the
deterministic half of it (Tier A) is `TestFleet*` in `internal/app`, which needs
no network and runs under `-race` with the rest of the suite.

## Before the first run

1. **A dedicated Drive folder, and its ID** — not `root`. `-drive-root root` maps
   the mount to the whole of My Drive.
2. **`Store.Remove` trashes by default.** Deletions here are recoverable from the
   web UI for 30 days — but the cases delete thousands of files at a time and a
   trashed file still counts against the quota, so empty the trash between runs.
   With `delete = "permanent"` they are gone for good instead.
3. **A throwaway Google account.** The delete and conflict cases are designed to
   destroy data.
4. **One token per client — tidiness, not safety.** This item used to claim
   `token.json` is rewritten on refresh so that clients sharing a path race on
   that write. MC-45 disproved it: nothing in a *mount* ever writes a token
   file. `gauth.SaveToken` has exactly one caller, `drivel login`, and
   `TestOnlyLoginWritesTheOAuthToken` is a tree-wide grep that keeps it that
   way. Keep them separate anyway, for reasons that survive: a revoked or
   expired token then takes down one client instead of the fleet, and a
   per-client file makes it obvious which client authenticated as whom. See the
   plan's §2.1.
5. **Check the account before trusting the rig.** `TestLiveRigPreflight` is the
   tree's one read-only live probe: it reports which account a token belongs to,
   how much quota is left (MC-10 parks 6 GiB of a free account's 15 GB) and
   whether the `drivel-rig` folder still exists. It creates nothing, so it is
   safe against any account.

   ```sh
   DRIVEL_LIVE_RIG=1 \
     DRIVEL_CREDS=rig/c1/credentials.json \
     DRIVEL_TOKEN=rig/c1/token.json \
     go test -count=1 -v -run TestLiveRigPreflight ./internal/provider/gdrive/
   ```

   Run it once per client, because it is also how an expired token announces
   itself. A Cloud project left in **Testing** publishing status expires refresh
   tokens after seven days, so a rig that worked last week fails with
   `oauth2: "invalid_grant"` and nothing else wrong with it — check that before
   looking for a drivel bug.

## Laying it out

```sh
make build
scripts/multiclient/rig-init.sh -r <FOLDER_ID> -c ~/credentials.json -n 3
```

That writes `rig/c1…c3` (mountpoint, backing dir, state DB, index DB, config,
logs) plus `rig/oracle`. Then give each client a token of its own, by pointing
`login` straight at the rig's files:

```sh
./bin/drivel login -scope drive \
  -credentials rig/c1/credentials.json \
  -token       rig/c1/token.json
```

**Not `-account c1`.** That writes the token under `$XDG_CONFIG_HOME/drivel/c1`
instead, and appends an `[account.c1]` block and a `[[mount]]` at `~/drive-c1`
to your own config file — none of which the rig reads. One run per client, each
forcing a fresh consent, against **one** Google account: the clients sync one
folder, so quota contention (MC-36, MC-50/51) and the same-name-sibling case
(MC-30) are properties of a single user's Drive. `rig-init.sh -t` seeds every client
from one token, which is safe for the reason rule 4 above gives and is the quick
way to stand up a wide fleet — MC-51 needs five clients and nobody wants five
browser consents — at the cost of one revocation taking all of them down.

Start each client in its own terminal:

```sh
./bin/drivel mount -config rig/c1/config.toml 2>&1 | ts > rig/c1/logs/run.log
```

`ts` (moreutils) timestamps every line; the convergence and latency numbers read
those timestamps.

## When the rig has gone stale

A rig is two halves — a Drive folder and four local state directories — and
either can outlive the other. The folder is the one that disappears: a case
deletes it, or the web UI does, after which every `config.toml` still names an
ID that 404s and the preflight above reports `no "drivel-rig" folder at the top
level`. Create a replacement (web UI, or any API client — the preflight
deliberately will not), then fix **both** halves.

**Re-pinning the new ID is not enough on its own.** Each client's state DB holds
§4 echo records — which are also M7b's delete baseline — for paths whose remote
objects are gone, so against a fresh empty folder every one of them is the
sweep's *deleted remotely* row. A thousand stale baselines against
`max-deletes = 100` means the pass is **abandoned** rather than trimmed, and that
guard is deliberate: a count that large means the premise is broken, not that
there are a thousand real deletions. So the rig sits refusing to reconcile, and
with `sweep-interval = 0` nothing retries. Reset the local half:

```sh
mount | grep fuse.drivel          # must be empty before you delete anything
for c in c1 c2 c3 oracle; do
  find rig/$c/data rig/$c/logs -mindepth 1 -delete
  rm -f rig/$c/drivel-state.db rig/$c/drivel-index.db
done
sed -i "s|<OLD_ID>|<NEW_ID>|g" rig/*/config.toml
```

Unmount first for a reason that is not tidiness: emptying a backing tree under
a live mount emits an `OpUnlink` per file and an `OpRmdir` per directory, and
the engine pushes every one of them to the folder you were trying to keep. Keep
the `token.json` files — they are credentials rather than state, and
re-authorising costs a browser round trip per client.

## Running a case

```sh
# MC-11: 10k small files from c1, and how long the fleet takes to agree
scripts/multiclient/workload.sh many rig/c1/mnt 10000
scripts/multiclient/converge.sh -q -t 3600 rig/c1/data rig/c2/data rig/c3/data

# MC-20: 4 KiB into the middle of a 1 GiB file — one upload and two downloads of
# the whole file, which is the number the case exists to produce
scripts/multiclient/workload.sh large   rig/c1/mnt 1024
scripts/multiclient/converge.sh -t 3600 rig/c1/data rig/c2/data rig/c3/data
scripts/multiclient/workload.sh partial rig/c1/mnt 1024
scripts/multiclient/converge.sh -t 3600 rig/c1/data rig/c2/data rig/c3/data
scripts/multiclient/logcount.sh rig/c*/logs/run.log
```

Resource cost, sampled alongside:

```sh
args=()
for n in 1 2 3; do
  host=$(pgrep -f "config rig/c$n")
  args+=("$host:c$n" "$(pgrep -P "$host" -f plugin-serve):c$n-backend")
done
scripts/multiclient/sample.sh -o rig/metrics/mc20.csv "${args[@]}"
```

**Sample the backend child as well as the host, or the numbers are wrong.** Since
M9 a client is two processes: the host owns the mount, the engine and the state DB,
and `drivel plugin-serve <kind>` owns the path index. Disk and CPU for "a client"
is the sum. MC-11's write-amplification figure was recorded before M9, when both
bbolt stores were in one process, so it is not comparable to a host-only sample
taken today (see the plan, §4.17).

## Group IV and VI: the case drivers

Three cases have drivers of their own, because each needs a fleet reshaped
around it rather than a workload poured into a fixed one:

```sh
scripts/multiclient/mc50-steady-state.sh -d rig -n 300 -m 5 -o rig/results/mc50
scripts/multiclient/mc51-scaling.sh      -d rig -N "1 2 3 5" -k 1024
scripts/multiclient/mc44-throughput.sh   -m 64 -n 5          # see MC-44 below
scripts/multiclient/rig-purge.py         rig/c1              # empty the remote
```

`mc50` drives idle / light / heavy cells against a fleet that is **already
running with one `-pprof` port per client** (6061 upward, in client order) and
reports through `mc50-report.sh`, which is split out so a run can be
re-reported without being re-run. `mc51` drives the fleet *size* instead, so it
starts and stops clients itself and needs them stopped to begin with.
`rig-purge.py` reads the folder to empty out of a client's own `config.toml`
and refuses a root of `root`, so it hardcodes neither a path nor an account.

Two things these drivers learned the hard way, both worth copying into the next
one. **Count after a settle, never at the moment a condition goes true**: MC-51
reported zero uploads at N=1 because convergence is trivially true the instant
the file is written and the debounce window alone is 300ms, and three downloads
at N=2 because the cold-start enumeration was still landing. And **a bare `wait`
also waits for your samplers** — MC-50's driver hung on it, because the sampler
and the goroutine scraper never exit; wait for the workload PIDs by name.

**Stopping clients is by process name, never by a command-line pattern.**
`pgrep -f 'drivel mount -config'` and `ps | awk '/drivel mount/'` both match the
shell that is running them, so a kill loop built from either one kills the
session. Use `ps -eo pid,comm | awk '$2=="drivel"'`: a shell's `comm` is never
`drivel`. (A plugin child's `comm` is `exe`, since it is launched as
`/proc/self/exe` — M9 rule 10 — so `ps -eo comm | grep -c '^exe$'` is how you
check none was left behind.)

## Checking against Drive, not against each other

Three clients agreeing proves they converged, not that they converged on what
Drive holds. The oracle is a cold client that reconstructs the tree from
`Enumerate` alone, so its backing dir *is* the remote's manifest:

```sh
rm -rf rig/oracle/data/* rig/oracle/drivel-state.db rig/oracle/drivel-index.db
./bin/drivel mount -config rig/oracle/config.toml 2>&1 | ts > rig/oracle/logs/run.log &
scripts/multiclient/converge.sh -q rig/c1/data rig/oracle/data
```

For a second opinion that is not drivel's own code — and for MC-30, where by
construction no path-addressed client can see the duplicates — use
`rclone lsjson --hash -R`.

## What convergence does and does not mean

`converge.sh` compares path, size, content hash and kind — deliberately **not**
mtime. A drivel fleet never agrees on mtime and is not trying to: a downloaded
file carries the remote's `modifiedTime`, but the peer that originated the write
keeps its own local write time and nothing writes the provider's stamp back down
over it. So for every file, exactly one client's mtime differs from everyone
else's, forever. `manifest.sh` still reports mtime — it is diagnostic, and MC-13
wants it — but comparing it would mean no case ever converges.

## What the scripts do not measure

- **Goroutines and heap.** `sample.sh` gets RSS, CPU, disk I/O, fds and threads
  from `/proc`, which answers "did it grow" and cannot answer "what grew". Start
  each client with its own `-pprof` port for that:

  ```sh
  drivel mount -config rig/c1/config.toml -pprof localhost:6061   # c2 -> 6062, c3 -> 6063
  curl -s 'http://localhost:6061/debug/pprof/goroutine?debug=1' | head -1
  go tool pprof -top ./bin/drivel 'http://localhost:6061/debug/pprof/heap'
  ```

  Keep them on loopback. The endpoint serves the process's heap, which here means
  synced file paths and, in a buffer somewhere, their contents.
- **API request counts.** The ground truth is the GCP console — APIs & Services →
  Drive API → Metrics, by method, plus the Quotas page for the ceiling. It is also
  the only honest measure of whether a case would have hit a real user's quota,
  which is a fleet-wide property: all three clients are the same user against the
  same project. `logcount.sh` approximates the transfer half from the logs.
- **Per-process network bytes.** Awkward on a shared host. Either give each client
  its own netns and read `nstat`, or derive volume from `logcount.sh` plus the
  known file sizes.

  **`/proc/<pid>/io` is not a third option, and the reason is the project's own
  HTTP/3 requirement.** `wchar` counts the write family, and QUIC sends with
  `sendmsg`, which is not in it: a complete 64 MiB upload through `gdrive` moved
  the backend's `wchar` by **0.10 MiB** (measured 2026-10-16, while reading a
  column that turned out to measure nothing). What *is* usable there is the
  **backend's `rchar`** — it receives the file from the host over the plugin
  socket, flow-controlled end to end, so it tracks an upload's progress closely
  (64.83 MiB for that same transfer) and is the cheapest way to tell "this
  transfer is under way" from "this transfer has not started". The host's own
  counters read ~3x the file size on `rchar` and ~2x on `wchar` for one upload,
  because the mount, the push and gate 3's hash each read it and the backing
  write and the socket stream each write it.

## Group V: injecting failures

Most of Group V is settled in Tier A (`internal/app/failure_test.go`), and what is
left here is the part that needs a real process, a real transport or a real
filesystem. Read the matrix rows first: four of the nine cases now have a confirmed
answer, and two of those answers changed what the live run should look for.

**Before any of it, know what repairs what.** A push that fails for more than a few
seconds is *dropped* — five attempts, jittered backoff, then `[sync] push … failed
after 5 attempt(s)` — and a download whose apply fails is never re-offered, because
the cursor advances past a failed page. Both are repaired only by an M7b sweep. So a
fault-injection case run with `sweep-interval = "0"` has **no repair path at all**,
and a run that ends with the fleet diverged may be measuring that rather than
whatever it meant to measure. Decide per case: leave it at 0 and assert the
divergence, or set a short interval and measure the recovery.

### MC-40, MC-41 — kill the process

```sh
kill -9 "$(pgrep -f 'rig/c1/config.toml')"
```

**MC-40 ran 2026-10-13** over the SFTP rig below rather than Drive; the plan's row
has the results, and what is still unrun is the resumable-session half. What
follows is the recipe, including the two things that cost time the first time.

Two things to check that the Tier A versions cannot. The upload session is not
persisted, so a restart re-uploads from zero: look for a *duplicate* remote object
beside the real one rather than a resumed transfer — and note that only an
ID-addressed backend can produce one, so this half of the case is Drive's alone.
And since M23 the backend runs in its own process, so after the kill, check for an
orphan:

```sh
# Note the host's pid when you start it, then find ITS child by parentage:
ps -eo pid,ppid,args | awk -v p="$host" '$2==p && /plugin-serve/ {print $1}'
kill -9 "$host"; sleep 3; kill -0 "$child"   # succeeds => it outlived its host
pgrep -af 'drivel plugin-serve'              # finds nothing, ever
pkill  -x drivel                             # does NOT reach the child
```

**The child's command line is `/proc/self/exe plugin-serve <kind>` and its `comm`
is `exe`**, because that is the path the host re-executes. So a recipe built
around the program's name misses it and reads as "no orphan". A child that
outlives its host is holding the account's credentials with nothing driving it —
which is what this found, and `plugin/liveness.go` is the fix.

**Counting `plugin-serve` processes does not answer this, and the first MC-40 (b)
re-run got it wrong before the result was read.** Every live client has a backend
of its own, so a fleet of two always shows at least one; and a pattern over
command lines matches the shell that runs the pattern, which is the trap this
README already records for stopping clients. Parentage is the only form that
discriminates, and it has to be sampled *before* the kill, because afterwards the
child is reparented to pid 1 and nothing connects it to the host it belonged to.

Two things about getting the kill to land mid-upload. **Trigger on the temporary
existing, not on its size**: the destination path never holds a partial file
(M18 rule 7), so what to watch for is `.drivel-upload.*` in the destination's
directory, and a size threshold turns a throughput measurement into a race you
lose on a slow disk. **Put the rig's backing trees and the SFTP root on tmpfs** —
a 512 MiB push took 3s there and between 25s and 95s on a 91%-full spinning
volume, with the variance landing exactly where the kill has to be timed.

And run the case in both shapes, because they did not have the same answer: a kill
mid-upload of a **new** file recovers on the next sweep, and a kill mid-upload of a
**rewrite** of an already-synced file recovered not at all until M27 part 4. Set
`sweep-interval` long enough that no sweep races the push you are interrupting
(10m for the client being killed), or the sweep's own push of the same path
confuses the result — and a short one for *after* the restart, which is why the
rig below carries two configs per client.

**The (b) half is scripted, because its finding is a silence.** Nothing happening
is not evidence on its own, so the script takes the build to crash and the build
to come back up as separate arguments and is run three times:

```sh
# The "before" build comes from a worktree, so the checkout stays where it is:
git worktree add /tmp/pre <COMMIT_BEFORE_THE_FIX>
(cd /tmp/pre && CGO_ENABLED=0 go build -o /tmp/pre/bin/drivel ./cmd/drivel)

cp=scripts/multiclient/mc40b-lostpush.sh
$cp -d "$rig" -b /tmp/pre/bin/drivel                  -m 256 -f 25 -w 300  # the defect
$cp -d "$rig" -b bin/drivel                           -m 256 -f 25 -w 180  # the fix
$cp -d "$rig" -b /tmp/pre/bin/drivel -B bin/drivel    -m 256 -f 25 -w 180  # the upgrade
```

The third is the shape a user meets: the crash happens on whatever was installed
and the repair is whatever they upgraded to, so it is also the one that shows the
row works off a state DB an older build wrote. Give the first leg a long watch —
its claim is "N sweeps and nothing changed", and N is the number that makes it.

Two halves of the case are Drive's alone and are in `mc40b-drive.sh`: whether the
repair **updates** the object or grows a second one beside it (a path-addressed
backend cannot pose that question), and whether a metadata-only divergence
**settles**, since `gdrive` is the tree's only `ContentHasher` and M6 gate 3 runs
nowhere else. Its oracle is `rig-ls.py`, which lists the folder with digests
straight from the API — a peer client cannot answer "how many objects are at this
path?", because same-name siblings collapse into one local file (MC-30).

**Do not edit a script while a run of it is in flight.** bash reads a script
incrementally and seeks back to a byte offset between commands, so inserting
lines near the top of a file whose loop is still running resumes the run in the
middle of a comment. It cost one 7-minute leg here, and the failure looks like a
product problem rather than a harness one: the run simply stops reporting.

### MC-44, MC-44b — take the network away from one client

The hard part is "one client". A plain output rule partitions the whole host, which
turns MC-44b into "the fleet is offline" and measures nothing. Three ways to scope
it, cheapest first: run that client as its own user and match `meta skuid`; run it
under `systemd-run --user --scope` and match its cgroup; or give it its own netns,
which also answers the per-process byte counts this README says it cannot.

```sh
sudo nft add table inet drivel-fault
sudo nft 'add chain inet drivel-fault out { type filter hook output priority 0; }'
# MC-44: QUIC only, so the HTTP/2 fallback carries the traffic
sudo nft add rule inet drivel-fault out meta skuid 1001 udp dport 443 drop
# MC-44b: everything
sudo nft add rule inet drivel-fault out meta skuid 1001 tcp dport 443 drop
# ... restore
sudo nft delete table inet drivel-fault
```

MC-44's measurement is the point: same workload, one client on H3 and two on the
H2 fallback, so the CPU and throughput columns of `sample.sh` are a free
protocol comparison. For MC-44b the one thing Tier A cannot answer is whether the
change cursor survives 20 real minutes; everything else about that case is already
pinned, including that inbound recovers and outbound does not.

**MC-44 ran in full on 2026-10-13**, in three rigs rather than one, and which one
you want depends on the question.

**When the fallback fires, and whether HTTP/3 comes back**, needs no account at
all: driving `internal/transport` against a public HTTP/3 endpoint answers both,
and that is where both of the case's product findings were. **How long the
fallback takes** needs the blackhole rig below, because a local drop is not a
blackhole. **What each protocol costs** needs the Drive provider, because that is
where the transport is injected:

```sh
DRIVEL_CREDS=rig/c1/credentials.json \
DRIVEL_TOKEN=rig/c1/token.json \
DRIVEL_LIVE_ROOT=<FOLDER_ID> \
  scripts/multiclient/mc44-throughput.sh -m 64 -n 5 -o rig/results/mc44-throughput/$(date +%Y%m%dT%H%M%S)
```

That drops UDP/443 for its own cgroup to force the fallback, proves the scope in
both directions before it measures anything, and runs alternating HTTP/2 and
HTTP/3 pairs. Two things in it are the difference between a measurement and a
story. **The protocol is attributed, not assumed** — nftables counters on the
output hook, so an HTTP/2 run must read ~0 on udp443 and an HTTP/3 run ~0 on
tcp443; the counters are of *egress*, so a run's figure is ~the payload for the
upload leg and ~nothing for the download leg. And **it alternates and takes a
median** because download times vary **twofold between consecutive runs of one
protocol** while upload times repeat to within 0.1%: one sample of each reads as a
clean 2:1 win for whichever got lucky, which is what the first run of it produced.
The answer, for reference, is that throughput is indistinguishable (Drive is the
limiter at ~4 MiB/s up) and HTTP/3 costs ten times the CPU per MiB uploaded.

Three things from the transport run, and then the blackhole rig that followed it.

**Check whether you share a network namespace before writing any rule.** In the
project's dev container `nft list ruleset` shows the *host's* tables, so an
unscoped output rule takes QUIC away from the developer's browser and from any
real mount they have running. A cgroup is the cheapest scope that needs no second
user:

```sh
sudo mkdir /sys/fs/cgroup/drivel-fault
sudo sh -c 'echo PID > /sys/fs/cgroup/drivel-fault/cgroup.procs'
sudo nft add rule inet drivel-fault out socket cgroupv2 level 1 "drivel-fault" udp dport 443 drop
```

**Then prove the scope in both directions before measuring anything**, because a
rule that silently matches nothing reads exactly like a transport that never falls
back: a process *outside* the cgroup must still negotiate HTTP/3, and one inside
must not.

**An output-hook `drop` is not a blackhole, and the two differ by 6x.** The kernel
reports a local drop to the sender as `EPERM`, so the QUIC write fails immediately
and the fallback is instant. A network that silently discards UDP/443 gives no
error, and the attempt has to be waited out instead. Both shapes are now run; use
the second for anything about *latency*, and expect two numbers from it, not one
(mid-session 30s, startup 5s — see the MC-44 row).

The blackhole rig is **two namespaces and no host involvement at all**, which is
what makes it safe on a machine whose network namespace is shared with the user's
desktop:

```sh
ip netns add mc44c; ip netns add mc44s
ip link add mc44veth-c type veth peer name mc44veth-s
ip link set mc44veth-c netns mc44c; ip link set mc44veth-s netns mc44s
ip -n mc44c addr add 10.244.44.2/24 dev mc44veth-c
ip -n mc44s addr add 10.244.44.1/24 dev mc44veth-s
ip -n mc44c link set lo up; ip -n mc44c link set mc44veth-c up
ip -n mc44s link set lo up; ip -n mc44s link set mc44veth-s up
printf '127.0.0.1 localhost\n10.244.44.1 mc44.test\n' > /etc/netns/mc44c/hosts
```

Four things about it. **The drop goes in the *server* namespace's input hook** —
a rule in the client's output hook is the `EPERM` case again, so it has to be
somewhere the packet reaches after leaving the sender. **Neither veth end stays in
the host namespace**, so there is nothing to clean up there and nothing to get
wrong; `ip netns del` takes the interfaces and the nft tables with it. **Prove the
isolation before measuring**: no `mc44` interface in the host's `ip -br link`, the
host's `nft list tables` count unchanged, `ping 10.244.44.1` from the host failing,
and the host's own `curl --http3-only` still working. And **an origin is needed
inside the rig** — a quic-go `http3.Server` and a `net/http` TLS server on the same
`:443` with one self-signed cert, which the client trusts through `SSL_CERT_FILE`.
Drive itself is unreachable from a namespace with no route out, and that is the
point: the measurement is of `transport.New()`, which is reached by a probe binary
inside the repo module.

**Time the restore against the window, not the rule.** The mark re-arms on failure
only, so HTTP/3 returns on the first request after *the last failure* plus
`h3Retry` — which can be nearly a whole window after the rule came off and reads
like the fix not working. Either shorten `h3Retry` for the run or compute the
expected instant from the mark; the blackhole run did the latter and landed on it
to the second.

One trap specific to the throughput rig, and it cost a run: `nft reset counters`
resets named counter *objects* and leaves an anonymous rule counter alone, so one
phase's total silently carries into the next and the second protocol reads as
though it sent everything twice. `nft reset rules` is the one that clears them.
Anything to do with the rig's expired tokens is in "Before the first run" above.

### MC-46 — fill a client's backing filesystem

Give it a small one rather than filling a real one:

```sh
sudo mount -t tmpfs -o size=64M tmpfs rig/c3/data     # then sync something larger
```

Tier A already shows that a transfer stopping partway leaves nothing at the
destination path. What only a filesystem can show is whether ENOSPC is reported at
all — the `io.Copy` into the temporary is where it surfaces — and what the mount
does to a *user* write when the backing store is full.

### MC-47 — cross-process hygiene

**Run 2026-10-10; the results are in the plan's matrix row and §4 item 18.** This
is the recipe, because the shape is worth being able to re-pose.

It needs two processes and does **not** need Drive. A local SFTP server is a remote
two `drivel` processes can share with no quota and no account — see the M18 rig
notes — which also makes the case repeatable. `app.Validate` refuses all four
shapes inside one process, so the whole question is what happens when nothing is
checking.

An unprivileged `sshd` on a high port, with `internal-sftp` (an external
`Subsystem` runs through the login shell, and anything that shell prints corrupts
the stream — see the M18 notes, it costs an hour to rediagnose):

```sh
ssh-keygen -q -t ed25519 -f host_ed25519 -N ''
ssh-keygen -q -t ed25519 -f client_ed25519 -N '' && cp client_ed25519.pub authorized_keys
cat > sshd_config <<EOF
Port 2247
ListenAddress 127.0.0.1
HostKey      $PWD/host_ed25519
AuthorizedKeysFile $PWD/authorized_keys
StrictModes no
UsePAM no
PasswordAuthentication no
AllowUsers $(id -un)
Subsystem sftp internal-sftp
EOF
/usr/sbin/sshd -f $PWD/sshd_config -E sshd.log
ssh-keyscan -H -p 2247 127.0.0.1 > known_hosts     # the provider fails closed; not optional
```

Then one config per client, each with its **own** mountpoint, backing dir and
state file, both pointing at one `root`. Two settings are not optional:
`sweep-interval` of a few seconds (SFTP has no feed, so this is the poll interval)
and **`materialize = true`** — without it each client uploads and neither receives,
which looks exactly like a broken rig and is not.

Then break one thing at a time, restoring between shapes:

| Shape | Change | What to watch |
|---|---|---|
| (a) shared state DB | B's `state` = A's `state` | B must refuse in ~5s naming the other process; A must keep pushing and its file must not gain a `.corrupt` sibling |
| (b) nested backing | B's `data` inside A's `data` | `find A/data -type f` and the path depth, sampled every few seconds — both grow without bound |
| (c) nested mountpoint | B's `path` inside A's `data` | the same growth, plus A's downloads landing in B's backing tree; then stop B and confirm the delete pass still infers nothing |
| (d) nested state DB | B's `state` inside A's `data` | A uploads B's live bbolt file to the shared remote |

(b) and (c) grow one copy per sweep interval per direction, so a 3s interval makes
them obvious in under a minute and a tiny seed file keeps the mess small. Stop both
clients before the depth gets long enough to annoy your filesystem.

## Keeping the two tiers honest

Every Tier B failure gets a Tier A regression test before it is fixed. Tier B is
slow, quota-limited and non-deterministic — a discovery instrument, not a gate.
`TestFleet*` in `internal/app` and `TestChanges*` in `internal/provider/gdrive`
are where the findings live afterwards.
