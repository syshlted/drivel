# The Drivel Manifesto

Drivel is a filesystem. It holds the only local copy of somebody's data, and it
talks to a service it does not control.

Nearly everything below follows from taking those two sentences seriously.

This is not a roadmap and not a feature list. It is the set of commitments that
decide arguments — when a change would be faster, simpler, or more popular but
breaks one of these, the commitment wins. If you are deciding whether to trust
Drivel with your files, or whether to spend an afternoon contributing to it,
this is the honest version of what you are getting.

---

## 1. Your files stay ordinary files

Drivel sits over a real directory. Stop Drivel and your files are still
there — plain files in plain directories, readable by everything you already
own. No database, no proprietary container, no daemon required to get your data
back out. Mount a directory onto itself and the files simply stay in it.

A sync client you cannot walk away from is not a sync client. It is a hostage
situation with good branding.

## 2. The filesystem never waits for the network

Listing a directory does not make an API call. Writing a file does not block on
an upload. Every remote operation happens off the filesystem path, behind a
buffered channel, on somebody else's schedule.

This is the whole design, and it is what separates Drivel from mounting a
remote filesystem directly. A cloud-first mount is honest about latency and
unusable because of it. Drivel is a local directory that happens to reconcile
itself with somewhere else.

There is one deliberate exception — reading a file that is a placeholder must
actually fetch the bytes — and it is written down as an exception rather than
quietly hidden in the average case.

## 3. Defaults are safety properties, not preferences

A remote deletion defaults to the recoverable kind. Reconcile refuses to infer
more than a hundred deletions without being told to. `nodev` and `nosuid` are
compulsory with no flag to turn them off. Extended-attribute passthrough is off,
because turning it on lets a user detach the marker that distinguishes a
placeholder from an empty file.

When a default protects data it is not a matter of taste, and it does not get
changed because the safe path is slower or because an experienced user finds it
patronising.

## 4. When unsure, refuse

An unknown key in the config file is an error, not a warning. `lazzy = true`
silently doing nothing is the same failure as a flag that stopped being read,
and both end with somebody believing a protection is on when it is off.

A hard link gets `EPERM` rather than silently becoming two files that will
diverge. A reconcile pass that would exceed its deletion cap abandons the entire
pass rather than trimming it to fit — a huge count means the premise is broken,
not that there are that many real deletions.

Refusing is louder and more annoying than guessing. It is also the only one of
the two that cannot lose a file.

## 5. A slower answer is never the wrong answer

Every optimisation in the sync engine falls through to the thing that always
worked. Range writes decline and the whole file uploads. A digest comparison
declines and the whole file uploads. A cached path that cannot be verified is
thrown away and looked up again.

That fallback is what makes the optimisations safe to have at all. Performance
work that cannot degrade safely does not ship.

## 6. We say what we have not done

The documentation distinguishes *compiles* from *tested* from *verified against
the real service*, and it names dates. macOS is compile-verified only and says
so in those words. FreeBSD was run on its own hardware, where it found a real
bug that no amount of green CI had surfaced — so "a green unit suite is not
evidence this backend works" is written down, because it was learned rather than
assumed.

A project that only publishes its successes is telling you something, and it is
not that there were no failures.

## 7. There is no Drivel service

Drivel ships no credentials of its own. You supply the ones for whatever storage
you point it at, so it reaches your data under an identity that is yours to
inspect and revoke. There is no account to create, no hosted component, no
paid tier, and no version of this project that has one.

What a *storage provider* does with what you send it is between you and them.
Drivel does not answer for software it did not write; where that matters, it is
documented on that provider's own page rather than promised in a headline.

## 8. The backend is a detail, not the product

Google Drive shipped first. SFTP shipped second. Neither of them is what Drivel
*is*.

The provider interface is public so that backends can be written by people who
are not us — including people we will never meet and companies we will never
talk to. Copy that describes Drivel stays backend-neutral on purpose. Naming the
backend that ships is honest; implying it is the only conceivable one is not.

## 9. Correctness is the feature

A sync client that is fast and occasionally eats a file is worse than no sync
client at all, because you will have trusted it.

The loop-suppression model, the placeholder guards, the deletion baselines, the
conflict copies — these are not infrastructure beneath the product. They *are*
the product. Everything else is convenience.

---

## What this costs

These commitments are not free, and pretending otherwise would violate the sixth
one.

Refusing rather than guessing means a typo in your config file stops the mount
instead of being ignored. Safe fallbacks mean Drivel sometimes uploads a whole
file where a cleverer client would have sent a fraction of it. Never blocking on
the network means what you see is what was last reconciled, not what the remote
holds this instant. Being honest about what is unverified means the platform
list looks shorter than a more confident project's would.

We think those are the right trades for something holding your only local copy.
You may not, and that is a legitimate reason to use something else.

## What we ask

Drivel is under the Mozilla Public License 2.0. The copyleft reaches Drivel's
own source files and deliberately stops there: build commercial products on it,
link it into proprietary software, write closed-source backends against the
provider interface — all permitted, permanently, by design.

Past that line we can only ask, so we do. If you write a backend, publish it. If
you run Drivel somewhere we cannot test, tell us what happened. If you fix a
correctness bug in the sync engine, send it back — a fix that lives only in your
fork is a fix the next person has to find the hard way, and in this particular
codebase "the hard way" means somebody's data.

That is a request and not a term. See [README](README.md#license) for the
licence, [CONTRIBUTING.md](CONTRIBUTING.md) for the practical version, and
[docs/project/licensing.md](docs/project/licensing.md) for why the line is drawn
where it is.
