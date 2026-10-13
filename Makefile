# ABOUTME: Build and development targets for ccvault
# ABOUTME: Provides build, test, and release automation

.PHONY: build test test-race test-short test-precommit test-coverage clean install lint release

# Packages excluded from the commit-time gate, as an extended-regexp anchored
# at a path segment. Measured on a warm build cache with -race: internal/mcp
# 157s and internal/db 73s, against 30-40s for the twenty that remain, all of
# them together -- `go test` runs packages in parallel, so those two alone set
# the wall clock. Both are covered by `make test-race` in CI on every PR.
#
# Deliberately a skip list and not an allowlist: a package added tomorrow is
# gated from its first commit instead of being quietly left out.
PRECOMMIT_SKIP := /(internal/db|internal/mcp)$$

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")

# "+dirty" when the tree has changes, empty otherwise, and empty outside a
# repository. Resolved to the literal suffix in the shell rather than to a
# file list, so no filename ever reaches a make variable.
#
# This is the notion of dirty Go's own build info uses for vcs.modified --
# `git status --porcelain`, untracked files included -- so the commit line
# reads the same whether the stamp came from here or from the ReadBuildInfo
# fallback. VERSION above is tracked-only, because that is what upstream
# `git describe --dirty` does and it is not worth reimplementing.
GIT_DIRTY := $(shell test -z "$$(git status --porcelain 2>/dev/null)" || echo '+dirty')

# Part of the value, not appended in LDFLAGS: a caller passing COMMIT in gets
# it stamped verbatim, with no second suffix glued on.
COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null)$(GIT_DIRTY)
DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

# All three stamps, so `make build` rehearses what the release pipeline does
# and a broken -X shows up here rather than at tag time. A build with no
# stamps still reports honestly -- see cmd/ccvault/version.go.
LDFLAGS := -ldflags "-X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)"

build:
	go build $(LDFLAGS) -o ccvault ./cmd/ccvault

test:
	go test ./... -v

test-race:
	go test ./... -race

test-short:
	go test ./... -short

# The pre-commit gate. Everything `make test-race` runs, minus the two
# packages named in PRECOMMIT_SKIP above, and with no -short so the
# integration suite actually executes.
#
# -count=1 defeats the test cache. It costs the full 30-40s on every Go commit
# even when nothing relevant changed, and it is worth it twice over: a gate
# that reports a cached pass is the kind of gate this target exists to
# replace, and race detection is probabilistic, so a fresh run is a fresh roll
# of the dice rather than a repeat of one that already came up clean.
#
# -timeout 5m is per test binary, and 30s -- the value this replaces -- was
# not enough for this suite on two counts. Measured under -race, internal/tui
# takes 25s and test/integration 22s, so the old cap was marginal even for
# packages that were working. And test/integration shells out to `go build`
# from inside the test (version_test.go, cli_test.go), so a cold build cache
# is charged against the timeout rather than paid before it -- which is how
# that cap produced a goroutine dump indistinguishable from a real deadlock.
# Dropping -short makes that path run on every commit, so the headroom is now
# load-bearing rather than precautionary. At 5m, a timeout here means a hang.
test-precommit:
	@pkgs=$$(go list ./... | grep -Ev '$(PRECOMMIT_SKIP)') || exit 1; \
	go test -race -count=1 -timeout 5m $$pkgs

test-coverage:
	go test ./internal/db/... ./pkg/parser/... -coverprofile=coverage.out -covermode=atomic

lint:
	golangci-lint run

clean:
	rm -f ccvault
	rm -rf dist/

install:
	go install $(LDFLAGS) ./cmd/ccvault

# Build for multiple platforms
release: clean
	mkdir -p dist
	GOOS=darwin GOARCH=amd64 go build $(LDFLAGS) -o dist/ccvault-darwin-amd64 ./cmd/ccvault
	GOOS=darwin GOARCH=arm64 go build $(LDFLAGS) -o dist/ccvault-darwin-arm64 ./cmd/ccvault
	GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o dist/ccvault-linux-amd64 ./cmd/ccvault
	GOOS=linux GOARCH=arm64 go build $(LDFLAGS) -o dist/ccvault-linux-arm64 ./cmd/ccvault

# Sync and show stats
sync:
	go run ./cmd/ccvault sync

stats:
	go run ./cmd/ccvault stats

tui:
	go run ./cmd/ccvault tui

# Build analytics cache
cache:
	go run ./cmd/ccvault build-cache
