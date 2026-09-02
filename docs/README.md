# Drivel documentation

- [google-cloud-setup.md](google-cloud-setup.md) — get your own Google Drive API
  credentials (OAuth client ID/secret), step by step, with troubleshooting.
- [architecture.md](architecture.md) — Mermaid diagrams: runtime components and
  the package dependency graph. Pairs with [../DESIGN.md](../DESIGN.md).
- [development.md](development.md) — contributor guidelines: build, test, debug,
  and how to add a new storage backend or mount frontend.
- [publishing.md](publishing.md) — what's needed to list the module on
  pkg.go.dev.
- [marketing.md](marketing.md) — reusable website/README marketing copy.
- [drivel.1](drivel.1) — the `drivel` manpage (`man ./docs/drivel.1`).

Shell completions live in [../completions/](../completions):

```sh
# bash — system-wide, or source from ~/.bashrc
sudo cp completions/drivel.bash /usr/share/bash-completion/completions/drivel

# zsh — copy into a directory on your $fpath (file must be named _drivel)
cp completions/_drivel ~/.zsh/completions/_drivel   # then: autoload -Uz compinit && compinit
```
