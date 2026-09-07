# Drivel documentation

Two collections, split by who is reading.

**[user/](user/) — using Drivel.** Install it, get Google credentials, mount a
folder, understand what it does to your files, fix it when it misbehaves.

| | |
| --- | --- |
| [Quickstart](user/quickstart.md) | From nothing to a mounted Drive folder. |
| [Installing](user/install.md) | Build from source, `go install`, FUSE helper, completions, man page, uninstall. |
| [Google credentials](user/google-cloud-setup.md) | Creating the OAuth client Drivel logs in with. |
| [Configuration](user/configuration.md) | Every flag and every config-file key. |
| [Lazy mode](user/lazy-mode.md) | `-lazy`, placeholders, and the one filesystem rule that makes it safe. |
| [Data safety](user/data-safety.md) | Conflicts, deletions, and what Drivel will never do to your files. |
| [Troubleshooting](user/troubleshooting.md) | Errors, stale mounts, expired tokens, quota. |
| [Platform support](user/platforms.md) | What runs where, and what changes off Linux. |
| [`drivel(1)`](user/drivel.1) | The man page — `man ./docs/user/drivel.1`. |

**[dev/](dev/) — working on Drivel.** Architecture, the invariants you must not
break, how to add a provider, and how to build and test.

| | |
| --- | --- |
| [Architecture](dev/architecture.md) | Runtime components and the package dependency graph. |
| [Workflows](dev/workflows.md) | How a write, a remote change, a sweep and a hydration actually run. |
| [Storage schema](dev/schema.md) | What the two bbolt databases hold, and what is authoritative. |
| [Dependencies](dev/dependencies.md) | Every direct dependency, why it is there, and what was rejected. |
| [Conventions](dev/conventions.md) | The rules that carry correctness. Read before changing sync. |
| [Building](dev/building.md) | Make targets, toolchain, lint, hooks, CI, debugging. |
| [Testing](dev/testing.md) | The suite, the kernel facilities it needs, and platform runs. |
| [Writing a provider](dev/new-provider.md) | Implementing `provider.Store` and its optional interfaces. |
| [Glossary](dev/glossary.md) | Echo, baseline, sweep, placeholder, seam — the words this codebase reuses. |
| [Multi-client test plan](dev/multiclient-test-plan.md) | The fleet-convergence campaign. |

**[project/](project/)** holds repo housekeeping: [publishing](project/publishing.md)
to pkg.go.dev and [marketing copy](project/marketing.md).

`DESIGN.md` and `CLAUDE.md` at the repo root are internal working documents — the
long-form architectural record and the agent/contributor conventions. They are not
part of either collection above and are not maintained as published documentation.
