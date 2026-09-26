# Project status

> [!CAUTION]
> **Drivel is a work in progress. Do not use it for anything you cannot afford
> to lose.**
>
> It is unfinished and under-tested, and it **will probably lose data** for
> somebody before it stops being either. Keep backups, make them often, and
> check that you can restore from them.

This is the single place that statement lives; everything else on the site and
in this manual links here.

## Why this page exists rather than a version number

Drivel is a filesystem that **deletes files on your behalf, in both directions,
partly on the strength of what it infers happened while it was not running.**
That is the job, and there is no version of it that does not involve acting on a
belief about the past.

[Data safety](data-safety.md) describes the rules that govern those decisions,
and they are real: a deletion is only ever inferred from a sync baseline, the
first run deletes nothing, a local file that diverged is kept and pushed back
rather than removed, and a delete pass whose count looks wrong abandons itself
instead of trimming. The design is careful on purpose.

**What is missing is not the care. It is the mileage.** The rules have been
reasoned about far more than they have been run, and every serious bug found so
far was found by running the thing on a real machine against a real server —
never by reading it, and never by a green test suite. The [manifesto](../../MANIFESTO.md)
commits to saying what has not been done. This is that, at the level of the
whole project.

## What this means for you

- **Keep a backup, and restore from it once.** A backup nobody has ever restored
  is a hypothesis. This is the only item on this list that would save you from
  everything else on it.
- **Do not let a Drivel mount hold the only copy of anything.** The backing
  directory is ordinary files and survives Drivel being uninstalled — but it
  does not survive Drivel deleting from it, which is a thing Drivel is designed
  to do.
- **The first run against an existing tree is the riskiest moment.** That is when
  the enumeration sweep reconciles two sides that have never been compared. It
  deletes nothing on a first run by design; it can still push, pull, and create
  conflict copies in bulk.
- **Watch the log the first few times.** Refusals, skips and conflict copies are
  all announced. A mount that is quietly doing nothing is easier to notice early
  than to diagnose later.
- **Prefer a scratch copy for evaluation.** Point `-data` at a duplicate of the
  tree you care about, not the original, until you have watched it behave.
- **Be deliberate about `-max-deletes`.** It defaults to 100 and it is the guard
  that turns a wrong premise into a refused pass instead of a mass deletion.
  Raising it is a decision, not tidying.

## What has actually been run

Every package has tests and CI runs them under the race detector on every push;
that is the floor, not the evidence. Above it:

- **Google Drive** — exercised continuously in development against a real
  account, with one behaviour (what a removal does to the remote object) pinned
  by a live test against the real API in September 2026.
- **SFTP** — verified through a real mount against OpenSSH in September 2026: an
  existing tree materialised, edits moved both ways, rename and delete
  propagated, the diverged-copy guard held. Two bugs were found by that single
  run that nothing else had surfaced.
- **Linux** is the platform everything is developed on. **FreeBSD** has been run
  on its own hardware. **macOS has never been run at all** — it compiles, and
  that is the entire claim. See [platform support](platforms.md).

What has *not* happened is sustained multi-machine use by people who did not
write it, over months, on data they care about. That is the missing input, and
no amount of further reading replaces it.

## Known sharp edges

These are documented rather than fixed, because each one's available fixes cost
something worse. They are not the whole list — they are the ones you could
plausibly meet.

- **Same-name siblings** on Google Drive resolve to one visible file, and a
  deletion can make the path reappear with an older sibling's contents. See
  [data safety](data-safety.md#same-name-siblings).
- **A case-insensitive backing filesystem** (the default on macOS) collapses two
  distinct remote names into one local path. Nothing handles this yet.
- **Lazy mode is unsafe on a backing filesystem without extended attributes**,
  and the mount warns rather than refusing. See [lazy mode](lazy-mode.md).
- **SFTP polls**, because the protocol has no change feed, and its default poll
  interval is far too long for interactive use. See [SFTP](sftp.md).

## When this page goes away

Not on a date, and not at a version number picked to look finished. It goes away
when sustained real-world use has stopped producing the kind of bug that the
last several real-world runs each produced — and the honest way to read this
page is that until then, the project would rather have your scepticism than your
trust.

If you run it and something goes wrong, **that is the most useful thing you can
do for it**: [open an issue](https://github.com/syshlted/drivel/issues), and say
what you had mounted and what you expected.
