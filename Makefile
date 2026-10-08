# ABOUTME: Build and development targets for ccvault
# ABOUTME: Provides build, test, and release automation

.PHONY: build test test-race test-short test-coverage clean install lint release

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
