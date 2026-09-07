# Multi-client rig (Tier B)

Three real `drivel` processes against one Drive folder. The plan these implement
is [`docs/multiclient-test-plan.md`](../../docs/dev/multiclient-test-plan.md); the
deterministic half of it (Tier A) is `TestFleet*` in `internal/app`, which needs
no network and runs under `-race` with the rest of the suite.

## Before the first run

1. **A dedicated Drive folder, and its ID** — not `root`. `-drive-root root` maps
   the mount to the whole of My Drive.
2. **`Store.Remove` is `Files.Delete`.** Deletions here are permanent; they do not
   go to the trash and cannot be undone from the web UI.
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
scripts/multiclient/sample.sh -o rig/metrics/mc20.csv \
  "$(pgrep -f 'config rig/c1'):c1" \
  "$(pgrep -f 'config rig/c2'):c2" \
  "$(pgrep -f 'config rig/c3'):c3"
```

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

## Keeping the two tiers honest

Every Tier B failure gets a Tier A regression test before it is fixed. Tier B is
slow, quota-limited and non-deterministic — a discovery instrument, not a gate.
`TestFleet*` in `internal/app` and `TestChanges*` in `internal/provider/gdrive`
are where the findings live afterwards.
