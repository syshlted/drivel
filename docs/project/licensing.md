# Licensing

Why Drivel is under the Mozilla Public License 2.0, what that decides, and what
would change the answer. This is a decision record, not a licence summary — the
licence itself is [`LICENSE`](../../LICENSE) and it is Mozilla's text verbatim.

**Nothing here is legal advice.**

## The decision

Drivel is **MPL-2.0** as of 2026-09-21. It was **AGPLv3** from 2026-07-20 until
then, across the whole of v1 and most of v2.

The change was made before the first tagged release and before the project had
any external users or contributors, which is the only reason it was cheap. It
gets more expensive every day; if this is ever revisited, that is the first fact
to weigh.

## Why AGPLv3 first, and why that wasn't a mistake

AGPL was chosen deliberately and for a coherent reason: a sync engine is
exactly the kind of component a vendor absorbs, improves privately, and ships
inside a product while the upstream project stagnates. Reciprocity is a real
defence against that, and the maintainer's own preference was and is for
copyleft.

What went wrong was fit, not principle.

## Why it stopped fitting

**AGPL's distinguishing clause is inert here.** §13 — network use counts as
distribution — is the *only* thing separating AGPL from plain GPL. It triggers
when users interact with the software remotely. Drivel is a filesystem that
runs on the machine whose disk it is managing. Nobody hosts it as a service.
The project was paying AGPL's full adoption cost for a clause that would never
fire.

**And it fought the architecture.** M9 made `provider` public *specifically* so
backends could be written outside this tree; M24 is a registry for installing
third-party ones. Under AGPLv3 a plugin importing
`github.com/zishmusic/drivel/provider` plausibly had to be AGPL itself, while a
plugin speaking only protobuf was the §2.9.2 arms-length case — two routes to
the same seam with different answers, and the more convenient route was the one
that punished the author. DESIGN.md §9/M24 carried that as an unresolved
blocker for months. The licence was suppressing the ecosystem the plugin
architecture exists to create.

## The argument that did not survive checking

The trigger for reopening the question was a claim that recent FDA rules require
embedded medical software to be unmodifiable in the field, making GPLv3-family
anti-tivoization a growing liability.

**It did not hold up, and it is recorded here so nobody rebuilds the case on
it.** The canonical source is the FSF's *Regulatory compliance is no reason to
lock up users*, published **June 2006** during GPLv3 drafting — and it says
outright that the FDA probably does not in fact require this. It was a
manufacturer talking point, not a rule. No recent FDA action was found doing
what the claim describes, and US law has moved the other way: FD&C Act §524B
requires device makers to plan for *patching* devices post-market.

It would not have applied to Drivel in any case. Anti-tivoization (§6
Installation Information) attaches to consumer "User Products", and Drivel is a
desktop filesystem that will never ship inside a regulated device.

The conclusion survived on other grounds. The rationale did not, and a rationale
that collapses under the first question is worse than none — it invites the
whole decision to be reversed by whoever asks.

## Why not Apache-2.0

Apache was the intended destination for most of this discussion, paired with a
non-binding request that people share changes back. What defeated it was
answering the question the recommendation rested on: *who would actually build a
proprietary fork of a FUSE sync client?*

The answer is: plausibly quite a few people.

- **Cloud providers with no decent Linux client** — the strongest case. Writing
  a backend against a documented seam is now a bounded task, and most of them
  would write it closed.
- **NAS vendors.** Cloud sync on Linux firmware is a shipping feature for every
  one of them.
- **The market Insync already serves commercially**, on a weaker foundation.

Drivel's own roadmap is an open invitation to exactly these people. Once the
answer to "would a closed fork exist?" is *probably*, reciprocity stops being
theoretical and Apache stops being free.

**The two obvious comparators bracket the choice, and they disagree in an
instructive way.** rclone is MIT: its commercial layer — paid GUIs and wrappers
— sits on top and contributes back voluntarily. OpenTofu is **MPL-2.0**, and was
founded in 2023 by a consortium of *commercial vendors* (Gruntwork, Spacelift,
env0, Scalr, Harness) choosing a licence with an entirely free hand after
escaping BUSL. They picked file-level copyleft and kept it through Linux
Foundation adoption and CNCF sandbox status.

That disposes of the strongest objection to MPL. It is not a barrier to
commercial adoption; it is what commercial vendors choose when they want a
licence nobody's legal department has to argue about. Two further objections
raised against it were simply wrong: MPL is not unusual in Go — Terraform,
Vault, Consul and Packer were all Go under MPL-2.0 and are among the most
commercially embedded Go projects ever written — and MPL does not block a single
commercial model Drivel might attract.

## What MPL commits us to

1. **The copyleft is per file** (§1.10, §3.2). A modified Drivel source file
   ships with its source. A larger work that merely uses Drivel does not. This
   boundary is the reason for the choice and not a side effect of it: anything
   that would oblige a *caller* to open its source is outside what this licence
   asks for, and a future change that drifts across that line is a change of
   licence in substance.
2. **`LICENSE` stays verbatim.** Never edit it, never append to it, never add a
   second licence file at the root. Both breakages defeat the `licensecheck`
   detector pkg.go.dev uses — see [publishing.md](publishing.md) — and a
   modified licence is a bespoke licence that every corporate reviewer must read
   from scratch, which is precisely the friction being avoided.
3. **Every `.go` file carries the Exhibit A header.** Without it, a file copied
   out of this tree arrives somewhere else saying nothing about what it is. That
   is where file-level copyleft leaks, and the header is the only thing that
   closes it. `make license-check` enforces this.
4. **Contributions are inbound-matches-outbound** (§5). No CLA, and none should
   be introduced — a CLA is the mechanism by which a project later relicenses
   proprietary, and not having one is a promise worth keeping.
5. **It stays GPL-compatible** (§3.3, Secondary Licenses). Moving to AGPL later
   is possible. Moving from Apache would not have been.

## What the licence deliberately does not reach

Backends, wrappers, GUIs, packaging, and anything else built *around* Drivel.
That is by design and is permanent.

Because it is permanent, the project asks rather than requires — see the README's
licence section. If you write a `drivel-provider-*`, publish it; if you run
Drivel where we cannot test, say what happened. **Keep that a request.** Wording
that drifts toward sounding like a term reintroduces the ambiguity this licence
was chosen to remove, and a reader who has to wonder whether there is a hidden
obligation gets the worst of both licences.

The one thing that is *not* merely asked is the name: MPL covers the code, not
the trademark.

## What would change this answer

- **A CLA appearing.** It would mean someone is contemplating relicensing
  proprietary, and that conversation should be explicit.
- **The file boundary failing in practice.** MPL's unit is the file, which is
  softer in Go than in C — code copied *out* into a new file is arguably not a
  Modification. Terraform and OpenTofu lived with this for a decade at far
  greater stakes, so it is tolerable, but a concrete instance of it being
  exploited is evidence worth acting on.
- **The commercial premise proving wrong.** If years pass with no vendor
  interest, the reciprocity is defending against nothing and Apache's lower
  friction becomes the better trade. That is an argument for *later*, and it
  requires evidence rather than impatience.
