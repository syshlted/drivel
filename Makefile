# drivel — build, lint and test entrypoints.
#
# Every gate is defined exactly once, here. The git hooks (lefthook.yml) and CI
# both invoke these targets rather than restating the commands, so "it passed
# locally" and "it passed in CI" cannot drift apart.

SHELL := /usr/bin/env bash
BIN   := $(CURDIR)/bin

# Pinned tool versions. Bump here; local hooks and CI follow automatically.
GOLANGCI_LINT_VERSION ?= v2.13.2
GOVULNCHECK_VERSION   ?= v1.7.0
LEFTHOOK_VERSION      ?= v1.13.6

# lefthook v1.13.6 does not compile under Go 1.27: its go-json-experiment
# dependency aliases stdlib symbols that encoding/json/v2 renamed. Pinning the
# toolchain that builds it is safe in a way it would not be for the linters —
# lefthook only shells out to make targets, it never parses Go source. Remove
# this pin once a lefthook release builds on the current toolchain.
LEFTHOOK_GOTOOLCHAIN  ?= go1.26.1

# Branch that lint-new measures "new" against.
MAIN_BRANCH ?= master

# Install locations. SBINDIR is not under PREFIX by default: mount(8) looks for a
# helper in /sbin and /usr/sbin only, so a helper in /usr/local/sbin is a helper
# mount(8) will never find.
PREFIX  ?= /usr/local
MANDIR  ?= $(PREFIX)/share/man
SBINDIR ?= /sbin

# Where the shell completions go. Both are the conventional locations rather than
# the only ones: bash-completion also reads $XDG_DATA_HOME, and zsh reads whatever
# is on $fpath, which is why the generated files carry per-user instructions of
# their own at the top.
BASHCOMPDIR ?= $(PREFIX)/share/bash-completion/completions
ZSHCOMPDIR  ?= $(PREFIX)/share/zsh/site-functions

# Tool binaries are stamped with both the tool version and the Go version that
# built them, so either changing forces a reinstall.
#
# The Go version is not decoration: a source-processing tool built with an older
# toolchain cannot parse code that uses a newer one, and it fails with "file
# requires newer Go version" rather than with a lint finding. Stamping it means a
# toolchain upgrade rebuilds the tools instead of silently breaking them.
GOVERSION := $(shell go env GOVERSION)

GOLANGCI_LINT := $(BIN)/golangci-lint-$(GOLANGCI_LINT_VERSION)-$(GOVERSION)
GOVULNCHECK   := $(BIN)/govulncheck-$(GOVULNCHECK_VERSION)-$(GOVERSION)
LEFTHOOK      := $(BIN)/lefthook-$(LEFTHOOK_VERSION)-$(LEFTHOOK_GOTOOLCHAIN)

.DEFAULT_GOAL := help

# Renders "name<separator>description" lines as an aligned two-column list with
# the left column bold. Three comment markers feed it, all read straight out of
# this file: `## target: what it does`, `#> make command  # what it does` for the
# Examples section, and `#>> make run ...` for the Running one. A target, its
# description and any worked example of it are therefore written together, where
# the recipe is, and cannot drift from it.
#
# The column width is measured rather than fixed, because a hand-tuned %-12s
# fails silently — the first name longer than the pad pushes its own row out of
# line and nothing says so. It is pasted into the format string rather than
# passed as a %-*s argument: `*` is a printf(3) feature that POSIX awk does not
# promise, and this file is read on FreeBSD too, where awk is a third
# implementation again.
COLUMNIZE = awk -v FS=$(1) '{ n[NR] = $$1; d[NR] = $$2; if (length($$1) > w) w = length($$1) } \
	END { fmt = "  \033[1m%-" w "s\033[0m  %s\n"; for (i = 1; i <= NR; i++) printf fmt, n[i], d[i] }'

## help: list every target, with examples of the common invocations
.PHONY: help
help:
	@printf '\033[1mTargets\033[0m\n'
	@grep -hE '^## ' $(MAKEFILE_LIST) | sed 's/^## //' | $(call COLUMNIZE,': *')
	@printf '\n\033[1mExamples\033[0m\n'
	@grep -hE '^#> ' $(MAKEFILE_LIST) | sed 's/^#> //' | $(call COLUMNIZE,' +# ')
	@printf '\n\033[1mRunning\033[0m — features are flags on the binary, documented in docs/user/configuration.md\n'
	@grep -hE '^#>> ' $(MAKEFILE_LIST) | sed 's/^#>> //' | $(call COLUMNIZE,' +# ')

#> make                             # the default goal: this help

# --------------------------------------------------------------------- build

# drivel is pure Go and links nothing itself, but the standard library is not:
# net and os/user carry cgo implementations (NSS-backed DNS and user lookup) that
# get compiled in whenever cgo is available. Go defaults CGO_ENABLED to 1 on a
# native build with a C compiler on PATH, so an unset value silently produces a
# binary dynamically linked against the build host's glibc — and one that will
# not start on a distro whose glibc is older.
#
# Defaulting to 0 is what makes the shipped artifact match what DESIGN.md §2.9
# claims: a static binary with no library dependencies, from a tree where every
# cross-compile failure traces to go-fuse and nothing else.
#
# Turning it on runs counter to that, and is deliberately reserved for ONE case:
# distro-specific packaged builds, where linking the system's libraries is the
# entire point. There the package tracks the OS's patch level, and patching those
# libraries becomes the distribution's job rather than a reason for us to cut a
# release. That trade is only worth making when something downstream is actually
# doing the patching — anywhere else it buys a portability problem and nothing.
#
# Scoped to this target on purpose: `go test -race` REQUIRES cgo, so hoisting
# this to a global would break the entire test suite with "-race requires cgo".
CGO_ENABLED ?= 0

#> make build                       # static, portable, no libc dependency
#> make build CGO_ENABLED=1         # distro packaging only; links the host's libc
## build: compile the drivel binary into bin/ (static; CGO_ENABLED=1 for distro packaging)
.PHONY: build
build:
	CGO_ENABLED=$(CGO_ENABLED) go build -o $(BIN)/drivel ./cmd/drivel

# The local run loop: build first, then serve. Building first is not a courtesy
# — debugging a mount against yesterday's binary is a long afternoon.
#
# MNT and DATA are separate from ARGS so the mode of the mount can be varied
# without restating it, and each is omitted when empty rather than passed blank:
# no DATA is in-place mode (Linux), and no MNT at all is the -config path, which
# refuses to be combined with flags describing what to mount. ARGS is handed to
# the binary verbatim; the flags themselves are documented in
# docs/user/configuration.md and by `drivel mount -h`, not restated here.
MNT  ?= ./mnt
DATA ?= ./data
ARGS ?=

#>> make run                                    # ./mnt over ./data; no credentials, so log-only
#>> make run ARGS='-debug'                      # ...with FUSE tracing on
#>> make run MNT=./dir DATA=                    # in-place: ./dir is its own backing (Linux only)
#>> make run MNT= DATA=                         # serve every mount in the XDG config file instead
#>> make run ARGS='-credentials creds.json'     # enable the Drive backend (token from `drivel login`)
#>> make run ARGS='-credentials c.json -lazy'   # ...lazily: placeholders, content on first read
#>> make run ARGS='-lazy -hydrate-workers 16'   # raise the cap on concurrent hydrations
#>> make run ARGS='-xattr'                      # enable xattr passthrough (off by default, deliberately)
#>> make run ARGS='-index ""'                   # disable the path↔ID index; costs quota, never correctness
#>> make run ARGS='-sweep-interval 0'           # disable periodic re-enumeration
#>> make run ARGS='-resync -max-deletes 0'      # force a sweep, uncap the deletions it may infer
#>> make run ARGS='-pprof localhost:6060'       # enable the pprof endpoint for this process
## run: build, then run the binary (MNT, DATA, ARGS; Ctrl-C unmounts)
.PHONY: run
run: build
	$(BIN)/drivel mount $(if $(MNT),-mount $(MNT)) $(if $(DATA),-data $(DATA)) $(ARGS)

# The shell completions are generated from the flag definitions in cmd/drivel,
# under the `completions` build tag — the renderer is build-time machinery and a
# released binary has no reason to carry it. See cmd/drivel/completions.go.
#
# The generated files are committed because whoever installs drivel from a
# tarball or a distro package has no Go toolchain to run this with; the check
# target below is what stops them going stale.
#> make completions                  # regenerate completions/ after adding a flag
## completions: regenerate the shell completion files from drivel's own flags
.PHONY: completions
completions:
	go run -tags completions ./cmd/drivel gen-completions -o completions

## completions-check: fail if the committed completions no longer match the flags
.PHONY: completions-check
completions-check:
	go run -tags completions ./cmd/drivel gen-completions -o completions -check

## clean: remove build output, installed tools and the test cache
.PHONY: clean
clean:
	rm -rf $(BIN)
	go clean -testcache

## install: binary, man page, shell completions + the mount(8) helper symlinks (Linux)
.PHONY: install
install: build
	install -d $(DESTDIR)$(PREFIX)/bin $(DESTDIR)$(MANDIR)/man1
	install -m0755 $(BIN)/drivel $(DESTDIR)$(PREFIX)/bin/drivel
	install -m0644 docs/user/drivel.1 $(DESTDIR)$(MANDIR)/man1/drivel.1
# The helper is the same binary under another name: mount(8) picks a program by
# the filesystem type, and argv[0] is the whole difference. A symlink rather than
# a second binary means there is no second thing to keep in version lockstep.
#
# Both names are installed because both fstab types are reasonable to write.
# fuse.drivel is the one the docs use: go-fuse mounts as "fuse." + its subtype, so
# that type is what /proc/self/mountinfo already reports, and systemd's
# fstab-generator compares the two.
	install -d $(DESTDIR)$(SBINDIR)
	ln -sf $(PREFIX)/bin/drivel $(DESTDIR)$(SBINDIR)/mount.fuse.drivel
	ln -sf $(PREFIX)/bin/drivel $(DESTDIR)$(SBINDIR)/mount.drivel
# The completion files, under the names each shell looks for: bash loads
# "drivel" (the command name) from its completions directory, zsh autoloads
# "_drivel" from $fpath. They are the committed, generated copies — `make
# completions` is what regenerates them, and completions-check is what keeps the
# committed pair honest.
	install -d $(DESTDIR)$(BASHCOMPDIR) $(DESTDIR)$(ZSHCOMPDIR)
	install -m0644 completions/drivel.bash $(DESTDIR)$(BASHCOMPDIR)/drivel
	install -m0644 completions/_drivel $(DESTDIR)$(ZSHCOMPDIR)/_drivel

## uninstall: remove what install placed
.PHONY: uninstall
uninstall:
	rm -f $(DESTDIR)$(PREFIX)/bin/drivel $(DESTDIR)$(MANDIR)/man1/drivel.1
	rm -f $(DESTDIR)$(SBINDIR)/mount.fuse.drivel $(DESTDIR)$(SBINDIR)/mount.drivel
	rm -f $(DESTDIR)$(BASHCOMPDIR)/drivel $(DESTDIR)$(ZSHCOMPDIR)/_drivel

# ---------------------------------------------------------------------- test

## test: race-enabled suite; skips tests whose kernel facilities are absent
.PHONY: test
test:
	go test -race ./...

#> make test-full                   # the whole suite, nothing silently skipped
## test-full: race suite with no silent skips — needs /dev/fuse and user xattrs
.PHONY: test-full
test-full:
	DRIVEL_REQUIRE_TESTENV=all go test -race ./...

## test-fast: no race instrumentation; the pre-commit lane
.PHONY: test-fast
test-fast:
	go test ./...

#> make cover                       # coverage profile into bin/coverage.out
## cover: race suite with a coverage profile written to bin/coverage.out
.PHONY: cover
cover:
	@mkdir -p $(BIN)
	go test -race -covermode=atomic -coverprofile=$(BIN)/coverage.out ./...
	go tool cover -func=$(BIN)/coverage.out | tail -1

# ----------------------------------------------------------------- lint & fmt

## fmt: apply formatting fixes in place
.PHONY: fmt
fmt: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) fmt

## fmt-check: fail on unformatted code without modifying anything
.PHONY: fmt-check
fmt-check: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) fmt --diff

## lint: run the golangci-lint suite
.PHONY: lint
lint: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run

#> make lint-new MAIN_BRANCH=main   # lint only what this branch adds
## lint-new: lint only what this branch adds (the retrofit ramp)
.PHONY: lint-new
lint-new: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run --new-from-merge-base=$(MAIN_BRANCH)

## lint-fix: run the suite and apply the fixes it can make itself
.PHONY: lint-fix
lint-fix: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run --fix

## vet: go vet — a subset of lint, but free and always installed
.PHONY: vet
vet:
	go vet ./...

## vuln: known vulnerabilities in dependencies and the toolchain
.PHONY: vuln
vuln: $(GOVULNCHECK)
	$(GOVULNCHECK) ./...

## tidy-check: fail if go.mod/go.sum are not tidy
.PHONY: tidy-check
tidy-check:
	go mod tidy -diff

# ----------------------------------------------------------------- aggregates

#> make precommit                   # the fast gate, before committing
#> make check                       # everything CI runs, in CI's order
## check: everything CI runs, in CI's order
.PHONY: check
check: tidy-check fmt-check completions-check lint test-full vuln

## precommit: the fast gate the pre-commit hook runs
.PHONY: precommit
precommit: fmt-check lint build

# --------------------------------------------------------------------- tools

#> make tools hooks                 # one-time setup, in a fresh clone
#> make clean tools                 # reinstall the tools after a version bump
## tools: install the pinned tool binaries into bin/
.PHONY: tools
tools: $(GOLANGCI_LINT) $(GOVULNCHECK) $(LEFTHOOK)

## hooks: install the git hooks (run once per clone, and after a version bump)
.PHONY: hooks
hooks: $(LEFTHOOK)
	@printf 'export LEFTHOOK_BIN=%s\n' '$(LEFTHOOK)' > .lefthook-rc.sh
	$(LEFTHOOK) install

$(GOLANGCI_LINT):
	@mkdir -p $(BIN)
	GOBIN=$(BIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	@mv $(BIN)/golangci-lint $@

$(GOVULNCHECK):
	@mkdir -p $(BIN)
	GOBIN=$(BIN) go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
	@mv $(BIN)/govulncheck $@

$(LEFTHOOK):
	@mkdir -p $(BIN)
	GOTOOLCHAIN=$(LEFTHOOK_GOTOOLCHAIN) GOBIN=$(BIN) go install github.com/evilmartians/lefthook@$(LEFTHOOK_VERSION)
	@mv $(BIN)/lefthook $@
