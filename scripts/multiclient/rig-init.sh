#!/usr/bin/env bash
# rig-init.sh — lay out a multi-client rig: N drivel clients, one Drive folder.
#
# Each client gets its own mountpoint, backing dir, state DB, index DB, config
# and log. Nothing is shared except the remote folder — which is the point — and
# the OAuth client secret, which is a credential rather than state.
#
#   ./rig-init.sh -r <FOLDER_ID> -c ~/credentials.json [-n 3] [-d rig]
#
# Then, per client, in its own terminal:
#   drivel mount -config rig/c1/config.toml 2>&1 | ts > rig/c1/logs/run.log
#
# Tokens: each client needs its own. token.json is rewritten on refresh and three
# processes sharing one path race on that write. This script copies an existing
# token into each client if you point it at one, otherwise run `drivel login
# -account cN` per client (see README.md).
set -euo pipefail

root=rig
n=3
folder=
creds=
token=

usage() { sed -n '2,18p' "$0"; exit "${1:-0}"; }

while getopts ":r:c:t:n:d:h" opt; do
  case $opt in
    r) folder=$OPTARG ;;
    c) creds=$OPTARG ;;
    t) token=$OPTARG ;;
    n) n=$OPTARG ;;
    d) root=$OPTARG ;;
    h) usage 0 ;;
    *) usage 2 ;;
  esac
done

[[ -n $folder ]] || { echo "rig-init: -r <FOLDER_ID> is required" >&2; usage 2; }
[[ -n $creds  ]] || { echo "rig-init: -c <credentials.json> is required" >&2; usage 2; }
[[ -f $creds  ]] || { echo "rig-init: no such file: $creds" >&2; exit 1; }

# A dedicated folder, never the root alias. -drive-root root maps the mount to
# the whole of My Drive, and Store.Remove is Files.Delete — permanent, not the
# trash. These tests are designed to destroy data; point them at a folder that
# exists for that.
if [[ $folder == root ]]; then
  echo "rig-init: refusing to build a rig on the whole of My Drive." >&2
  echo "          Create a dedicated folder and pass its ID." >&2
  exit 1
fi

mkdir -p "$root"
abs=$(cd "$root" && pwd)

for i in $(seq 1 "$n"); do
  c=c$i
  d=$abs/$c
  mkdir -p "$d"/{mnt,data,logs}
  cp "$creds" "$d/credentials.json"
  [[ -n $token ]] && cp "$token" "$d/token.json"

  cat > "$d/config.toml" <<TOML
# Client $c of the multi-client rig. See docs/multiclient-test-plan.md.
[account.$c]
provider    = "gdrive"
credentials = "$d/credentials.json"
token       = "$d/token.json"

[[mount]]
name  = "$c"
path  = "$d/mnt"
data  = "$d/data"
# The state DB stays outside every backing tree: inside one it syncs itself, and
# its own writes generate the events that cause more writes.
state = "$d/drivel-state.db"
account = "$c"

# Off by default. The periodic sweep is the most disruptive scheduled event in
# the system, so a run that has not asked for one must not have one fire in the
# middle of its measurements. Set "2m" for the soak (MC-53).
sweep-interval = "0"
# Bounds what a *reconcile* may infer. Feed-driven deletes are not capped by it.
max-deletes = 100

[mount.provider]
root  = "$folder"
index = "$d/drivel-index.db"
TOML
done

# The oracle: a cold client that reconstructs the tree from Enumerate alone, so
# its backing dir *is* the remote's manifest. Not started with the fleet — run it
# when you want to check what Drive actually holds.
d=$abs/oracle
mkdir -p "$d"/{mnt,data,logs}
cp "$creds" "$d/credentials.json"
[[ -n $token ]] && cp "$token" "$d/token.json"
cat > "$d/config.toml" <<TOML
# The oracle (docs/multiclient-test-plan.md §2.4). Three clients agreeing proves
# they converged, not that they converged on what Drive holds. Wipe data/ and the
# state DB before each use so it enumerates from nothing:
#   rm -rf $d/data/* $d/drivel-state.db $d/drivel-index.db
[account.oracle]
provider    = "gdrive"
credentials = "$d/credentials.json"
token       = "$d/token.json"

[[mount]]
name        = "oracle"
account     = "oracle"
path        = "$d/mnt"
data        = "$d/data"
state       = "$d/drivel-state.db"
resync      = true
materialize = true

[mount.provider]
root  = "$folder"
index = "$d/drivel-index.db"
TOML

mkdir -p "$abs"/{manifests,metrics,results}

echo "rig laid out in $abs ($n clients + oracle), folder $folder"
if [[ -z $token ]]; then
  echo
  echo "next: give each client a token of its own —"
  for i in $(seq 1 "$n"); do
    echo "  drivel login -account c$i   # then copy the token into $abs/c$i/token.json"
  done
fi
