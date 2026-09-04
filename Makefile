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

## help: list available targets
.PHONY: help
help:
	@grep -hE '^## ' $(MAKEFILE_LIST) | sed 's/^## //' | awk -F ': *' '{printf "  \033[1m%-12s\033[0m %s\n", $$1, $$2}'

# --------------------------------------------------------------------- build

## build: compile the drivel binary into bin/
.PHONY: build
build:
	go build -o $(BIN)/drivel ./cmd/drivel

## clean: remove build output, installed tools and the test cache
.PHONY: clean
clean:
	rm -rf $(BIN)
	go clean -testcache

# ---------------------------------------------------------------------- test

## test: race-enabled suite; skips tests whose kernel facilities are absent
.PHONY: test
test:
	go test -race ./...

## test-full: race suite with no silent skips — needs /dev/fuse and user xattrs
.PHONY: test-full
test-full:
	DRIVEL_REQUIRE_TESTENV=all go test -race ./...

## test-fast: no race instrumentation; the pre-commit lane
.PHONY: test-fast
test-fast:
	go test ./...

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

## check: everything CI runs, in CI's order
.PHONY: check
check: tidy-check fmt-check lint test-full vuln

## precommit: the fast gate the pre-commit hook runs
.PHONY: precommit
precommit: fmt-check lint build

# --------------------------------------------------------------------- tools

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
