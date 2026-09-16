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
`internal/provider/gdrive/changes_test.go` covers the change feed's shape, on a
`fakeDrive` that now serves a real `changes.list` log, and
`internal/provider/gdrive/siblings_test.go` covers MC-30 below the seam.

Tier A owns: convergence, echo suppression across clients, conflict-copy
semantics, rename propagation, delete inference, the reconcile guards, ping-pong
detection.

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
4. **One token per client.** `token.json` is rewritten on refresh; three processes
   sharing one path race on that write. Give each client its own config directory
   (`drivel login -account c1|c2|c3`) or its own copy of the token file. MC-45
   exists to confirm this is actually necessary; assume it is until then.

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
| `failed after .* attempt` | **Pushes abandoned — data not sent.** `executeWithRetry` allows 5 attempts over ~7.5s of jittered backoff and then *drops* the event rather than requeueing it, so the local file is correct and the remote never hears about it. Only an M7b sweep repairs that, and §2.3 runs every case with `sweep-interval = "0"` — meaning a throttled run diverges silently while every volume counter still looks healthy. Counting the retry line without this one is the wrong asymmetry: the retry is the recoverable event |
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
| **MC-11** | 10 000 × 4 KiB files across 100 directories, created on A. | All three converge. Record: wall clock, API request count, 403/retry count, peak RSS, state DB and index DB size per client. **PASS live** (§5): 10 000 uploads, 10 000 downloads each on c2/c3, 0 conflicts, 0 duplicates, 0 abandoned pushes, **0 403s**; 2 h 32 m at a flat 1.09 uploads/s, converging 6.9 s after the last upload; peak RSS 112 MB on the writer and 39 MB on each puller; both DBs 32 KiB → 4 MiB (~416 B/entry). A cold oracle rebuilt the same tree from `Enumerate` and agrees. Four findings the case did not predict: **the FUSE write path blocks at the upload rate** once a burst exceeds the 1024-slot buffer plus 4 × 64 worker queues (measured backlog 1286 against a capacity of 1280, so `tar -x` runs at Drive's upload rate and nothing says why); **write amplification ≈ 6000×** (190 KB of content, 1175 MB to disk) from per-operation bbolt commits, with the oracle's bulk `SetMany` reconcile 2.8× cheaper for identical content; **enumeration scales with the account, not the mount** (the oracle listed 11 129 objects to keep 10 101, discarding 1028 belonging to another rig's folder — so delete a scenario's folder once recorded); and **MC-01's duplicate upload did not reproduce** — 10 000 lines for 10 000 distinct paths. | Debounce/worker-pool behaviour under burst; the first realistic chance of hitting user rate limits. **It is not that chance**: drivel's serialised upload path caps a writing client near 1 request/s, so a single-writer case cannot burst hard enough to trip a quota. That question moves to MC-36 and MC-51. |
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
| **MC-40** | `kill -9` A mid-upload of a 5 GiB file. Restart. | No partial file remotely, no duplicate created on retry, upload resumes or restarts cleanly. Session URIs are deliberately not persisted, so a restart means a fresh upload — confirm it does not leave an orphan. | Crash-consistency of the push path. |
| **MC-41** | `kill -9` C mid-sweep (during MC-01's initial enumeration). Restart. | `[sweep] resuming the enumeration interrupted at …`; the persistent `seen` marks prevent pages consumed by the dead process from being read as remote deletions. | The M7b resume path — an in-memory seen-set would delete the user's data here. |
| **MC-42** | Delete C's `drivel-index.db` while stopped; restart. | Degrades to `files.list` name lookups. **No duplicates created, no data lost** — only latency and quota. | M7 invariant 2. Before M7, this produced duplicate uploads beside real files. |
| **MC-43** | With C in `-lazy` mode holding placeholders, delete C's `drivel-state.db`; restart. | The `user.drivel.placeholder` xattr still identifies them; nothing pushes a placeholder over real remote content. | M5 invariants 1 and 2 — the data-loss guards. Run with `DRIVEL_REQUIRE_TESTENV=xattr` semantics in mind: on a filesystem without user xattrs this case is meaningless, and `app.Mount` warns as much. |
| **MC-44** | Block UDP/443 for A only (`nft add rule … udp dport 443 drop`) for 10 min under load, then restore. | HTTP/3 dial fails, HTTP/2 fallback carries the traffic, sync continues. Compare A's throughput and CPU against B and C for the same workload — that is a free H3-vs-H2 measurement on identical work. | The transport fallback under load rather than at startup, plus the per-client cost of each protocol. |
| **MC-44b** | Block **all** traffic to Drive for one client for 20 min, then restore. | Retries with backoff, no crash, no local data loss, converges on restore. Cursor should still be valid at 20 min. | Partition tolerance; distinguishes "offline" from "broken". |
| **MC-45** | Point two clients at the *same* `token.json` and run a workload across a refresh boundary. | Determine whether concurrent refresh-and-rewrite corrupts the file or drops a token. | Validates (or removes) the per-client-token requirement in §2.1. |
| **MC-46** | Fill C's `data/` filesystem to 100% during a large inbound download. | ENOSPC surfaces as an error, not as a truncated file that then gets pushed back up as the new remote content. | A short local file becoming the canonical remote content is the worst available outcome. |
| **MC-47** | Cross-process hygiene, deliberately: (a) two processes sharing one state DB; (b) client B's `data/` placed inside client A's `data/`; (c) B's mountpoint inside A's backing tree. | (a) should fail fast with the 5s bbolt flock timeout — safe but opaque. (b) and (c) are **unguarded across processes** and are expected to produce event storms or recursion. | Confirms which cross-process footguns are real, and whether a lock file or a startup check is worth adding. |

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
   now that "run several drivels" is a supported shape.
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
    (item 15), so no case would ever have reported convergence; and `logcount.sh`
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
15. **mtime is not synchronised, and nothing said so.** Found by exercising
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
6. **Failure injection**: Group V.
7. **Resource and soak**: Group VI, last, because MC-53 wants a stable build.

**Exit criteria.** Every case in Groups I–IV either passes or has a filed issue
with a Tier A regression test reproducing it; §3's log-derived counters are
published for every case; and §6's cost table exists with numbers for N = 1, 2, 3.
