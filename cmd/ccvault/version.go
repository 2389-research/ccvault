// ABOUTME: Resolves the version, commit and build date that ccvault reports about itself
// ABOUTME: Prefers release ldflags stamps and falls back to the build info Go embeds

package main

import (
	"fmt"
	"runtime/debug"
	"sync"
)

// Stamps written by the release pipeline with `-ldflags -X main.<name>=...`:
// the Makefile sets version, goreleaser and the release workflow set all
// three. Every other build leaves them empty, and resolveBuild falls back to
// the build information the Go toolchain embeds. They must stay package-level
// vars of type string in package main or the linker silently ignores the -X.
var (
	version string
	commit  string
	date    string
)

// unknown is what a build report shows for a fact this binary does not carry.
// It is a literal rather than an empty field so that a script reading
// `ccvault version` always finds every key.
const unknown = "unknown"

// buildStamps holds the raw ldflags values so resolveBuild can be tested
// without relinking the binary for every case.
type buildStamps struct {
	Version string
	Commit  string
	Date    string
}

// buildReport is what ccvault knows about its own build, rendered for display.
type buildReport struct {
	// Version is a release version ("0.3.0"), a `git describe` string from
	// `make build`, the module version of a `go install pkg@version`, or
	// "dev" for a build from a source tree.
	Version string
	// Commit is the full revision this binary was built from, with "+dirty"
	// appended when the tree had uncommitted changes, or unknown.
	Commit string
	// Date is when this binary was built, or unknown. An unstamped build
	// cannot know its own build time, so it reports the commit timestamp
	// alongside the unknown rather than passing one off as the other.
	Date string
}

// Text is the body of `ccvault version`. The first line is unchanged from
// earlier releases -- "ccvault <version>" -- so scripts that read the version
// off it keep working; the provenance lines follow as "key: value".
func (b buildReport) Text() string {
	return fmt.Sprintf("ccvault %s\ncommit: %s\nbuilt:  %s\n", b.Version, b.Commit, b.Date)
}

// currentBuild reports the build facts for this binary. It resolves lazily:
// the ldflags vars are set by the linker, but ReadBuildInfo is a runtime call
// and the result is wanted from command handlers, long after init.
var currentBuild = sync.OnceValue(func() buildReport {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		bi = nil
	}
	return resolveBuild(buildStamps{Version: version, Commit: commit, Date: date}, bi)
})

// resolveBuild merges the ldflags stamps with Go's embedded build info.
//
// A stamp always wins: it is the only source that knows the release version.
// Without stamps the build info still carries plenty. A binary from
// `go install <module>@<version>` records that version and no VCS settings,
// so its version is real and gets reported as-is. A binary built from a
// source tree records VCS settings, and the version Go synthesizes there is a
// pseudo-version above the last tag -- "v0.2.1-0.2026...-8a3df353" for a repo
// whose newest tag is v0.2.0. Reporting that would hand out a version number
// for a release that does not exist, so a source-tree build calls itself
// "dev" and leans on the commit for identification.
func resolveBuild(s buildStamps, bi *debug.BuildInfo) buildReport {
	var vcsRevision, vcsTime string
	var fromVCS, vcsDirty bool
	if bi != nil {
		for _, setting := range bi.Settings {
			switch setting.Key {
			case "vcs":
				fromVCS = true
			case "vcs.revision":
				vcsRevision = setting.Value
			case "vcs.time":
				vcsTime = setting.Value
			case "vcs.modified":
				vcsDirty = setting.Value == "true"
			}
		}
	}

	report := buildReport{Version: "dev", Commit: unknown, Date: unknown}

	switch {
	case s.Version != "":
		report.Version = s.Version
	case !fromVCS && bi != nil && isModuleVersion(bi.Main.Version):
		report.Version = bi.Main.Version
	}

	switch {
	case s.Commit != "":
		report.Commit = s.Commit
	case vcsRevision != "":
		report.Commit = vcsRevision
		if vcsDirty {
			report.Commit += "+dirty"
		}
	}

	switch {
	case s.Date != "":
		report.Date = s.Date
	case vcsTime != "":
		// Not the build date -- the commit date. Say so rather than let a
		// reader take it for when the binary was produced.
		report.Date = fmt.Sprintf("%s (commit %s)", unknown, vcsTime)
	}

	return report
}

// isModuleVersion reports whether v is a version a module proxy handed us
// rather than a placeholder. The Go toolchain writes "(devel)" for a main
// module it has no version for.
func isModuleVersion(v string) bool {
	return v != "" && v != "(devel)"
}
