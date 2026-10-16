#!/usr/bin/env python3
"""Empty a rig's Drive folder — the clean-remote step the case drivers need.

    rig-purge.py CLIENTDIR [--trash]

CLIENTDIR is one client's directory (the one holding credentials.json,
token.json and config.toml). The folder to empty is read from that config's
`root`, and the credentials from beside it, so **this script hardcodes neither a
path nor a folder ID**: the repo deliberately records no machine's rig location
and no account's folder (commit 20e5460 removed one, and a purge script with a
baked-in folder ID is a footgun as well as a leak).

Only the mount root's direct children are removed, because trashing a folder
takes its subtree with it. It refuses a root of "root" or empty for the reason
rig-init.sh does: that is My Drive, and this script deletes.

The folder is emptied with `files.delete`, which is permanent, so purging never
fills the trash. **drivel's own deletes do** — `-drive-delete` defaults to
`trash` and that is a safety property, not an oversight — so a case like MC-23
leaves its whole subtree in the trash, where it keeps counting against the
account's storage quota until something empties it. `--trash` is that something,
and the plan says to do it *between* runs rather than at the end (§5, item 7),
because the quota it frees is what the next case has to spend. It is
`files.emptyTrash`, so it is **account-wide and not folder-scoped** — correct on
a rig account that exists for this and wrong anywhere else, which is why it is
an explicit flag and not part of the purge.

Either way the account's storage quota is printed afterwards: MC-20 at five
clients moves ~5 GiB against a 15 GiB free tier, so "how much is left" is a
precondition of the next run rather than a curiosity.
"""
import json, os, re, sys, urllib.parse, urllib.request

args = sys.argv[1:]
trash = "--trash" in args
args = [a for a in args if a != "--trash"]
if len(args) != 1:
    sys.exit("usage: rig-purge.py CLIENTDIR [--trash]")
client = args[0].rstrip("/")

cfg = open(os.path.join(client, "config.toml")).read()
m = re.search(r'^\s*root\s*=\s*"([^"]*)"', cfg, re.M)
root = m.group(1) if m else ""
if root in ("", "root"):
    sys.exit("rig-purge: refusing to empty My Drive; config's root must name a folder")

creds = json.load(open(os.path.join(client, "credentials.json")))["installed"]
tok = json.load(open(os.path.join(client, "token.json")))
body = urllib.parse.urlencode({
    "client_id": creds["client_id"], "client_secret": creds["client_secret"],
    "refresh_token": tok["refresh_token"], "grant_type": "refresh_token"}).encode()
access = json.load(urllib.request.urlopen(
    urllib.request.Request("https://oauth2.googleapis.com/token", data=body)))["access_token"]

def call(method, url):
    r = urllib.request.Request(url, method=method)
    r.add_header("Authorization", "Bearer " + access)
    resp = urllib.request.urlopen(r)
    return resp.status if method == "DELETE" else json.load(resp)

q = urllib.parse.quote(f"'{root}' in parents and trashed=false", safe="")
files = call("GET", "https://www.googleapis.com/drive/v3/files"
                    f"?q={q}&fields=files(id,name)&pageSize=1000")["files"]
for f in files:
    call("DELETE", f"https://www.googleapis.com/drive/v3/files/{f['id']}")
print(f"purged {len(files)} object(s) from {root}")

if trash:
    call("DELETE", "https://www.googleapis.com/drive/v3/files/trash")
    print("emptied the account's trash")

# A different `q` from the query above, which has been consumed by now.
quota = call("GET", "https://www.googleapis.com/drive/v3/about?fields=storageQuota")["storageQuota"]
gib = lambda k: int(quota.get(k, 0)) / 2**30
print(f"quota: {gib('usage'):.2f} of {gib('limit'):.2f} GiB used, "
      f"{gib('usageInDriveTrash'):.2f} GiB of that in the trash")
