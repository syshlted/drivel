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
4. **One token per client.** `token.json` is rewritten on refresh, and three
   processes sharing one path race on that write.

## Laying it out

```sh
make build
scripts/multiclient/rig-init.sh -r <FOLDER_ID> -c ~/credentials.json -n 3
```

That writes `rig/c1…c3` (mountpoint, backing dir, state DB, index DB, config,
logs) plus `rig/oracle`. Give each client a token — `drivel login -account c1`,
then copy the token into `rig/c1/token.json` — or pass `-t token.json` to
`rig-init.sh` to seed all of them from one you already have.

Start each client in its own terminal:

```sh
./bin/drivel mount -config rig/c1/config.toml 2>&1 | ts > rig/c1/logs/run.log
```

`ts` (moreutils) timestamps every line; the convergence and latency numbers read
those timestamps.

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
pgrep -af plugin-serve          # works
pgrep -af 'drivel plugin-serve' # finds nothing, ever
pkill  -x drivel                # does NOT reach the child
```

**The child's command line is `/proc/self/exe plugin-serve <kind>` and its `comm`
is `exe`**, because that is the path the host re-executes. So match on
`plugin-serve` and nothing else; a recipe built around the program's name misses
it and reads as "no orphan". A child that outlives its host is holding the
account's credentials with nothing driving it — which is what this found, and
`plugin/liveness.go` is the fix.

Two things about getting the kill to land mid-upload. **Trigger on the temporary
existing, not on its size**: the destination path never holds a partial file
(M18 rule 7), so what to watch for is `.drivel-upload.*` in the destination's
directory, and a size threshold turns a throughput measurement into a race you
lose on a slow disk. **Put the rig's backing trees and the SFTP root on tmpfs** —
a 512 MiB push took 3s there and between 25s and 95s on a 91%-full spinning
volume, with the variance landing exactly where the kill has to be timed.

And run the case in both shapes, because they do not have the same answer: a kill
mid-upload of a **new** file recovers on the next sweep, and a kill mid-upload of a
**rewrite** of an already-synced file does not recover at all. Set
`sweep-interval` long enough that no sweep races the push you are interrupting
(10m for the client being killed), or the sweep's own push of the same path
confuses the result.

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

**The transport half of MC-44 ran 2026-10-13**, and it ran without an account at
all: driving `internal/transport` against a public HTTP/3 endpoint answers when
the fallback fires and whether HTTP/3 ever comes back, which is where both of that
case's findings were. It does not answer the throughput comparison, which needs
the Drive provider because that is where the transport is injected. If you only
need the first question, that is the cheap way to ask it. Four things from the run.

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

**Time the restore against the re-probe window, not against the rule.** A failed
re-probe re-arms the window, so HTTP/3 returns on the first request after the last
probe plus `h3Retry` — which can be most of a window after the rule came off, and
looks like the fix not working. Shorten `h3Retry` for the run rather than guessing.

**An output-hook `drop` is not a blackhole.** The kernel reports it to the local
sender as `EPERM`, so the QUIC write fails immediately and the fallback is
instant. A network that silently discards UDP/443 gives no error and the fallback
waits on `HandshakeIdleTimeout` instead. Measuring that needs the drop to happen
off-host — a netns with the rule on the far side of the veth — which is the one
thing per-cgroup scoping cannot express.

And for the Drive half: **the rig's tokens expire.** A Google Cloud project left
in **Testing** publishing status issues refresh tokens that die after seven days,
so `drivel login` has to be re-run per client before any Group V or VI session.
The symptom is `oauth2: "invalid_grant"` on the first call, which looks like a
drivel bug and is not one.

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
