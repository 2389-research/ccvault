// ABOUTME: Tests that build facts resolve from ldflags stamps or embedded build info
// ABOUTME: Covers release, make build, go install and source-tree builds

package main

import (
	"runtime/debug"
	"strings"
	"testing"
)

// buildInfo builds a *debug.BuildInfo with the given main-module version and
// VCS settings, the two things resolveBuild reads.
func buildInfo(mainVersion string, settings map[string]string) *debug.BuildInfo {
	bi := &debug.BuildInfo{}
	bi.Main.Version = mainVersion
	for k, v := range settings {
		bi.Settings = append(bi.Settings, debug.BuildSetting{Key: k, Value: v})
	}
	return bi
}

func TestResolveBuild(t *testing.T) {
	sourceTree := map[string]string{
		"vcs":          "git",
		"vcs.revision": "8a3df3533c167834ae79fddc557f0268610604bd",
		"vcs.time":     "2026-10-07T21:09:16Z",
		"vcs.modified": "false",
	}

	cases := []struct {
		name   string
		stamps buildStamps
		info   *debug.BuildInfo
		want   buildReport
	}{
		{
			// The release workflow and goreleaser stamp all three. Each
			// must survive to the report, which is what the -X flags for
			// commit and date were silently failing to do.
			name: "release build stamps every field",
			stamps: buildStamps{
				Version: "0.3.0",
				Commit:  "8a3df3533c167834ae79fddc557f0268610604bd",
				Date:    "2026-10-08T12:34:56Z",
			},
			info: buildInfo("(devel)", sourceTree),
			want: buildReport{
				Version: "0.3.0",
				Commit:  "8a3df3533c167834ae79fddc557f0268610604bd",
				Date:    "2026-10-08T12:34:56Z",
			},
		},
		{
			// `make build` stamps only the version, from git describe. The
			// commit and date still come from the embedded build info.
			name:   "make build stamps the version and borrows the rest",
			stamps: buildStamps{Version: "v0.2.0-145-g8a3df35-dirty"},
			info:   buildInfo("(devel)", sourceTree),
			want: buildReport{
				Version: "v0.2.0-145-g8a3df35-dirty",
				Commit:  "8a3df3533c167834ae79fddc557f0268610604bd",
				Date:    "unknown (commit 2026-10-07T21:09:16Z)",
			},
		},
		{
			// A plain `go build` in a git tree: no stamps at all, and the
			// version Go synthesizes is a pseudo-version above the newest
			// tag. Reporting it would name a release that does not exist.
			name:   "source tree build calls itself dev",
			stamps: buildStamps{},
			info: buildInfo("v0.2.1-0.20261007210916-8a3df3533c16", map[string]string{
				"vcs":          "git",
				"vcs.revision": "8a3df3533c167834ae79fddc557f0268610604bd",
				"vcs.time":     "2026-10-07T21:09:16Z",
				"vcs.modified": "true",
			}),
			want: buildReport{
				Version: "dev",
				Commit:  "8a3df3533c167834ae79fddc557f0268610604bd+dirty",
				Date:    "unknown (commit 2026-10-07T21:09:16Z)",
			},
		},
		{
			// `go install github.com/2389-research/ccvault/cmd/ccvault@latest`
			// -- the README's second install route. No ldflags reach it and
			// no VCS tree backs it, but the module version is real.
			name:   "module install reports the module version",
			stamps: buildStamps{},
			info:   buildInfo("v0.2.0", nil),
			want: buildReport{
				Version: "v0.2.0",
				Commit:  "unknown",
				Date:    "unknown",
			},
		},
		{
			// A source archive with no .git, built with no stamps: nothing
			// is knowable, and the report says so instead of guessing.
			name:   "unstamped build outside a repo knows nothing",
			stamps: buildStamps{},
			info:   buildInfo("(devel)", nil),
			want: buildReport{
				Version: "dev",
				Commit:  "unknown",
				Date:    "unknown",
			},
		},
		{
			name:   "missing build info degrades to dev",
			stamps: buildStamps{},
			info:   nil,
			want: buildReport{
				Version: "dev",
				Commit:  "unknown",
				Date:    "unknown",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveBuild(tc.stamps, tc.info)
			if got != tc.want {
				t.Errorf("resolveBuild() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestResolveBuildNeverReportsAHardcodedVersion pins the defect that made
// `ccvault version` useless as a diagnostic: the fallback used to be the
// literal "0.1.0", a plausible-looking number that no build ever was.
func TestResolveBuildNeverReportsAHardcodedVersion(t *testing.T) {
	got := resolveBuild(buildStamps{}, buildInfo("(devel)", nil))
	if got.Version != "dev" {
		t.Errorf("unstamped version = %q, want %q", got.Version, "dev")
	}
}

// TestBuildReportTextKeepsFirstLineParseable guards the output contract: the
// first line stays "ccvault <version>" so a script reading field 2 off it
// survives the provenance lines being added below.
func TestBuildReportTextKeepsFirstLineParseable(t *testing.T) {
	text := buildReport{Version: "0.3.0", Commit: "abc123", Date: "2026-10-08T12:34:56Z"}.Text()

	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("version output has %d lines, want 3:\n%s", len(lines), text)
	}
	if lines[0] != "ccvault 0.3.0" {
		t.Errorf("first line = %q, want %q", lines[0], "ccvault 0.3.0")
	}
	if fields := strings.Fields(lines[0]); len(fields) != 2 || fields[1] != "0.3.0" {
		t.Errorf("first line does not parse as `ccvault <version>`: %q", lines[0])
	}
	for i, want := range []string{"commit: abc123", "built:  2026-10-08T12:34:56Z"} {
		if lines[i+1] != want {
			t.Errorf("line %d = %q, want %q", i+2, lines[i+1], want)
		}
	}
}
