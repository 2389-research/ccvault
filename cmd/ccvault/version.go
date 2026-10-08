// ABOUTME: Holds the ldflags stamps the linker writes into package main
// ABOUTME: Resolves them, once, into the build report every command reports from

package main

import (
	"runtime/debug"
	"sync"

	"github.com/2389-research/ccvault/internal/buildinfo"
)

// Stamps written by the release pipeline with `-ldflags -X main.<name>=...`:
// the Makefile sets version, goreleaser and the release workflow set all
// three. Every other build leaves them empty, and buildinfo.Resolve falls
// back to the build information the Go toolchain embeds. They must stay
// package-level vars of type string in package main or the linker silently
// ignores the -X -- which is why they live here rather than in
// internal/buildinfo alongside the resolution they feed.
var (
	version string
	commit  string
	date    string
)

// currentBuild reports the build facts for this binary. It resolves lazily:
// the ldflags vars are set by the linker, but ReadBuildInfo is a runtime call
// and the result is wanted from command handlers, long after init.
var currentBuild = sync.OnceValue(func() buildinfo.Report {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		bi = nil
	}
	return buildinfo.Resolve(buildinfo.Stamps{Version: version, Commit: commit, Date: date}, bi)
})
