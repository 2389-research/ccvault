// ABOUTME: Resolves the version, commit and build date that ccvault reports about itself
// ABOUTME: Prefers release ldflags stamps and falls back to the build info Go embeds

// Package buildinfo answers what build this binary is, for every part of
// ccvault that has to say so: the `version` command and the version the MCP
// server advertises to its clients.
//
// The ldflags stamps themselves stay in package main, because the Makefile,
// goreleaser and the release workflow all pass `-X main.<name>=...` and the
// linker silently ignores an -X naming a symbol that does not exist. Package
// main reads its stamps into Stamps and calls Resolve; everything else is
// handed the resulting Report.
package buildinfo

import (
	"fmt"
	"runtime/debug"
)

// Unknown is what a build report shows for a fact this binary does not carry.
// It is a literal rather than an empty field so that a script reading
// `ccvault version` always finds every key, and so that the version an MCP
// client is told is never the empty string.
const Unknown = "unknown"

// Stamps holds the raw ldflags values. Taking them as an argument is what
// lets Resolve be tested without relinking the binary for every case.
type Stamps struct {
	Version string
	Commit  string
	Date    string
}

// Report is what ccvault knows about its own build, rendered for display.
type Report struct {
	// Version is a release version ("0.3.0"), a `git describe` string from
	// `make build`, the module version of a `go install pkg@version`, or
	// "dev" for a build from a source tree.
	Version string
	// Commit is the full revision this binary was built from, with "+dirty"
	// appended when the tree had uncommitted changes, or Unknown.
	Commit string
	// Date is when this binary was built, or Unknown. An unstamped build
	// cannot know its own build time, so it reports the commit timestamp
	// alongside the Unknown rather than passing one off as the other.
	Date string
}

// Text is the body of `ccvault version`. The first line is unchanged from
// earlier releases -- "ccvault <version>" -- so scripts that read the version
// off it keep working; the provenance lines follow as "key: value".
func (b Report) Text() string {
	return fmt.Sprintf("ccvault %s\ncommit: %s\nbuilt:  %s\n", b.Version, b.Commit, b.Date)
}

// Resolve merges the ldflags stamps with Go's embedded build info.
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
func Resolve(s Stamps, bi *debug.BuildInfo) Report {
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

	report := Report{Version: "dev", Commit: Unknown, Date: Unknown}

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
		report.Date = fmt.Sprintf("%s (commit %s)", Unknown, vcsTime)
	}

	return report
}

// isModuleVersion reports whether v is a version a module proxy handed us
// rather than a placeholder. The Go toolchain writes "(devel)" for a main
// module it has no version for.
func isModuleVersion(v string) bool {
	return v != "" && v != "(devel)"
}
