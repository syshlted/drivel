#!/usr/bin/env python3
"""List a rig's Drive folder with digests — the independent oracle a case needs.

    rig-ls.py CLIENTDIR [SUBPATH]

Prints one line per object under the mount root: `md5  size  name`, recursively,
newest first within a folder, so two objects sharing a path are both visible.
That is the point of it. A peer client cannot answer "how many objects are at
this path?" — same-name siblings collapse into one local file (MC-30) — and a
client's own log cannot answer "what does the remote hold?" without being the
thing under test. This asks Drive.

Credentials, token and folder come from CLIENTDIR's config.toml exactly as
rig-purge.py reads them, so this script hardcodes neither a path nor a folder ID.
Read-only: it never writes to Drive.
"""
import json, os, re, sys, urllib.parse, urllib.request

if len(sys.argv) not in (2, 3):
    sys.exit("usage: rig-ls.py CLIENTDIR [SUBPATH]")
client = sys.argv[1].rstrip("/")
want = sys.argv[2].strip("/") if len(sys.argv) == 3 else ""

cfg = open(os.path.join(client, "config.toml")).read()
m = re.search(r'^\s*root\s*=\s*"([^"]*)"', cfg, re.M)
root = m.group(1) if m else ""
if root in ("", "root"):
    sys.exit("rig-ls: config's root must name a folder")

creds = json.load(open(os.path.join(client, "credentials.json")))["installed"]
tok = json.load(open(os.path.join(client, "token.json")))
body = urllib.parse.urlencode({
    "client_id": creds["client_id"], "client_secret": creds["client_secret"],
    "refresh_token": tok["refresh_token"], "grant_type": "refresh_token"}).encode()
access = json.load(urllib.request.urlopen(
    urllib.request.Request("https://oauth2.googleapis.com/token", data=body)))["access_token"]

FOLDER = "application/vnd.google-apps.folder"


def children(parent):
    q = urllib.parse.quote(f"'{parent}' in parents and trashed=false", safe="")
    url = ("https://www.googleapis.com/drive/v3/files"
           f"?q={q}&orderBy=modifiedTime%20desc&pageSize=1000"
           "&fields=files(id,name,mimeType,size,md5Checksum)")
    r = urllib.request.Request(url)
    r.add_header("Authorization", "Bearer " + access)
    return json.load(urllib.request.urlopen(r))["files"]


def walk(parent, prefix):
    for f in children(parent):
        path = f"{prefix}{f['name']}"
        if f["mimeType"] == FOLDER:
            print(f"{'-':<32} {'dir':>12}  {path}/")
            walk(f["id"], path + "/")
        else:
            print(f"{f.get('md5Checksum', '-'):<32} {f.get('size', '-'):>12}  {path}")


# A subpath is resolved a level at a time, because Drive has no path lookup.
here, prefix = root, ""
for part in [p for p in want.split("/") if p]:
    hit = [f for f in children(here) if f["name"] == part]
    if not hit:
        sys.exit(f"rig-ls: no {part} under {prefix or 'the mount root'}")
    here, prefix = hit[0]["id"], prefix + part + "/"
walk(here, prefix)
