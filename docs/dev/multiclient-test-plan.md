# Multi-client sync test plan

Three `drivel` instances, one Drive folder, one machine.

## 0. Why this is a separate exercise

Every layer already has tests against its own fake, and `internal/app/e2e_test.go`
crosses the seams once for a single client. None of that can reach the failures
that only exist when several independent state machines share one remote:

- **N-way convergence.** §4 echo suppression is *per client*. A push by A is an
  echo to A and a genuine remote change to B and C. Nothing in the single-client
  tests exercises "B applies a change that A originated and does not push it back".
- **Divergence is designed in.** §6 conflict copies are local-only and never
  synced. After any real conflict the three trees are *supposed* to differ. An
  integrity oracle that demands three identical trees will report every conflict
  test as a failure.
- **Drive allows what POSIX cannot represent.** Two clients creating the same path
  concurrently produce two same-name siblings in one folder;
  `gdrive.lookupChildLocked` then picks the most recently modified and logs that
  the others "are not visible at this path".
- **Quota is a shared, fleet-wide resource.** All three clients authenticate as
  the same user against the same GCP project, so they contend for the same
  per-user request budget. The throttling threshold is a property of the fleet,
  not of a client.
- **`app.Validate` is intra-process.** It guards N mounts inside one `App`. Three
  separate processes get no cross-checking at all: overlapping backing trees,
  a mount inside another's backing directory, or a shared state DB are all
  unvalidated. (The shared state DB at least fails safely — bbolt takes an
  exclusive flock and `state.boltOptions` sets a 5s timeout — but the overlap
  cases are live ammunition.)

## 1. Two tiers, in this order

**Tier A — deterministic, in-process, fake provider.** Three `app.Mount`s in one
test process, all sharing one `memStore` (`internal/app/memstore_test.go` already
implements the whole `provider.Store` + `ChangeSource` + `Enumerator` surface, with
`putCount`/`getCount` accessors that make amplification directly assertable). Runs
under `-race`, in CI, in seconds.

**Built:** `internal/app/multiclient_test.go` — the `fleet` harness (N peers, each
startable and stoppable on its own, with per-peer logging, manifests and a
convergence check that includes the remote as a party) and `TestFleet*` covering
MC-03, MC-12 (its reserved-name half), MC-21, MC-23, MC-25, MC-26, MC-31a,
MC-31b and MC-32. About 90s for the set.
`internal/app/faultstore_test.go` adds per-peer fault injection over the same
shared store — a client cut off from the remote, a transfer that stops partway,
and an enumeration that can be held open at a page boundary — and
`internal/app/failure_test.go` is Group V on top of it: MC-41, MC-43 (in two
shapes), MC-44b, MC-46 and MC-45. About another 100s, most of it MC-44b waiting
out the product's 30s idle poll cadence.
`internal/provider/gdrive/changes_test.go` covers the change feed's shape, on a
`fakeDrive` that now serves a real `changes.list` log, and
`internal/provider/gdrive/siblings_test.go` covers MC-30 below the seam.

Tier A owns: convergence, echo suppression across clients, conflict-copy
semantics, rename propagation, delete inference, the reconcile guards, ping-pong
detection, and — since Group V — restart and degradation for every fault that is
a *provider answering badly* rather than a process dying.

The faults are **per peer and not per fleet**, which is the whole difference
between "the provider is down" (nobody syncs, nothing to observe) and "this
client is partitioned" (the fleet moves on without it and has to re-converge
afterwards). The fleet therefore registers one provider kind per peer, each a
decorator over the one store they share.

Two traps in the harness itself, both paid for. `testLog.saw` answers "has this
mount ever logged that?", and a peer's log accumulates across its restarts — so
every Group V case, which restarts a peer in order to exist, was waiting for a
line its previous life had already printed. That wait returns on the first poll
and leaves every assertion after it racing whatever the new process is doing;
`mark`/`sawSince` is the fix and is what the restart cases must use. And the
fleet's convergence check hashes the backing tree, so a **lazy** peer's
placeholders read as content that disagrees with everyone: the two placeholder
cases assert against the remote and the marker directly and never call
`waitConverged`.

**Tier B — three real processes against real Drive.** Scripts in
`scripts/multiclient` (`rig-init.sh`, `workload.sh`, `manifest.sh`, `converge.sh`,
`sample.sh`, `logcount.sh`, `mutate.sh`) with a README that walks a case through.
Owns everything Tier A cannot fake: transport behaviour (HTTP/3 vs the HTTP/2 fallback), Drive's actual
same-name-sibling behaviour, 403 rate limiting, Google-native documents, real
resumable-upload chunking, real wall-clock convergence, and all resource
measurement.

The rule: **every Tier B failure gets a Tier A regression test before it is
fixed.** Tier B is slow, quota-limited and non-deterministic; it is a discovery
instrument, not a suite you can run on every commit.

MC-30 was the last Tier A case, and it needed the hook the others did not: it
belongs in `internal/provider/gdrive` against `fakeDrive`, not in `memStore`,
because duplicate siblings are a property of Drive's data model and emulating
them in a path-keyed map would test the emulation rather than the behaviour.
Below the seam the fake is ID-keyed like the real thing, so the situation arises
by itself once two things are true of it: `Files.Create` mints a fresh opaque ID
per call (keying a created object by its name folds three concurrent creates into
one file), and it stores bytes, since "two of these contents are now unreachable"
is a claim about content. A `nameBarrier` in the fake holds every name query
until all three are in flight, which is what makes the race deterministic — left
to chance the three `Put`s serialise, the second client finds the first client's
file and updates it, and the situation under test never forms. Dropping the
barrier fails the test on every run, which is the check that it is load-bearing.

MC-25 needed one too and no longer does. Rather than a call-counting fault
injector, `memStore.Changes` now answers a cursor it cannot place with
`ErrCursorExpired` — which is what Drive does (410 for a retired token, 400 with a
`pageToken` reason for a malformed one) and what `gdrive` already classifies. The
test then kills a stopped peer's cursor by writing a dead token into its state DB,
which is the same thing the Tier B procedure does by hand, so the two tiers reach
the expiry through the same door.

---

## 2. Rig

### 2.1 Safety preconditions — read before the first run

1. **A dedicated Drive folder, and pass its ID.** `-drive-root root` maps the
   mount to the whole of My Drive. Combined with an empty `-data` and a reconcile,
   that is the documented catastrophic configuration.
2. **`Store.Remove` trashes rather than deletes, and the trash fills up.**
   Deletions in these tests are recoverable from the web UI for 30 days, which is
   a safety net and not a reason to relax: several cases delete thousands of files
   at a time, and everything they trash keeps counting against the account's quota
   until it is emptied. Empty it between runs. Run any case with
   `delete = "permanent"` and the old rule applies instead — gone is gone.
3. **A throwaway Google account.** The delete and conflict cases are designed to
   destroy data.
4. **One token per client**, and it is tidiness rather than safety. This item used
   to say `token.json` is rewritten on refresh and that three processes sharing one
   path race on that write. **That is wrong and MC-45 is why**: nothing in a mount
   ever writes a token file — `gauth.SaveToken` has one caller, `drivel login` —
   and the refresh happens in memory inside `oauth2`. Keep giving each client its
   own config directory (`drivel login -account c1|c2|c3`) anyway, because it is
   what makes a per-client log, state DB and index obvious from the layout and
   because a future write-back would reinstate the hazard, but a shared token path
   does not corrupt anything today. `TestOnlyLoginWritesTheOAuthToken` is what
   keeps that true.

### 2.2 Layout

```
rig/
  c1/{mnt,data,logs}  c1/drivel-state.db  c1/drivel-index.db  c1/config.toml
  c2/…  c3/…
  oracle/{mnt,data}                 # 4th cold client, see 2.4
  manifests/  metrics/  results/
```

State and index DBs live **outside** every `data/` tree — one mount's DB inside
another's backing directory is the cross-process version of the guard
`app.Validate` enforces in-process, and it syncs the DB while it is being written
to.

Per-client config (`rig/c1/config.toml`):

```toml
[account.c1]
provider = "gdrive"
credentials = "/abs/rig/c1/credentials.json"
token = "/abs/rig/c1/token.json"

[[mount]]
name = "c1"
account = "c1"
path = "/abs/rig/c1/mnt"
data = "/abs/rig/c1/data"
state = "/abs/rig/c1/drivel-state.db"
sweep-interval = "24h"          # override per test; "0" disables
max-deletes = 100               # override per test; 0 = unlimited

[mount.provider]
root = "<FOLDER_ID>"            # never "root"
index = "/abs/rig/c1/drivel-index.db"
```

Launch: `./bin/drivel mount -config rig/cN/config.toml 2>&1 | ts > rig/cN/logs/run.log`
(`ts` from moreutils gives every line a timestamp, which the convergence detector
and the latency measurements both need).

#### 2.2.1 One rig per scenario

`rig/` above is the original Tier B rig, and it stays as it is. Everything after
the smoke cases gets a **scenario**: its own rig, its own Drive folder, created by
`scripts/multiclient/scenario.sh new NAME -r FOLDER_ID` and recorded in
`scenarios/REGISTRY.md`.

Reusing one rig is the tempting alternative and it costs more than it looks.
Several cases are counting arguments — MC-11 writes 10 000 files and asks how many
`[sync] upload` lines came out — and that is subtraction only while the folder
holds nothing else. Against a populated folder every derived count needs
path-prefix filtering, including the count the case exists to produce. The
cheaper-looking fix, clearing the folder first, is worse: it is a thousand deletes
and a thousand trashed objects still on the quota, spent to reach a state a new
folder gives away.

A scenario is one physical directory and never a symlink into another. drivel's
cardinal rule is about backing-tree and mountpoint overlap and `app.Validate`
compares paths to enforce it, so a symlinked path is precisely how that guard gets
defeated without anyone noticing.

**Placement is part of the scenario**, because the hardware is not uniform:

| Placement | Where | For |
|---|---|---|
| `overlay` | `~/drivel-rig/scenarios` | Anything reporting a latency, throughput or convergence time |
| `bulk` | the external disk, `$DRIVEL_BULK_ROOT` | Capacity-bound cases only — MC-10, MC-52, MC-46 |

A duration is only comparable to durations taken on the same disk, and the
MC-01/02/03 baselines were taken on the overlay. The bulk disk at the time of
writing is a magnetic USB drive measured at 61 MB/s sequential and **19 synchronous
4 KiB writes per second** — that second figure is what a bbolt commit costs — and
it is scheduled to be replaced by an SSD, so `scenario.sh` stamps the device into
each scenario's `SCENARIO.md` at creation. A result that does not name its disk
becomes uninterpretable the moment the hardware changes. See the bulk root's
`README.md` for the full measurements and the migration notes.

**Never run two fleets at once.** They share a Drive account, so the second one's
traffic lands in the first one's quota and request-rate numbers; `scenario.sh
start` refuses while any other client is alive.

### 2.3 Test-run knobs

Several defaults are tuned for production and make tests take a day. Override
per case, and record the override in the result:

| Knob | Default | Test value | Why |
|---|---|---|---|
| `sweep-interval` | 24h | `2m` for MC-53, `0` elsewhere | The periodic sweep is the single most dangerous scheduled event in the system; it must be exercised deliberately, and must not fire in the middle of unrelated cases. |
| `max-deletes` | 100 | vary per case | MC-25 depends on crossing it; MC-23 depends on not reaching it. |
| Poll cadence | 2s/30s (`syncengine.DefaultCadence`) | leave alone in Tier B | It is what a user gets. Tier A drives it directly. |
| `-lazy` | off | one client on, in MC-60 | Mixed-mode fleets are the realistic deployment. |

### 2.4 The oracle problem

Three clients agreeing proves they converged, not that they converged on what
Drive holds. Validate the remote independently:

- **Cold client.** A fourth `drivel` with empty `data/`, no state DB, no index,
  `-resync -materialize`. It reconstructs the tree from `Enumerate` alone. Its
  manifest *is* the remote's manifest, and building it exercises M7b at the same
  time. Preferred, because it is the same code path a new machine would take.
- **Independent cross-check.** `rclone lsjson --hash -R` against the same folder,
  for the cases where you want a second opinion that isn't drivel's own code —
  particularly MC-30 (same-name siblings), which by construction a path-addressed
  client cannot see.

### 2.5 Manifests

```sh
# manifest.sh DIR > out.tsv  — path, size, sha256, mtime; conflict copies split out
cd "$1" && find . -type f -printf '%P\n' | LC_ALL=C sort | while IFS= read -r p; do
  printf '%s\t%s\t%s\t%s\n' "$p" "$(stat -c%s "$p")" "$(sha256sum "$p" | cut -d' ' -f1)" "$(stat -c%Y "$p")"
done
```

Compare with `diff <(grep -v ' (conflict ' a.tsv) <(grep -v ' (conflict ' b.tsv)`
and report conflict copies separately — see §0. Run the manifest against `data/`,
not `mnt/`, except where the case explicitly tests visibility through FUSE; in
lazy mode hashing through `mnt/` hydrates every placeholder and destroys the
thing you were measuring.

### 2.6 Convergence detector

Converged = all three `data/` manifests equal (modulo conflict copies) **and**
equal to the oracle's, sustained across two samples 15s apart. Sample every 5s;
record time-to-converge from the last local write. Hard timeout 10× the expected
value, then dump all logs and mark the case failed rather than hanging.

Equal means path, size, content hash and kind — **not mtime**, and that is a
finding rather than a convenience. A downloaded file carries the remote's
`modifiedTime`: `Downloader.download` stamps it, which is what makes two
*pulling* peers agree and what stops a file that just arrived from looking newer
than the remote it came from (it would otherwise be pushed straight back — MC-32's
ping-pong, reached through the download path instead of a conflict). But the peer
that *originated* the write keeps its own local write time; the provider stamps
the upload with a time of its own and nothing writes that back down. So for every
file exactly one client's mtime differs from every other's, permanently and by
design. `converge.sh` compares columns 1, 2, 3 and 5; comparing column 4 would
mean no Tier B case ever converged, which is how this was found. Pinned by
`TestDownloadCarriesTheRemoteModifiedTime` in `internal/syncengine`.

Do not define convergence as "the logs went quiet" alone — the slow cadence is
30s, so silence is ambiguous — but do record log quiescence separately, because
"trees agree but the logs are still moving" is the ping-pong signature MC-32 hunts
for.

### 2.7 Measurement

**Per-client resources** — sample `/proc/<pid>/` at 1 Hz into CSV:

| Source | Field | What it answers |
|---|---|---|
| `status` | `VmRSS`, `VmHWM` | Steady-state footprint; whether a 5 GiB file is streamed or buffered |
| `stat` | `utime`+`stime` | CPU per client; the delta across a case is the cost of that case |
| `io` | `read_bytes`, `write_bytes` | Disk amplification (a 4 KiB edit that rewrites 1 GiB is visible here) |
| `fd/` | count | fd leaks under the file-handle write capture |
| `status` | `Threads` | go-fuse pool growth |

**Goroutines and heap** need in-process instrumentation, which is what
`mount -pprof localhost:6060` is for — it turns "RSS grew 400 MB" into "here is
the retained object graph", and it is the only way to answer MC-53's "goroutine
count flat". Sample it alongside `sample.sh`:

```sh
curl -s "http://localhost:6060/debug/pprof/goroutine?debug=1" | head -1   # "goroutine profile: total N"
go tool pprof -top ./bin/drivel "http://localhost:6060/debug/pprof/heap"
```

Give each client its own port. Loopback is now enforced rather than advised —
a non-loopback bind is refused unless `-pprof-allow-remote` is given — because the
endpoint serves the process's heap, which in this system means synced file paths
and, in a buffer somewhere, their contents. Note that an editor's port forwarder
will happily publish a loopback bind off the host, which is the one case the rule
does not cover; on a rig, check what is forwarded before a long run.

**Network** — per-process byte counts are awkward on a shared host. Two options
that work: run each client in its own netns and read `nstat`, or (simpler, and
sufficient) derive transfer volume from the logs, since `[sync] push`,
`[pull] download` and `[sync] skip … remote already holds these bytes` name the
path and the file sizes are known. For request counts, the ground truth is the
GCP console: **APIs & Services → Drive API → Metrics**, broken down by method,
plus the Quotas page for the throttling ceiling. That is also the only honest
measure of whether a case would have hit a real user's quota.

**Log-derived counters** — grep counts per client, per case:

| Pattern | Meaning |
|---|---|
| `\[sync\] upload` / `\[pull\] download` | Transfer fan-out. The upload line carries the byte count, so volume needs no second lookup. **Not** `\[sync\] push` — those lines are failures and retries only, and this table said otherwise until the first live MC-03 reported one upload as zero |
| `\[sync\] delete` / `\[sync\] rename` | Outbound metadata ops, the counterpart to `\[pull\] delete`. MC-23's originating client is otherwise silent while its peers log 5000 deletes each |
| `\[sync\] skip .* remote already holds` | M6 gate 3 firing (should be common) |
| `\[sync\] range write` | Must be **zero** on Drive — `gdrive` deliberately has no `RangePutter` |
| `\[sync\] skip .* unhydrated placeholder` | M5 guard firing |
| `\[pull\] conflict` | §6 resolution |
| `attempt .* failed, retrying` | Transient/quota pressure |
| `failed after .* attempt` | **Pushes abandoned — data not sent *yet*.** `executeWithRetry` allows 5 attempts over ~7.5s of jittered backoff and then drops the event, so the local file is correct and the remote has not heard about it. **Since M27 (2026-10-12) the path is recorded** and the next sweep is brought forward ~5 minutes to re-send it — including under `sweep-interval = "0"`, which §2.3 sets for every live case — so this line now measures *latency and repair*, not permanent divergence. Count it with `\[sweep\] repush` below: the pair is the recovery, and this line alone with no repush following it is the thing to escalate. Counting the retry line without this one is still the wrong asymmetry |
| `\[sweep\] repush` | **An abandoned push being repaired** (M27). Its absence after a `failed after` line is the signal |
| `back-pressure: writes through the mount` | **The mount is blocking writes on the uploader** (M27). One line per episode, paired with `back-pressure: cleared`; an unpaired one means it is still going on. This is the thing MC-11 measured as a 1286-deep backlog and could only infer. Its depth is often well short of the 1280 bound — paths are hashed to workers, so one busy worker blocks at `eventBuffer + 64` while the others idle, first observed at 646 of 1280 |
| `\[drive\] .* share the name` | Same-name siblings — **silent data loss** |
| `\[sweep\] REFUSING to delete` | `-max-deletes` abandoned a pass |
| `\[sweep\] reconcile complete in` | Sweep cost |

---

## 3. Test matrix

Each case names what it would catch, because a case that cannot fail informatively
is not worth its wall-clock.

### Group I — baseline

| ID | Setup / action | Expected | Catches |
|---|---|---|---|
| **MC-01** | Seed the folder from A only (1000 files, 20 dirs, 50 MB). Start B and C cold. | Sweep materialises the tree on both. Manifests identical. **Zero** conflict copies, **zero** deletes on the first run. **PASS live** (§5): 3-page enumeration, 1023 objects, 1002 files + 21 dirs materialised on each cold client in 400s, `1023 reconciled locally, 0 pushed`, converged in 13s, 0 conflicts, 0 deletes. | The "first-ever run deletes nothing" invariant, and the M7b sweep under a tree it did not create. |
| **MC-02** | All three idle for 30 min, no writes. | RSS drift < 2 MB/client. CPU < 1%. Drive requests settle at the slow cadence (≈1 `changes.list`/30s/client ⇒ ≈10 req/100s/client, ≈30 for the fleet). **PASS live** (§5): RSS drift negative on all three (−176 kB, −1.7 MB, −2.0 MB), CPU ≈ 0.007%, HWM flat, fds and threads flat, and zero log lines inside the window. | Poll-loop leaks; a cadence that never backs off; the idle quota floor of a fleet. |
| **MC-03** | Write one 1 MiB file on A. | 1 push, exactly 2 downloads, 0 conflicts. Converges within fast-cadence + transfer. **PASS live, twice** (§5): 1 upload, exactly 2 downloads, 0 conflicts both times — but **2.7s on a warm fleet and 21s on one idle for hours**, because the cadence has backed off to 30s. Fan-out to an idle peer is bounded by the poll cadence, not by transfer time; quote both numbers or this case overstates what a user sees by 3×. | Baseline fan-out latency and the N-1 download rule. |

### Group II — scale and integrity

| ID | Setup / action | Expected | Catches |
|---|---|---|---|
| **MC-10** | 1 GiB then 5 GiB of random data written on A. | Byte-identical on all three + oracle. **Peak RSS must not scale with file size** — uploads and downloads stream. | A buffering regression that OOMs a fleet on one big file. Also exercises resumable chunking with `ChunkTransferTimeout` unset. |
| **MC-10b** | While A is still uploading the 5 GiB file, poll for it on B and C. | The file is either absent or complete — never short and never mid-write. Hash any snapshot that appears. | The atomic temp-file + rename in `Downloader.download`; a partial file served as content is silent corruption. |
| **MC-11** | 10 000 × 4 KiB files across 100 directories, created on A. | All three converge. Record: wall clock, API request count, 403/retry count, peak RSS, state DB and index DB size per client. **PASS live** (§5): 10 000 uploads, 10 000 downloads each on c2/c3, 0 conflicts, 0 duplicates, 0 abandoned pushes, **0 403s**; 2 h 32 m at a flat 1.09 uploads/s, converging 6.9 s after the last upload; peak RSS 112 MB on the writer and 39 MB on each puller; both DBs 32 KiB → 4 MiB (~416 B/entry). A cold oracle rebuilt the same tree from `Enumerate` and agrees. Four findings the case did not predict: **the FUSE write path blocks at the upload rate** once a burst exceeds the 1024-slot buffer plus 4 × 64 worker queues (measured backlog 1286 against a capacity of 1280, so `tar -x` runs at Drive's upload rate and nothing says why); **write amplification ≈ 6000×** (190 KB of content, 1175 MB to disk) from per-operation bbolt commits, with the oracle's bulk `SetMany` reconcile 2.8× cheaper for identical content — **but read that ratio in per-commit terms, not as 6000×**, because neither half of it means what it looks like (see §4.17); **enumeration scales with the account, not the mount** (the oracle listed 11 129 objects to keep 10 101, discarding 1028 belonging to another rig's folder — so delete a scenario's folder once recorded); and **MC-01's duplicate upload did not reproduce** — 10 000 lines for 10 000 distinct paths. | Debounce/worker-pool behaviour under burst; the first realistic chance of hitting user rate limits. **It is not that chance**: drivel's serialised upload path caps a writing client near 1 request/s, so a single-writer case cannot burst hard enough to trip a quota. That question moves to MC-36 and MC-51. |
| **MC-12** | Hostile names, all created on A: depth-40 nesting · 5000 entries in one directory (forces Drive paging) · unicode and emoji · `'` and `\` (`escapeQuery`) · spaces, `%`, newline · 255-byte names · a file legitimately named `report (conflict 2024-01-01 00-00-00).pdf` · a file named `.drivel-notes`. | Everything round-trips. **The reserved-name half is answered in Tier A and the two paths disagree:** `skipLocal` guards only reconcile's local walk, so a user file of either shape written *through a mount* is pushed and fans out like anything else, while the same file created with the engine not watching is stepped over by the sweep that exists to catch up on exactly that — silently, and for as long as the file exists. Pinned by `TestFleetSyncsAReservedNameFromTheMountButNeverFromASweep`. Still Tier B: the escaping, paging, unicode and length cases, which need real Drive. | Query-escaping bugs; paging bugs; the conflict-name regex catching real user files. |
| **MC-13** | Empty files · sparse file with holes · hardlinked pair · symlink (relative and absolute) · fifo · device node · setuid bit · a file with user xattrs · non-UTC mtimes. | Define the behaviour and write it down. Drive has no representation for most of these; what matters is that drivel fails predictably rather than corrupting or looping. **Mostly answered by M15 items 1–3 (shipped 2026-09-08), and answered in code rather than in prose**: the hardlinked pair is now impossible (`EPERM` at `Link`, where before both names were pushed as two diverging remote objects); symlinks, fifos, sockets and device nodes stay local and log one line per path at both the mount and the sweep; the mountpoint is `nodev,nosuid` compulsorily, so a setuid bit or device node arriving from a remote is inert there; user xattrs were already never synced. Pinned by `internal/vfs/special_test.go` and `TestSweepLogsTheLocalFilesItCannotSync`. **Still Tier B:** empty and sparse files, non-UTC mtimes, and the setuid bit's *round trip* — the mount option makes it inert locally, it says nothing about what Drive stores. **Still open by decision:** POSIX mode/ownership/ACLs, which is M15 item 4 and needs a provider-metadata capability. | Undefined behaviour becoming a support question. §10 territory. |
| **MC-14** | Create a Google Doc, a Sheet and a Slide in the Drive web UI inside the folder. | All three clients log `skip … Google-native document`, never materialise them, and — critically — **never infer them as deleted** in a later sweep. | The `ExportOnly` seen-marking. A regression here deletes the user's real Drive documents. |

### Group III — mutation semantics

| ID | Setup / action | Expected | Catches |
|---|---|---|---|
| **MC-20** | 1 GiB file, then `dd conv=notrunc` 4 KiB at offset 512 MiB, on A. | `gdrive` implements no `RangePutter`, so this is a whole-file `Put`: **≈3 GiB of traffic for a 4 KiB change** (1 up + 2 down). Verify integrity; record the number. | This is the headline overhead figure for a multi-client fleet. Also confirms zero `[sync] range write` lines against Drive. |
| **MC-21** | `touch` an unmodified file on A. | M6 gate 3: `[sync] skip … remote already holds these bytes`. **Zero** downloads on B and C. | A regression here turns every `touch` into a full 3× file-size fleet event. |
| **MC-22** | Append 1 MiB, 20 times, 1s apart, to a file on A (i.e. spaced wider than the default 300 ms `-push-delay`). | ≈20 uploads of growing size — ≈210 MiB pushed and ≈420 MiB pulled for a 20 MiB file. Quantify it; it is the cost of append-style workloads (logs, databases). Then repeat with `-push-delay 30s`, which should collapse the whole run to ≈1 upload: the point of the knob is this case, and the ratio is the measurement worth recording. | Nothing is broken here; the point is to measure an amplification the design implies and users will hit — and, now, how much of it is recoverable by coalescing. Note the amplification only exists because each append *closes* the file; an appender that holds it open emits nothing at all until it closes. |
| **MC-23** | `rm -rf` a 5000-file subtree on A, all clients online. | Propagates via the change feed. `-max-deletes` is **not** consulted (it bounds reconcile inference, not feed deletes). Verify nothing survives at the mountpoint and that the whole subtree is *in* the trash — 5000 trashed objects is also the case that shows what the default costs in quota. | Feed-driven bulk delete throughput; the `kids` index keeping `forgetLocked` linear rather than quadratic. |
| **MC-24** | Stop C. Delete 5000 files on A. Restart C with its cursor still valid. | Feed replays the deletes; C converges. No sweep involved. | The normal offline-catch-up path. |
| **MC-25** | Stop C. Delete 5000 files on A. Kill C's cursor (write garbage into the `cursor` bucket of C's state DB — Drive answers a malformed token with 400/`pageToken`, which classifies with 410 as `ErrCursorExpired`). Restart C. | **Confirmed in Tier A**, exactly as predicted: `[pull] cursor expired: re-enumerating` → the sweep infers more deletes than the cap → **`[sweep] REFUSING to delete`, whole pass abandoned** → C stays diverged with every stale copy intact. Then the part the prediction missed: restarting with a raised cap does **nothing**, because the refused pass still recorded itself as a *completed* sweep, so there is no cursor to recover, no interrupted sweep to resume and (until `-sweep-interval`) no schedule. Recovery is `-resync` **and** the raised cap; the refusal message now says so. | The most likely real-world operational stall in a multi-client deployment: the guard is correct, and the recovery was manual, two-part, and misdescribed by the message that told you to perform it. |
| **MC-26** | Rename one file on A: `x.txt` → `y.txt`. | **Confirmed in Tier A, and fixed.** Drive's feed reports a rename as the file at its *new* name; `provider.RemoteChange` carries no identity, so `toRemoteChange` yielded a create at `y.txt` and nothing removed `x.txt`. `gdrive.vacatedPathLocked` now emits the removal for the path the object left, derived from the in-memory reverse map — not the persistent index, and not for directories (see DESIGN.md §2.5 for why both). A **folder** move is still left to the sweep; that half of the case is still open against real Drive. | A rename silently duplicated every renamed file on every other client for up to `sweep-interval` (default **24h**). Invisible to single-client testing: a client never sees its own renames come back. |
| **MC-27** | Rename a directory holding 1000 files on A. | Same mechanism, amplified: B and C would show 2000 files, and — because the new paths have no echo — the entire subtree is **re-downloaded** on each client. Measure the transfer. | Turns MC-26 from a curiosity into a bandwidth and correctness event. |
| **MC-28** | Move a file between directories on A. | Same class as MC-26. | Confirms it is about identity, not about names. |

### Group IV — concurrency and conflict

This is the group that justifies the exercise. Use `flock`-gated start barriers so
the three actions genuinely overlap; a shell loop across three ssh-less local
processes will not otherwise line up inside a 300 ms debounce window.

| ID | Setup / action | Expected | Catches |
|---|---|---|---|
| **MC-30** | All three create `same.txt` with distinct content, simultaneously. | **Confirmed in Tier A**, exactly as predicted: three lookups miss, three creates land, and one folder holds three files named `same.txt`. `[drive] 3 remote files share the name …` appears and one content is readable. Two further facts the prediction did not contain, both now pinned: **`rm` does not empty the path** — removing the visible sibling uncovers the next-newest, so an ordinary delete is followed by that path reappearing fleet-wide with someone else's bytes; and **the sweep and the mount disagree about which sibling is "the" file** — `Enumerate` reports the path once per sibling with a different hash each time, and `reconcileRemote` applies them in listing order, whereas `lookupChildLocked` resolves by modifiedTime. Still open against real Drive: whether `files.list` returns siblings in an order that makes that disagreement stable, and what `rclone lsjson` shows beside it. | Silent data loss that no path-addressed client can detect or repair. The mitigation (create-if-absent semantics, or a post-create dedup) is a design decision this test should force. |
| **MC-31** | A and B both modify an existing `shared.txt` within the debounce window. | One wins by mtime; the loser is preserved as a local-only conflict copy on exactly one client. Winner's content identical on all three. **No file's content is lost anywhere.** Trees legitimately differ. | The core §6 path under real concurrency. Assert on content preservation, not on tree equality. |
| **MC-32** | As MC-31, but skew mtimes with `touch -d` (B's copy 1h in the future), then let it run for 10 minutes untouched. | Last-writer-wins picks the future-stamped file. **The assertion is that push/download counts stop growing.** Linear growth = a livelock: A thinks it is newer, pushes; B thinks it is newer, pushes; forever. Note `resolveConflict` treats *equal* mtimes as "local newer", which is exactly the tie a shared filesystem clock can produce. | The ping-pong failure mode. §4's loop breaker (identical content is a no-op) should prevent it; this proves it. |
| **MC-33** | A deletes `x`; B modifies `x` concurrently. | Define and document: B's push either recreates `x` under a new file ID (resurrection) or fails and falls through. Whichever it is, no other file may be damaged and the fleet must settle. | Delete/edit races are the commonest real-world conflict and are entirely unspecified today. |
| **MC-34** | A renames `x`→`y`; B modifies `x`. | B's push resolves `x` by name, misses, and creates a new `x`. Expect both `x` and `y` remotely. | Compounding of MC-26; also a second route to MC-30's duplicate-name situation. |
| **MC-35** | A creates directory `n/`; B creates file `n`. | Drive permits both; POSIX does not. Record what each client ends up with. | A structural collision the seam cannot represent. |
| **MC-36** | All three write 1000 distinct files each into one shared directory, concurrently. | 3000 files everywhere, no losses, no duplicates. Record 403 rate, retry counts, convergence time, and the peak fleet request rate against the quota ceiling. | The throughput and quota-contention test. Expect this to be where rate limiting first bites. |
| **MC-37** | Alternate writes to one file A→B→A→B for 5 minutes, ~10s apart (wider than convergence, so each write starts from a converged state). | Converges after every write; total conflict copies ≈ 0. | Sequential hand-off, which is the realistic "two laptops, one document" workflow, as distinct from the simultaneous case. |

### Group V — failure, restart, degradation

| ID | Setup / action | Expected | Catches |
|---|---|---|---|
| **MC-40** | `kill -9` A mid-upload of a large file. Restart. | **Run 2026-10-13**, two `drivel` processes over a local sshd (the MC-47 rig; Drive's resumable-session half is still unrun — see below). It found three defects, and the one the case was written to look for was there: **the backend process outlived its host indefinitely.** go-plugin ends a plugin by having the host call `Client.Kill`, and `kill -9` runs no code, so the child went on holding the account's credentials and its connection to the provider with nothing driving it — one more per crash-restart cycle, still alive 11 minutes later. Fixed by a liveness pipe the host holds for its own lifetime (`plugin/liveness.go`); pinned by `TestABackendExitsWhenItsHostLetsGoOfThePipe`, which fails in 11s against the unguarded `Serve`. **The destination path is never left holding a partial file** — M18 rule 7's write-to-temp-then-rename, confirmed live from both sides: killed mid-transfer at 161 MB of 537 MB, the remote still held the previous content byte for byte. **A new file recovers on the next sweep and converges**, by the local walk's "no echo, therefore new locally" row, byte-identical, with no duplicate — and the duplicate half of this case is **Drive-specific by construction**, since a path-addressed backend cannot grow a second object at one path. The stranded temporary is now the normal outcome of a crash rather than a rarity, because the child exits promptly instead of unwinding; `Enumerate` skips the prefix so it is invisible, but nothing reclaims it, and that is the open follow-up the fix created. | Crash-consistency of the push path. |
| **MC-40 (b)** | The same kill, but mid-upload of a *rewrite* of a file that is already synced. | **The divergence is never repaired, and this is M27's blind spot rather than a regression.** Local holds the rewrite, the remote holds the previous content, and 44 sweeps over 7 minutes — past `repairDelay` — changed nothing: no push, no conflict copy, no repair record. **A crash is strictly worse than an outage here.** `Engine.oweRepair` runs when a push *gives up*, which is code; `kill -9` runs none, so the repair bucket is empty, while `pushLocalOnly` skips the path for having an echo and the remote pass matches that echo and correctly does nothing — the three-way silence M27's own entry describes, reached by the one route M27 does not cover. Recording the debt *before* the push would close it and is declined for the reason DESIGN.md §12 gives: it is the persistent outbound queue, and it doubles the commit count of every push. The cheaper candidate, and the one to weigh first, is to let the local walk consult the baseline fingerprint it already stores — `state.Echo.LocalSize`/`LocalMTime`, which `matchesBaseline` reads for the delete rows — so that a path whose local file no longer matches its own baseline is pushed rather than skipped. No new state, no extra commits, one `lstat` per file per sweep on a walk that already visits every file, and it would also have covered MC-44b without a repair bucket at all. Unscheduled; see §4's item. | That M27 repairs an abandoned push and not a lost one. |
| **MC-41** | `kill -9` C mid-sweep (during MC-01's initial enumeration). Restart. | **Confirmed in Tier A**, as predicted and with the prediction's mechanism intact: a sweep paged at 2, held at its first page boundary and then stopped, leaves a sweep record carrying the page-two cursor; the next start logs `[sweep] resuming the enumeration interrupted at …` and completes over **exactly the four objects it had not reached**, deleting nothing on either side. That count is the assertion that matters — a sweep that silently restarted would list all six, mark every one of them seen, and never consult a mark. Giving the resumed sweep a fresh generation (an in-memory seen-set, in one line) deletes exactly the two files the dead run had already consumed, locally, reporting `2 deleted locally` as the ordinary resolution of a bulk remote delete. Pinned by `TestFleetResumesAnInterruptedSweepWithoutInferringDeletes`. Still Tier B: the literal `kill -9`, and a sweep large enough for the resume to matter in wall-clock. | The M7b resume path — an in-memory seen-set would delete the user's data here. |
| **MC-42** | Delete C's `drivel-index.db` while stopped; restart. | Degrades to `files.list` name lookups. **No duplicates created, no data lost** — only latency and quota. **Answered below the seam and not in the fleet**: the index is Drive-private and a path-addressed fake has none to lose, so the regression tests for it are `TestResolutionWorksWithNoIndexAtAll`, `TestResolveFindsExistingRemoteFileWithColdIndex` and `TestStatFindsRemoteFileAfterRestart` in `internal/provider/gdrive`, which are where an index-shaped mistake is visible at all. Still Tier B: what the degradation *costs* against real Drive, which is the half nobody can fake. | M7 invariant 2. Before M7, this produced duplicate uploads beside real files. |
| **MC-43** | With C in `-lazy` mode holding placeholders, delete C's `drivel-state.db`; restart. | **Confirmed in Tier A, in two shapes, and they do not exercise the same guard.** With the remote intact, the post-loss sweep re-stamps every placeholder and records it a fresh echo: no push, no conflict copy, the remote byte-identical, and a read through the mountpoint still hydrates the real content — so the marker that survived is a working promise and not a label. The guard doing the work there is `applyTo`'s re-stamp branch; removing it manufactures a conflict copy per placeholder and leaves the *placeholder* at the real path with the user's content parked beside it. Removing the **local walk's** placeholder skip changes nothing in that shape, because the sweep applies its pages before it walks, so every placeholder already has a baseline by then — which is why the second shape deletes one file remotely while the client is down. Then the page never mentions it, there is no re-stamp and no echo, and the walk's own check is the only thing in front of a push; without it the sweep offers the placeholder and the engine's M5 gate refuses it, which is two independent guards collapsing into one and a fallback. Pinned by `TestFleetKeepsPlaceholdersSafeWhenTheStateDBIsLost` and `TestFleetNeverPushesAPlaceholderWhoseRemoteIsGone`. | M5 invariants 1 and 2 — the data-loss guards. Run with `DRIVEL_REQUIRE_TESTENV=xattr` semantics in mind: on a filesystem without user xattrs this case is meaningless, and `app.Mount` warns as much. |
| **MC-44** | Block UDP/443 for A only (`nft add rule … udp dport 443 drop`) for 10 min under load, then restore. | **Complete, run 2026-10-13: both fallback shapes (a local drop and a true blackhole) and the throughput comparison against live Drive.** The fallback is better than the case assumed and the *restore* was worse. **It fires on the very next request** — the last HTTP/3 response at 18.9s, UDP/443 dropped, the next request at 21.1s carried over HTTP/2 in 203ms, no error reaching the caller and no request lost — so "under load rather than at startup" has the same answer as at startup. One caveat on that number: an output-hook `drop` is reported to the local sender as `EPERM`, so the failure is immediate. **The blackhole shape then ran the same day** — two network namespaces across a veth with the drop on the *server* side's input hook, so the client's packets leave normally and nothing ever replies, which is the one thing cgroup scoping cannot express — and it has **two answers six times apart, where these notes predicted one**. A block arriving **mid-session costs 30.1s**: the in-flight request is on an established connection, nothing tells quic-go the peer has stopped hearing it, and it retransmits until the idle timer fires — `MaxIdleTimeout`, which the transport had left to quic-go's `protocol.DefaultIdleTimeout`. A block **already in place at startup costs 5.0s**, which is `HandshakeIdleTimeout`, and so does every re-probe, because by then there is no connection left and the attempt is a fresh dial — a second run held the blackhole for a full 320s and measured 5.02s at startup, 5.01s for the single re-probe, and 0ms for the other 101 requests, i.e. 1.7% of the wall clock at a 3s cadence. The predicted number was the right one for the wrong shape. Either way the request *completes* over HTTP/2 with no error above the transport and nothing retried: exactly one request of 131 in a 433s run exceeded 100ms. The fix is a pin rather than a change — `h3HandshakeIdle`/`h3MaxIdle`/`h3KeepAlive` are now named constants asserted by `TestNewPinsTheQUICTimeouts`, with `h3MaxIdle` set to the 30s already in force, so a dependency bump cannot move a measured and documented property silently; lowering it is the only direction with teeth (the live value is `min(ours, the peer's advertised max_idle_timeout)`) and it costs keep-alive packets on every healthy mount, since the ping interval is `min(KeepAlivePeriod, idleTimeout/2)`. **Restoring UDP/443 did not bring HTTP/3 back**: all 15 requests in the 29 seconds after the rule came off stayed on HTTP/2, and would have for the life of the process, because `fallback.down` was a `map[string]bool` written once and never cleared. For a mount that runs for weeks out of `/etc/fstab` that is one blip permanently downgrading the transport §2.6 makes a requirement. Fixed with a re-probe window (`h3Retry`, 5 min): the mark now records *when* HTTP/3 was last found unusable, `isDown` treats an older one as lapsed, and only a failure re-arms it — so recovery needs no second mechanism, which a mutation proved by leaving a "clear it on success" path with nothing to assert. Verified live at a 20s window: HTTP/3 returned 12.5s after the restore, on the first request past the re-arm — and again at the 5 minutes that ship, where the mark landed at t=45.1s, the rule came off at t=330.0s and HTTP/3 returned at t=345.4s. **Recovery latency is therefore "the rest of the current window", up to five minutes, and not a function of when the network came back**, which is what to compute before reading a run as the fix not working. Pinned by `TestHTTP3IsReprobedAfterTheRetryWindow` and `TestNewConfiguresTheRetryWindow`. **The throughput half then ran against live Drive** — five alternating pairs of a 64 MiB upload and download through `gdrive.Put`/`Get`, protocol forced by dropping UDP/443 for the run's cgroup and *attributed* by nftables counters rather than assumed (every HTTP/2 run read 0.0 MiB on udp443 and ~66 on tcp443, every HTTP/3 run the reverse) — and it splits in two. **Throughput is indistinguishable because Drive is the limiter**: upload medians 4.16 against 4.07 MiB/s with overlapping ranges and 64 MiB taking 15–17s on every run of either protocol, and download medians 33.46 against 30.83 MiB/s whose per-run spread (25.50–53.98 against 14.80–43.31) **varies twofold between consecutive runs of one protocol**, so that column measures Drive's minute and not the transport. That variance is the methodological finding: one sample each would have read as a clean 2:1 win for whichever got lucky, which is why the harness alternates and takes a median of five. **CPU is where they differ, by an order of magnitude**: upload 2.00 against **20.50 ms/MiB**, download 6.10 against 13.10, and unlike the throughput columns these do not overlap at all across ten runs (1.9–2.0 against 20.2–22.8; 6.1–6.9 against 12.6–15.0). The download figures carry the probe's own MD5 as a constant common to both, so the difference — 18.5 ms/MiB up, 7.0 down — is the protocol's and the ratio understates it; in absolute terms 1.3s of CPU per 64 MiB upload, ~8% of a core at the 4 MiB/s Drive allows against 0.8%. This **prices** §2.6's requirement rather than overturning it: that rationale is connection reuse, 0-RTT and head-of-line blocking across *many small requests*, which a two-stream bulk transfer does not test — but there is no bulk-throughput advantage to point at on this path, so the case for HTTP/3 has to be made on the poll loop. The earlier public-endpoint numbers (384 KB bodies, HTTP/3 66–117ms against HTTP/2 58–88ms) measured request rate and should not be quoted beside these. | The transport fallback under load rather than at startup, plus the per-client cost of each protocol. |
| **MC-44b** | Block **all** traffic to Drive for one client for 20 min, then restore. | **Confirmed in Tier A, and "converges on restore" is wrong — but less wrong since M27 (2026-10-12), which is folded in below.** Three of the four predictions hold: no crash, no local data loss (the write during the outage lands exactly as it would online — §2.1 is what makes the outage invisible to the user, and also what makes the loss invisible), and **inbound recovers on its own**, bounded by the idle cadence rather than by anything about the outage, since one failed poll sets the wait to 30s and the loop then retries indefinitely. Outbound does not: the push in flight gets five attempts over a few seconds of jittered backoff and is then **dropped** — `[sync] push write … failed after 5 attempt(s)` — so after the network returns the file exists only on that client, the feed is demonstrably live again, and nothing ever offers the file to the remote. **M27 changed the tail of this and not its head**: the abandonment is unchanged and still happens within seconds, but the path is now recorded (`(recorded for repair by the next sweep)`), a repair sweep is scheduled ~5 minutes out even at `sweep-interval = "0"`, and **an ordinary restart repairs it with no `-resync`** — which is what the case now asserts, along with the record surviving the restart in bbolt and the `[sweep] repush` line that sends it. The old claim here, that recovery was `-resync` and nothing else, was true when written and is not now. What has *not* changed: for the few minutes before the repair, the file exists only on that client and the fleet does not converge. Pinned by `TestFleetAbandonsAPushAcrossAnOutageAndRecoversOnlyBySweeping`. Still Tier B: whether the cursor survives 20 real minutes. | Partition tolerance; distinguishes "offline" from "broken". |
| **MC-45** | Point two clients at the *same* `token.json` and run a workload across a refresh boundary. | **Answered from the code rather than by running it, and the answer is that the premise does not hold**: nothing in a mount ever writes a token file. `gauth.SaveToken` has exactly one caller, `drivel login`; the provider only `LoadToken`s and hands the token to `oauth2`, which refreshes in memory and never writes back. So there is no concurrent rewrite to corrupt, and §2.1's one-token-per-client rule is belt-and-braces rather than a correctness requirement. The property is an absence and is load-bearing in two directions — it is what makes a shared token path safe, and it is what makes M26's secret-service work small (read at mount, write at login, no write-back across the plugin seam) — so it is pinned by a grep, `TestOnlyLoginWritesTheOAuthToken`, for the same reason the SFTP host-key hatch is: the mistake it guards against is a *new* caller in a package that does not exist yet. | Validates (or removes) the per-client-token requirement in §2.1. |
| **MC-46** | Fill C's `data/` filesystem to 100% during a large inbound download. | **Confirmed in Tier A for the half that matters, with one finding the case did not predict.** A transfer that stops partway leaves *nothing* at the destination path — the temp-file-plus-rename in `Downloader.download` is what does it, and writing in place instead leaves 8 bytes at the real path, which is the file the next sweep's local walk uploads as the user's content. No temporary is stranded, the remote is untouched, and a sweep after the transfer recovers is the repair. The finding: **an apply failure does not hold the cursor.** The page's error is logged and the cursor advances anyway, so the feed never re-offers that object and the file stays missing until a sweep — on Drive's 24h default that is a day, and at `sweep-interval = "0"` it is until someone restarts the mount. The case's own fault injection fails on the read side; real ENOSPC fails on the write side and stays Tier B, as does whether the error is reported rather than swallowed by the filesystem. Pinned by `TestFleetNeverLandsAPartialDownloadAndNeverPushesItBack`. | A short local file becoming the canonical remote content is the worst available outcome. |
| **MC-47** | Cross-process hygiene, deliberately: (a) two processes sharing one state DB; (b) client B's `data/` placed inside client A's `data/`; (c) B's mountpoint inside A's backing tree. | **Run 2026-10-10**, two real `drivel` processes over a local sshd, and the three shapes do not grade the same. **(a) is already guarded and the guard is the right one**: bbolt's flock refuses the second mount in 5.0s, the live mount's file is not touched and not moved aside, and the live mount keeps pushing throughout — so the answer to "is a lock file worth adding" is no, there already is one. What was wrong was the sentence: `mount b: open state db: open state db /path: timeout` names the operation twice and the cause not at all, and "timeout" is the one word that sends an operator to look at the network. Fixed, and pinned by `TestOpenOnALockedStateDBSaysWhyAndSaysItOnce`. **(b) is real, unbounded, and no component is malfunctioning** — A's walk finds B's backing tree "local only" and pushes it, B materialises it one level deeper inside A's tree, and that repeats once per sweep interval in each direction: one seeded 2-byte file became 12 local and 11 remote copies in 30s at a 3s interval, `bdata/bdata/bdata/…`. **(c) is (b) plus a broken cardinal rule**: A reads and writes *through* B's live FUSE mount, so one write became three remote paths and A's downloads landed in B's backing store — §2.7's rule generalised across processes, which is the mechanism behind the multiplication rather than a separate hazard. It does **not** deadlock (two FUSE connections, so A never re-enters its own handler). **The feared delete storm does not happen, and the reason is structural rather than lucky**: `applyDeletes` walks `EachUnseenEcho` — baselines the *remote* did not list — so a mountpoint going empty hides files *locally*, which is the "new remotely / would materialise" row and never a delete row. Verified both ways, with `materialize` on (the paths are re-downloaded, `13 reconciled locally`) and off (`2 not fetched`, remote untouched). A fourth shape, not in the original case: **B's state DB inside A's backing tree uploads one client's entire sync history to the shared remote**, torn, where every other client then materialises it. | Confirms which cross-process footguns are real, and whether a lock file or a startup check is worth adding. Answered: (a) needs no mechanism, only words; (b)/(c) need a guard that does not exist, and §4's cross-process item has the options. |

### Group VI — resource, cost, and soak

| ID | Setup / action | Expected | Catches |
|---|---|---|---|
| **MC-50** | Steady-state table: idle / light (1 write per minute) / heavy (MC-36 workload), per client. | Publish RSS, CPU%, disk read+write bytes, fd and thread counts, and Drive requests per minute for each cell. | The number the user actually asked for: cost per client per workload. |
| **MC-51** | Repeat MC-03, MC-20 and MC-23 with N = 1, 2, 3, 5 clients. | Traffic per change ≈ `1 upload + (N-1) downloads`; request rate ≈ linear in N; convergence time flat until quota throttling bends it. Find the N where 403s start. | The scaling law, and the practical fleet-size ceiling for one Drive account. |
| **MC-52** | 100 000 small files, then start a 4th client cold (full sweep) while the other three stay live. | Sweep RSS must stay bounded — pages stream, `seen` marks live in bbolt. Record sweep duration, peak RSS, index DB size. | Whether a large tree makes the sweep, not the transfer, the memory ceiling. This is where a three-client fleet would OOM a laptop. |
| **MC-53** | 24h soak: all three live, a seeded random mutation generator on each (create/modify/rename/delete, ~1 op per 10s), `sweep-interval = 2m` so the periodic sweep fires ~700 times. | RSS slope ≈ 0. Goroutine count flat (needs `-pprof`). No divergence at any hourly checkpoint. **No sweep ever deletes anything it should not.** | The periodic sweep is the most dangerous scheduled event in the system, and at the 24h default a bug in it takes a day to appear once. Compressing the interval is the only way to get statistical confidence in it. |
| **MC-60** | Mixed-mode fleet: A eager, B eager, C `-lazy`. Run MC-03, MC-20, MC-23, MC-26 and MC-31 against it. | Placeholders are never pushed; C's conflict copies are downloaded in full; a remote delete removes C's placeholder rather than leaving an `EIO` stub; hydration of a file another client is concurrently rewriting yields either version whole, never a splice. | The realistic deployment (laptop lazy, server eager) and the M5×multi-client interactions that no existing test covers. |

---

## 4. "Anything else?" — what the brief did not list

The five items you named are all in the matrix. These are the ones I would add,
roughly in descending order of how likely they are to change a decision:

1. **Rename propagation (MC-26/27).** ~~Hypothesis~~ — confirmed and fixed for
   files during the first Tier A pass: a remote rename could not be expressed
   across the seam, so every other client kept a duplicate at the old path until a
   sweep, up to 24h. Still open: the **directory** case, which is deliberately left
   to the sweep because a removal above the seam is a recursive local delete and
   Drive reports no changes for a moved folder's children. MC-27 against real Drive
   is the case that says whether that is good enough.
2. **Same-name siblings (MC-30).** **Confirmed in Tier A, and not fixed —
   deliberately, because every fix is a policy choice.** Drive's data model
   permits what the mount cannot show, and concurrent creation of the same path
   is a completely ordinary thing for three clients to do. The provider's
   behaviour is defensible in isolation (resolve to the most recently modified,
   and say in the log that the others are now invisible) and the *fleet* still
   ends up somewhere no user would predict: deleting the file uncovers an older
   one instead of emptying the path, and a sweep can leave the local file holding
   a different sibling than the one a push would update. Three candidate
   mitigations, none of them free — create-if-absent (Drive has no such
   primitive, so it is a lookup-then-create with the same window, narrowed);
   post-create dedup (someone has to lose, and the loser is content a user just
   wrote); or surfacing the siblings as distinct paths (visible, honest, and it
   makes every other client's path resolution non-deterministic). The one thing
   that should not happen is picking one silently, which is what happens today.
3. **The `-max-deletes` stall (MC-25).** **Confirmed in Tier A, and the recovery is
   now documented.** A guard that is right to refuse, on the exact sequence (client
   offline, bulk delete, cursor expiry) that multi-client use makes routine. The
   sharp edge was that a refused pass still counts as a *completed* sweep: raising
   the cap and restarting does nothing at all, and does it silently, which from the
   operator's side is indistinguishable from a fix. Recovery is `-resync` **and** a
   higher cap; the refusal message, the man page, README, DESIGN.md and CLAUDE.md
   now all say so. Two things the stalled peer does *not* do, both now pinned: it
   does not delete a subset (the pass is abandoned, not trimmed), and it does not
   push its stale copies back — they have a baseline, which excludes them from the
   sweep's local walk, so a stalled client cannot resurrect the tree for the fleet.
4. **The periodic sweep (MC-53).** Every other sweep trigger fires at startup and
   gets tested constantly. The 24h one fires when nobody is watching.
5. **Conflict copies are local-only and permanent divergence is by design.**
   Decide now whether that is the intended long-term policy for a fleet: after a
   week of ordinary conflicts, the three machines hold three different sets of
   copies and no client can see the others'.
6. **Shared quota, not shared bandwidth.** The fleet contends for one per-user
   request budget. Idle polling alone consumes a measurable share of it, and it
   scales with N before any user does any work.
7. **Cross-process guards do not exist (MC-47).** `app.Validate` cannot see other
   processes. Worth deciding whether a lock file or a startup probe is warranted
   now that "run several drivels" is a supported shape. **Answered by running it —
   see the cross-process item below and MC-47's row.** The short form: the shared state DB already has
   its lock and needed only a better sentence, and the two overlapping-directory
   shapes are the ones with nothing in front of them.
8. **Token file contention (MC-45).**
9. **Google-native documents (MC-14).** Any regression in `ExportOnly` seen-marking
   deletes real Drive documents, and three clients means three chances per sweep.
10. **HTTP/3 vs HTTP/2 cost per client (MC-44).** You have a project-level
    requirement to use QUIC; a three-client rig is the cheapest place to find out
    what it costs in CPU and throughput against the fallback, on identical work.
11. **Determinism and reproducibility.** Seed every generator, record the seed,
    keep every log and manifest under `results/<case>/<timestamp>/`, and record the
    drivel commit SHA in the result. Tier B failures are rare and you will not get
    a second chance at the evidence.
12. **A named quiescence contract.** "Converged" needs one definition used by every
    case (§2.6), or the results are not comparable across cases or across runs.
13. **The rig is code, and it was wrong in two places.** Both were found by running
    the scripts against hand-built trees with no Drive involved, and both would
    have failed *quietly* during a live run: `converge.sh` diffed the mtime column
    (item 16), so no case would ever have reported convergence; and `logcount.sh`
    passed `[sync] push` to `grep` without `-F`, where `[sync]` is a bracket
    expression matching one character out of `{s,y,n,c}` — so uploads, downloads,
    conflicts and sweeps all silently reported **0**, which is every volume number
    the campaign exists to produce. Neither is exotic and neither would have been
    obvious from a failed run. Re-run the offline smoke pass after touching any of
    these scripts: build a few fake trees and a fake log, run every script against
    them, and check the numbers by hand.
14. **The push path and the sweep disagree about reserved names (MC-12).**
    `skipLocal` is right to keep the local walk off `.drivel-*` and conflict-shaped
    names — without it a sweep re-uploads every conflict copy drivel has ever
    written, which is the §6 catastrophe — but it is the only filter in the
    system. A user file of either shape is pushed when the mount sees it created
    and is invisible to reconcile when it does not, so whether the file syncs
    depends on whether drivel happened to be watching. Neither the fleet's
    convergence check nor `manifest.sh` can see the difference, because both skip
    the same names. The narrow fix is to make the two paths agree; the honest one
    is to stop recognising drivel's own artefacts by their names at all, since a
    name is the one thing a user is free to choose.
15. **Two failures are repaired only by a sweep, and nothing schedules one —
    half answered by M27 (2026-10-12).** A push abandoned after its retry budget
    (MC-44b) and a download whose apply failed (MC-46) left the same state: one
    path out of sync, one log line, and no scheduled event that would notice.
    Both are reachable from an outage of seconds. **The push half is fixed**: the
    path is recorded in `internal/state` and the next sweep is brought forward
    ~5 minutes, which is a schedule without being an immediate sweep — and a
    restart repairs it with no flag at all. **The download half is unchanged** and
    still waits for `-sweep-interval`; an apply failure does not hold the cursor,
    so the feed never offers that object again. The decision it forced was whether
    either should *schedule* a sweep, and it is
    not obviously yes — a sweep is the most expensive thing the system does and the
    most dangerous (MC-25, MC-53), so triggering one from a transient failure trades
    a known small loss for an unknown large cost. The cheap half of the answer is
    that `-sweep-interval 0`, which §2.3 sets for every live case, turns off the
    only repair either one has; a user on the 24h default has it, slowly. Evidence
    in §5.6.

    **Two things this item had the wrong way round, found by building the fix.**
    The sweep did **not** already repair an abandoned push to a file that had
    been synced before: `pushLocalOnly` is the *new locally* row and skips every
    path with a baseline, while the remote pass matches the echo and correctly
    does nothing — so the cheap option, a bare "a sweep is owed" flag, would have
    scheduled a sweep that repaired nothing. That is why the record carries the
    path. And **MC-44b's "recovery is `-resync` and nothing else" no longer
    holds**: the record is itself a sweep trigger, so an ordinary restart
    repairs it. The case now asserts that, and the flag is gone from it.
16. **mtime is not synchronised, and nothing said so.** Found by exercising
    `converge.sh` offline before spending a live session on it: it compared the
    manifest's mtime column, which no two clients in a fleet will ever agree on
    (§2.6). The behaviour is defensible — the originator's local write time is a
    real fact about that machine, and stamping downloads with the remote's time is
    what keeps the pullers consistent and stops a fresh download from being pushed
    back — but "your files show different times on different laptops" is the kind
    of thing a user notices immediately and finds nowhere in the documentation.
    Worth deciding whether it stays undocumented, gets documented, or gets fixed
    by writing the provider's stamp back over the originator's copy after a push
    (which is a new local mutation and would have to be suppressed as an echo).
17. **Write amplification is real, but "≈ 6000×" is the wrong way to carry it, and
    both halves of the ratio mislead.** The numerator is an artefact of the rig:
    `workload.sh many` wrote `printf 'file %d of %d\n'`, about 19 bytes, where
    MC-11 specifies 4 KiB — so the recorded 190 KB should have been 40 MB, and the
    same run would have reported ≈29×. A ratio that moves by two orders of
    magnitude with the test's own file size settles no argument. (`many` now writes
    4 KiB per the spec; the recorded result stands as what happened, against
    19-byte files.)

    The denominator is the finding, and it decomposes exactly. **A bbolt commit
    costs ~36 KB of disk writes on a 10k-entry DB, whatever the key and value are,
    and stays flat as the DB grows** (measured 36.0 KB/commit at 20 000 keys, 36.3
    at 40 000). Disk cost is therefore *commits × a constant*, and **it is the
    fsync that costs, not the data**: opening with `NoSync` and then calling
    `Sync()` after every commit reproduces the default's total byte for byte, while
    one `Sync()` at the end writes 5.4 MB instead of 365. Each commit fdatasyncs
    twice — once for the data pages, once for the meta page that makes them
    reachable — and so rewrites the same handful of hot pages (leaf, branch, root,
    freelist, meta) every time. `NoFreelistSync` is worth 11% and nothing else:
    page size, freelist type and value size are all noise.

    So the number to carry is **commits per synced file**. It is deterministic,
    independent of the filesystem underneath, and it is what moves when somebody
    adds a store write to a per-file path. `pathindex.Set` and `SetMany` now
    compare in a read transaction before writing — bbolt advances the meta page on
    every commit whether a data page changed or not, so storing a byte-identical
    value cost a full commit, while a `View` costs nothing. That takes a re-push
    from 3 commits to 1 and a cold resolve off a warm index from 2 to 0.

    **It does not help this case, and that is worth being precise about rather
    than claiming a win here.** MC-11 creates 10 000 files that do not exist
    remotely, and a new path's mapping genuinely is new, so its one index commit
    stays. A create-only burst is the single workload the guard does nothing for;
    what it fixes is the ordinary steady state — editing a file that already
    exists re-stored its mapping twice per push — and every restart, which
    re-stores whatever the index already held. MC-11's remaining per-file cost is
    the `state.SetEcho`, and the only lever for that one is group commit, which
    DESIGN.md §12.3 measures and deliberately does not take.

    Note also that **1175 MB cannot be attributed per-operation from the recorded
    run**, which is why no "it would now be N MB" number appears here: the figure
    is a single process total from before M9 split the two stores across two
    processes, and `results/mc11/` holds no per-store breakdown. A re-run with the
    sampler fixed (below) is what would produce one.

    §5.3's rule is why this is settled in Tier A rather than by another live run:
    the whole thing is reachable in-process, and `TestPushCommitsPerFile`,
    `TestStatThenPutCommitsTheMappingOnce` and `TestIndexHitDoesNotRewriteWhatItRead`
    pin the budgets. What is *not* settled is whether the remaining commit should
    keep its two fsyncs; DESIGN.md §12 has that decision and why it is held.

    One measurement caveat for any re-run, and it is a rig bug rather than a
    footnote: **MC-11 predates M9 by six days**, so both bbolt stores were in one
    process when the 1175 MB was recorded. Since M9 the index DB belongs to the
    plugin process and the state DB to the host, and §2.7's sampler is pointed at
    the host — so a re-run would silently under-report by roughly the index's
    share. Sample the plugin child too.

18. **Two drivel processes can be pointed at overlapping directories, and nothing
    anywhere can see it.** `app.Validate` is the guard, and it is an
    *intra*-process one by construction: it compares the specs it was handed.
    MC-47 (b) and (c) are the same mistake made across two processes, and the
    result is unbounded content multiplication — one file becoming a dozen copies
    at `bdata/bdata/bdata/…` in thirty seconds, growing by one level per sweep
    interval, forever, on the local disk and on the shared remote.

    **What makes it hard to spot is that no component is misbehaving.** A's walk
    correctly finds a file local-only and pushes it; B's sweep correctly
    materialises a remote file it does not have; the engine correctly records a
    baseline for each. The defect is the absence of a question nobody is in a
    position to ask. The two sweeps also hide it from the operator: each log line
    is an ordinary push or download, and the only tell is that the paths keep
    getting longer.

    **The options, in order of what they cost.** A *marker file* at a backing
    tree's root naming its mount, checked by walking **up** from the backing root
    at open (O(depth), cheap, and it refuses before any damage) — but it only
    catches the nested-child direction, since the parent cannot discover a marker
    below it without a full walk. The complement is free: **the sweep's local walk
    already visits every file**, so recognising a foreign marker there and pruning
    that subtree costs nothing and is also simply correct — another drivel's
    backing store is not content. Together those cover (b) in both directions.
    Neither covers (c), where the thing inside A's tree is a *mountpoint*: that
    needs the walk to notice it crossed a device boundary, which is a policy call
    about backing trees spanning filesystems and is not obviously ours to make.

    **And a fourth shape the case did not list is worse than either**: B's *state
    DB* inside A's backing tree. A uploads it — a live bbolt file, read without a
    transaction, so torn and useless as a backup — and on a shared account every
    other client then materialises a copy of one client's entire sync history,
    which is a record of every path it has ever synced with sizes and digests.
    `app.Validate` refuses exactly this in-process ("a state DB inside a backing
    tree") precisely because it is the shape that needs no second mount to be
    wrong; across processes it is unguarded. A marker file does not catch it,
    because the state DB is an ordinary file with a configurable name. The cheap
    half is that drivel knows its own state and index paths at open and can refuse
    the ones inside *its own* backing tree already — what it cannot see is
    someone else's.

    Nothing here is scheduled. It is recorded so the next session does not have to
    rediscover which of the three shapes is actually dangerous, and so that
    "unguarded across processes" is not mistaken for "undetectable".

19. **One `cp` of a large file costs two full uploads, and this is what MC-11's
    extra upload was.** §5.3 below records a single duplicate in the 1000-file
    seed run and sends it to Tier A to be chased as a coalescer question. MC-40's
    rig answered it instead, by using a file big enough to make it happen every
    time: a 512 MiB copy logged `[sync] upload dbl.bin (536870912 B)` **twice**,
    three seconds apart, and the remote ended correct.

    The mechanism is not a race and there is nothing to fix in the coalescer.
    `OpCreate` is emitted when the file is created and `OpWrite` when the handle
    closes, so the debounce window merges them only for a write that *finishes*
    inside it — 300ms by default. Anything slower is two dispatches, and the first
    one pushes a file that is still being written: `pushContent` opens it, `Put`
    streams to EOF, and because the writer keeps extending it the whole file goes
    across under an event that described a fraction of it. So the cost is not
    "a partial upload plus a real one" but two complete ones, and it scales with
    every file whose write outlives the window — which is every large file.

    **The measurement was lying about it, and that is fixed.** The byte count on
    an upload line came from the `Stat` taken before the transfer, so the first of
    the two uploads reported 372 MiB of a 512 MiB file and read like a partial
    push. It now counts what the store actually read (`countingReader` in
    `pushContent`), which is what §2.7 says that line is for; pinned by
    `TestUploadLineReportsTheBytesActuallySent`. Any volume figure taken from a
    log written before 2026-10-13 under-reports a growing file.

    What to do about the doubling is a design question and is **not** scheduled.
    `OpCreate`'s content push cannot simply be dropped: it is what creates an
    empty file, so `touch` would stop syncing until something else touched it,
    and push-on-close means a handle held open emits nothing at all. The shape to
    consider is the mount declining to push content for a create whose handle is
    still open, which makes the event pair one event and is a `vfs` change rather
    than an engine one.

20. **A crash and an outage are not the same failure, and M27 only covers one.**
    MC-44b's abandoned push is recorded for repair because giving up is code that
    runs; MC-40 (b)'s is not, because `kill -9` runs none. The two then diverge
    permanently: a rewrite of an already-synced file has an echo, which is what
    `pushLocalOnly` skips on and what the remote pass matches, so no row of the
    reconcile table proposes it. Measured over 44 sweeps.

    This is worth separating from "M27 is incomplete". M27's entry says plainly
    that it records a path when a push *abandons*; the gap is that a sweep has no
    way to notice a local file that stopped matching its own baseline, which is
    strictly more than the crash case needs — it also covers a push lost to a
    power cut, an OOM kill, and a backing file written while the mount was down.
    The fingerprint for it is already stored (`state.Echo.LocalSize`/`LocalMTime`,
    added for the hashless-provider case in M18) and already consulted by
    `matchesBaseline` for the delete rows. Extending the walk to consult it for
    the *push* row is the candidate; what has to be settled first is what it means
    when the fingerprint says "not recorded" (a zero `LocalMTime` must never read
    as "matches", the same rule M18 item 12 states for deletion), and whether a
    sweep is allowed to push a file whose baseline it cannot verify.

---

## 5. Sequencing

1. **Prerequisites — done.** `memStore` sibling hooks; the manifest, sampler and
   convergence scripts (smoke-tested offline, see §4.13); the `-pprof` flag.
2. **Tier A first — done.** MC-03, MC-12 (reserved names), MC-21, MC-23, MC-25,
   MC-26, MC-30, MC-31 and MC-32: `go test -race -run TestFleet ./internal/app` and `-run
   'TestChanges|TestSimultaneous|TestRemovingTheVisible|TestEnumerateReportsEvery'
   ./internal/provider/gdrive`. Everything remaining in the matrix needs a real
   Drive, which is what Tier B is for.
3. **Tier B smoke — done. MC-01, MC-02 and MC-03 all pass.** The rig is built and
   lives outside the repo at `~/drivel-rig` (`rig/c1…c3`, `rig/oracle`,
   `rig/results`), pinned to a dedicated folder ID, one token per client, three
   real processes against real Drive. Results land in
   `rig/results/<case>/<timestamp>/` with the logs and the commit SHA, per §4.11.

   **MC-03 passes, and it is the case that found the missing upload log.** The
   first live run converged three clients and reported *zero* uploads, because a
   successful push logged nothing at all — from outside the process an upload that
   worked and one that never happened looked identical, which made every
   transfer-volume number §2.7 asks for unmeasurable. Fixed first, then re-run:
   one `[sync] upload mc03.bin (1048576 B)` on c1, exactly two `[pull] download`
   lines, ~2.7s from the upload line to both downloads, no conflict copies and no
   retries. That is the N-1 download rule and the baseline fan-out latency,
   measured rather than assumed.

   **MC-01 passes on every assertion, and two of its numbers are worth keeping.**
   Two clients started cold — empty backing dir, no state DB, no index — took
   1023 objects from a 3-page enumeration (0.75s) and materialised 1002 files and
   21 directories each in 400s, converging with c1 in 13s of `converge.sh`, zero
   conflict copies and zero deletes. The first number: **pages 1 and 2 resolved
   nothing under the mount root and page 3 resolved all 1023**, which is the
   parent-parking path doing real work rather than a listing that happened to
   arrive parent-first. The second: **`1023 reconciled locally, 0 pushed`** — a
   cold client must not push a byte back at a tree it has only just learned, and
   this is the run that says it does not. c1 meanwhile stayed at five log lines
   while two peers rebuilt its tree: no echo storm, nothing re-pushed. Peak RSS
   30 MB per puller against 53 MB transferred, `hwm == rss`, so downloads stream.

   **MC-02 passes with room to spare, and turned up the number this plan was
   missing.** Over 1827 idle seconds: RSS drift **negative** on all three clients
   (−176 kB, −1.7 MB, −2.0 MB — the target was under +2 MB, and what actually
   happens is the runtime handing back the sweep's working set), HWM never moving
   off its start value, CPU ≈ **0.007%** against a < 1% target, fds and threads
   flat, and **zero log lines on any client inside the window**. Goroutines were
   23 per client and sat at 24/27/27 hours later — bounded, though MC-53 is what
   actually answers that.

   Then a container DNS outage at 23:48, well after the window, gave an unplanned
   MC-44b: one `[pull] changes: … lookup www.googleapis.com` line per client per
   30s attempt for eight minutes, then silence. No crash, no fd or goroutine
   growth, nothing touched locally, cadence unchanged. But **silence after an
   outage is indistinguishable from a wedge**, because the pull loop only logs
   failures — §2.6's "content equality, not log silence" arriving in practice — so
   it was probed: 1 MiB on c1, one upload, exactly two downloads, byte-identical
   on all three, in **21s**.

   Keep that 21s next to MC-03's 2.7s. Same case, same file, and the difference is
   that a fleet idle for hours has backed its cadence off to 30s: **fan-out
   latency to an idle peer is bounded by the poll cadence, not by transfer time.**
   2.7s is a busy fleet; ~15–30s is what a user's second laptop actually sees.
   The design intends both and the matrix quotes only the first, which makes MC-03
   read as three times faster than the experience it describes. MC-51's scaling
   law should measure warm and idle separately for the same reason.

   **One thing the seed turned up that no case predicted.** Seeding MC-01's tree
   (1000 files × 50 KiB across 20 directories, ~1.15s per file from one client)
   produced **1001 upload lines for 1000 files**: `seed/d16/f13.bin` went up
   twice, seven seconds and seven other files apart, with no error line anywhere
   in the log. It is not a retry. It is a second event for a path whose first push
   had already left the debounce window, re-uploaded whole because at 50 KiB the
   file sits below `hashSkipMinSize` and so never reaches M6's unchanged-content
   gate — the gate that would otherwise have said "remote already holds these
   bytes". One extra 50 KiB in 51 MB is nothing; the shape is not, because MC-11
   is this same burst ten times larger and MC-22 measures precisely this
   amplification. Watch the ratio there rather than the count here.
4. **Tier B scale and semantics — MC-11 done, and it moved two other cases.**
   Groups II and III, ordered by what the hardware allows rather than by case
   number. **MC-11 passed on 2026-09-06** in the `mc11-burst` scenario (the first
   use of one-rig-per-scenario, §2.2.1) — full write-up in that scenario's
   `results/mc11/`. Two things it changes downstream: the quota hunt moves to
   MC-36/MC-51, because a single writer cannot burst past ~1 request/s; and
   **MC-52 should measure enumeration against account size deliberately**, since a
   cold client lists every object the account can see before filtering to the mount
   root, so each scenario's leftovers tax every later scenario's startup. Delete a
   scenario's Drive folder once its results are recorded.

   The remaining overlay cases go next — MC-21 and MC-12's remainder (minutes
   each), then MC-20 and MC-22. The bulk
   cases (MC-10, MC-10b, MC-52, MC-46) wait for the SSD that is replacing the
   external disk: running them on magnetic media means running them again
   afterwards to get a number worth quoting.

   Group III is run as **observation only** — MC-26b's folder move, MC-27, MC-28,
   MC-33, MC-34, MC-35. Record what happens and write it down; implement no
   mitigation. That is the treatment MC-30 got and for the same reason: each
   candidate fix loses something a user wrote, and the choice is the user's to be
   told about rather than ours to make silently.

5. **Tier B concurrency**: Group IV, with the start barrier.

**One thing MC-11 is *not* the instrument for.** The seed's extra upload (§5.3,
`seed/d16/f13.bin` pushed twice) reads like a case for MC-11 to amplify, and the
log says otherwise. Across the whole seed run there were zero retry lines, zero
errors and zero gate-3 skips, uploads were serialised at 1.18 s apiece — Drive
round-trip bound — and the duplicate landed 7 seconds and five other uploads after
the first. Drive's only role in it was pacing; the second event arose in the local
event/debounce/dispatch path, which is in-process and needs no network. That makes
it a **Tier A** question, reproducible against a fake store at thousands of
iterations per second under `-race`, instead of one instance per thousand files at
1.18 s each. Chase it there. Keep MC-11 as the burst-throughput, quota and
DB-growth measurement it is specified to be, and let its upload-count ratio be a
corroborating observation rather than the experiment.
6. **Failure injection — Group V done.** **MC-40 ran
   2026-10-13**, over the same two-process local-sshd rig as MC-47, and its row
   has what it found: three defects, of which the one the case was written to look
   for — a backend process outliving its host — was real and is fixed, and the one
   it did not predict is that **a crash is not an outage**. M27 records an
   abandoned push because abandoning is code; `kill -9` runs none, so MC-40 (b)'s
   divergence is permanent where MC-44b's repairs itself in about five minutes.
   Read the two rows together or the asymmetry is invisible. A failed *download*
   is still repaired only by the next scheduled sweep; M27 did not touch that
   half either. Six of the eight
   cases are in `internal/app/failure_test.go` (MC-41, MC-43 in two shapes,
   MC-44b, MC-46) with MC-45 answered from the code and MC-42 answered below the
   seam. **MC-47 ran for real on 2026-10-10** — two `drivel` processes over a local
   sshd, which is a shared remote with no quota and no account — and its row has
   what the three shapes actually do; the one thing it changed in the product is
   the message a locked state DB produces, and §4's cross-process item holds the
   guard options it declined to build. **MC-44's transport half ran the same day** and its row has
   all of it — the fallback fires on the next request, the *restore* did not work
   at all until this fixed it, and the blackhole shape that the first run could not
   express was then run in a pair of network namespaces and came back with **two
   latencies rather than the one these notes predicted**: 30.1s when the block
   arrives mid-session, 5.0s when it is already there, because an established QUIC
   connection waits out the idle timer where a dial waits out the handshake.
   **Its throughput half closed the group the same day**, once the rig's logins
   were renewed and its Drive folder rebuilt: HTTP/3 and HTTP/2 are
   indistinguishable in throughput because Drive is the limiter, and HTTP/3 costs
   **ten times the CPU per MiB uploaded**. `scripts/multiclient/mc44-throughput.sh`
   is the harness and `scripts/multiclient/README.md` has all three procedures —
   the cgroup scope, the two-namespace blackhole rig, and the throughput pair. Eight things came out
   of the group that no case predicted, in descending order of how much they
   matter.

   **The inbound loop was not part of the shutdown drain, and that is now fixed.**
   `Mount.Run` waited for the engine's bounded drain and then returned, leaving
   `down.Run` to finish on its own — so `Close` shut the state DB under a live
   sweep. It surfaced as `[sweep] delete pass: database not open`, which abandoned
   the delete pass and skipped `FinishSweep`, so a sweep that had done all its work
   was parked as interrupted and resumed on the next start. The sharper version
   never fired here and is why this is a fix rather than a log-noise cleanup: in
   in-place mode the backing path is `/proc/self/fd/N`, and once `Close` has closed
   that descriptor the number can be reused — a download still writing through it
   writes somewhere else entirely. `Run` now waits for the inbound loop before
   `close(events)`, bounded at 10s (shorter than the engine's 30s: a sweep that
   does not finish is resumed and a download that does not finish is retried, so
   neither is worth delaying a shutdown for).

   **An abandoned push and a failed download are both repaired only by a sweep**,
   and both are reachable from an outage of seconds. MC-44b and MC-46 arrive at
   this from opposite directions — the push has a five-attempt budget and then
   drops the event, the download has no budget at all because the cursor advances
   past a failed apply — and the consequence is the same: one path out of sync,
   one log line, and nothing scheduled to notice. Which is why §2.3's
   `sweep-interval = "0"` for the live rig is a *measurement* choice that disables
   the only repair either one has; a user on Drive's 24h default has it, slowly.
   Whether a dropped push or a failed apply should schedule a sweep is a design
   question this exercise is now entitled to ask, and the answer is not obviously
   yes: a sweep is the most expensive thing the system does and the most dangerous
   (MC-25, MC-53), so triggering one from a transient failure trades a known small
   loss for an unknown large cost. **DESIGN.md §9/M27 part 2 is where that question
   now lives, and it shipped keeping this paragraph's objection**: nothing starts
   a sweep on the spot. The abandoned path is recorded in `internal/state` and
   the *next* sweep is brought forward to ~5 minutes, backing off to 80 minutes
   if its repairs keep failing — the repair without the risk.

   One correction this exercise earns, because it is the opposite of what the
   plan assumed: **the sweep did not already repair an abandoned push.** The
   cheap option — a bare "a sweep is owed" flag — would have scheduled a sweep
   that repaired nothing, because `pushLocalOnly` skips every path that has a
   baseline and an abandoned push is almost always a rewrite of a file that has
   one. Measured before the fix, and now a regression test. Read that as a
   warning about this document's other "the sweep will catch it" claims: the
   sweep catches what its rows describe, and the rows are narrower than the word
   suggests.

   **A lazy peer is invisible to the fleet's convergence check**, because the check
   hashes the backing tree and a placeholder holds zeros. Nothing is wrong with
   either half; it means MC-60's mixed-mode fleet needs a convergence oracle that
   asks the hydrator, and that is a harness item to build before MC-60 rather than
   during it.

   **A backend process outlived its host, holding that account's credentials.**
   go-plugin ends a plugin by having the host call `Client.Kill`, and a host killed
   by a signal never reaches that line; nothing on the plugin side watches, because
   go-plugin has no mechanism for it — the child inherits the host's own stdin
   rather than a pipe to it, and a gRPC server keeps listening after its last
   client disconnects. So MC-40's `kill -9` left `plugin-serve` running
   indefinitely, one more per crash-restart cycle. Fixed with the pipe M16 already
   uses for its daemonize handshake; §4's item has the alternatives and why
   `Pdeathsig` is not one of them.

   **A transient UDP block downgraded the transport permanently.** The HTTP/3
   fallback remembers a host it could not reach over QUIC, which is necessary —
   without it every request pays a failed dial — but the memory had no end, so
   forty seconds without UDP/443 left the process on HTTP/2 for the rest of its
   life. Over an fstab mount that is weeks. Fixed with a re-probe window; MC-44's
   row has the measurement and why only a failure re-arms it.

   **How long the fallback takes was a dependency's default, and it is 30s rather
   than the 5s everything here claimed.** A blackholed UDP path is waited out, not
   reported, and which timer does the waiting depends on whether a QUIC connection
   already existed: `MaxIdleTimeout` (left unset, so quic-go's 30s) for a block
   that arrives mid-session, `HandshakeIdleTimeout` (5s, which the transport does
   set) for one already in place and for every re-probe. The request still
   completes over HTTP/2 and still loses nothing — one request of 131 in a 433s run
   exceeded 100ms — so what was wrong was the documentation, and the code change is
   a pin: all three timeouts are named constants now, with `h3MaxIdle` at the value
   that was already in force so that nobody's dependency bump moves a number four
   documents quote.

   **One `cp` of a large file costs two full uploads**, which is §4.19 and is also
   the answer to §5.3's open question about MC-11's extra upload. The byte count on
   the upload line was taken before the transfer rather than measured by it, so a
   log written before 2026-10-13 under-reports any file that grew during its own
   push.

   **An eager SFTP mount without `materialize` is upload-only, permanently**, and
   MC-47's rig found it by being configured the way the docs describe. Two
   processes pushed their files and neither received the other's; the sweep said
   `1 not fetched (add -materialize to download them)` every three seconds, which
   is the product reporting itself correctly. The setting gates the sweep's
   "new remotely" row only, so on a backend with a live feed it governs the content
   that was already there at first mount and nothing else — and on a feedless one
   it governs *everything* inbound, forever. `docs/user/sftp.md` said nothing about
   it; it does now. Worth carrying into M19–M21, which inherit the same shape.
7. **Resource and soak**: Group VI, last, because MC-53 wants a stable build.

   **The load is wall clock and storage, not API quota**, which is worth settling
   before the rig is rebuilt for it. Scaling from MC-11's measured 1.09 uploads/s:
   MC-52's 100 000 files is ~25h of uploading on the writer plus 100 000 downloads
   on each puller, and enumeration lists the *account* so a cold sweep is ~100+
   list requests at 1000 objects a page; MC-53 is three clients at ~1 op/10s ≈ 26k
   ops/day ≈ 78k requests, plus ~700 sweeps per client at `sweep-interval = 2m`.
   Peak request rate stays around 1–3/s throughout, and **MC-11 recorded zero 403s
   at that rate** — a single writer cannot burst past ~1 request/s, which is why
   the quota hunt moved to MC-36/MC-51 in the first place. Read the current
   per-project limit off the Cloud console rather than trusting a number here, but
   nothing in this group approaches it.

   **A fresh Cloud project and OAuth client is not required, and is still worth
   having — for measurement, not capacity.** MC-50 and MC-51 both ask for Drive
   requests per minute, which the console reports *per project*, directly, instead
   of inferring it from drivel's logs; and MC-51 is specifically trying to find the
   N where 403s start, which is a per-user/per-project limit, so sharing a project
   with a personal dogfood mount means measuring both and racing your own traffic.
   The rig's existing OAuth client is fine for the identities themselves — a fourth
   client for MC-52 is `drivel login -account c4` against the same app.

   Two constraints a new API key does nothing about. **Storage quota is per Google
   account, not per project** — and §2.1's note matters here, because `Remove`
   trashes by default and trashed objects keep counting until the trash is emptied,
   so empty it between runs rather than at the end. And MC-11's finding stands:
   enumeration scales with the account, so 100 000 objects left behind taxes every
   later scenario's startup. Delete each scenario's folder once its results are
   recorded.

**Exit criteria.** Every case in Groups I–IV either passes or has a filed issue
with a Tier A regression test reproducing it; §3's log-derived counters are
published for every case; and §6's cost table exists with numbers for N = 1, 2, 3.
