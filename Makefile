# ABOUTME: Build and development targets for ccvault
# ABOUTME: Provides build, test, and release automation

.PHONY: build build-server build-all test test-race test-short test-coverage clean install lint release

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -ldflags "-X main.version=$(VERSION)"

build:
	go build $(LDFLAGS) -o ccvault ./cmd/ccvault

build-server:
	go build $(LDFLAGS) -o ccvaultd ./cmd/ccvaultd

build-all: build build-server

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
	rm -f ccvault ccvaultd
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
