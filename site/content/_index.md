---
layout: hextra-home
---

{{< hextra/hero-headline >}}
  Your remote storage,&nbsp;<br class="sm:hx-block hx-hidden" />as an ordinary folder
{{< /hextra/hero-headline >}}

<div class="hx-mt-6 hx-mb-6">
{{< hextra/hero-subtitle >}}
  Open, edit and save with the tools you already use.&nbsp;<br class="sm:hx-block hx-hidden" />Drivel keeps a real local copy and syncs it in the background, both ways.
{{< /hextra/hero-subtitle >}}
</div>

<div class="hx-mb-12">
{{< hextra/hero-button text="Get started" link="docs/quickstart" >}}
</div>

{{< hextra/feature-grid >}}
  {{< hextra/feature-card
    title="Instant reads"
    subtitle="Filesystem operations never block on the network. Your file manager never spins waiting on the cloud — uploads and downloads happen off to the side."
  >}}
  {{< hextra/feature-card
    title="Bidirectional background sync"
    subtitle="Edit locally or remotely and both sides converge. Two backends ship today: Google Drive, and SFTP to any server you have an SSH account on."
  >}}
  {{< hextra/feature-card
    title="Lazy mode"
    subtitle="A whole remote tree visible as zero-byte placeholders, content fetched on first read. A store larger than your disk becomes usable."
  >}}
  {{< hextra/feature-card
    title="Guarded deletions"
    subtitle="A deletion is inferred only from a sync baseline, so a first run deletes nothing — and a suspicious count refuses the whole pass rather than trimming it."
  >}}
  {{< hextra/feature-card
    title="Conflict-safe"
    subtitle="Concurrent edits produce a conflict copy. Nobody's version is thrown away to settle an argument."
  >}}
  {{< hextra/feature-card
    title="Bring your own credentials"
    subtitle="Drivel is a client you run yourself — there is no Drivel service, and it ships no credentials of its own. It reaches your storage under an identity that is yours to inspect and revoke."
  >}}
  {{< hextra/feature-card
    title="Several accounts at once"
    subtitle="N mounts in one process, validated against each other before any of them opens."
  >}}
  {{< hextra/feature-card
    title="Pluggable backends"
    subtitle="The remote side is a narrow path-addressed interface rather than a Drive-shaped one. Each backend runs in its own process."
  >}}
  {{< hextra/feature-card
    title="Free and open"
    subtitle="Mozilla Public License 2.0 — the code you run is the code you can read."
  >}}
{{< /hextra/feature-grid >}}

<div class="hx-mt-12">

## Install

```sh
sudo apt install fuse3                                    # or: dnf install fuse3
go install github.com/syshlted/drivel/cmd/drivel@latest
go install github.com/syshlted/drivel/cmd/drivel-provider-gdrive@latest
```

A backend is a separate executable, so the host binary alone has no providers.
Syncing to a server you can `ssh` to needs no login step and no credentials of
its own — it is your existing SSH key. See [SFTP](docs/sftp/).

</div>
