// ABOUTME: Drives the real binary's `version` command for each way ccvault gets built
// ABOUTME: Pins that release ldflags land and that an unstamped build does not invent a version

package integration

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildStamped builds cmd/ccvault with the given -X stamps and returns the
// path. Unlike ccvaultBinary it builds per call, because the whole point is
// to vary the link-time values.
func buildStamped(t *testing.T, stamps ...string) string {
	t.Helper()

	root := repoRoot(t)
	out := filepath.Join(t.TempDir(), "ccvault")

	ldflags := strings.Join(stamps, " ")
	//nolint:gosec // G204: out is under t.TempDir(); stamps are literals from this test
	cmd := exec.Command("go", "build", "-ldflags", ldflags, "-o", out, "github.com/2389-research/ccvault/cmd/ccvault")
	cmd.Dir = root
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build with ldflags %q: %v\n%s", ldflags, err, combined)
	}
	return out
}

// versionFields runs `version` on a binary and splits the output into its
// first-line version and its "key: value" provenance lines.
func versionFields(t *testing.T, bin string) (version string, fields map[string]string) {
	t.Helper()

	//nolint:gosec // G204: bin is a binary this test just built
	out, err := exec.Command(bin, "version").CombinedOutput()
	if err != nil {
		t.Fatalf("ccvault version: %v\n%s", err, out)
	}

	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	parts := strings.Fields(lines[0])
	if len(parts) != 2 || parts[0] != "ccvault" {
		t.Fatalf("first line is not `ccvault <version>`: %q", lines[0])
	}

	fields = map[string]string{}
	for _, line := range lines[1:] {
		key, value, found := strings.Cut(line, ":")
		if !found {
			t.Errorf("provenance line is not `key: value`: %q", line)
			continue
		}
		fields[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return parts[1], fields
}

// TestVersion_ReleaseStampsReachTheOutput is the regression test for release
// builds that dropped two thirds of what they injected. The release workflow
// and goreleaser both pass -X for main.version, main.commit and main.date;
// the linker ignores an -X naming a symbol that does not exist, so for every
// release cut so far the commit and date went nowhere, silently.
func TestVersion_ReleaseStampsReachTheOutput(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}

	const (
		wantVersion = "9.9.9"
		wantCommit  = "0123456789abcdef0123456789abcdef01234567"
		wantDate    = "2026-10-08T12:34:56Z"
	)
	bin := buildStamped(t,
		"-X", "main.version="+wantVersion,
		"-X", "main.commit="+wantCommit,
		"-X", "main.date="+wantDate,
	)

	version, fields := versionFields(t, bin)
	if version != wantVersion {
		t.Errorf("version = %q, want %q", version, wantVersion)
	}
	if fields["commit"] != wantCommit {
		t.Errorf("commit = %q, want %q", fields["commit"], wantCommit)
	}
	if fields["built"] != wantDate {
		t.Errorf("built = %q, want %q", fields["built"], wantDate)
	}
}

// TestVersion_UnstampedBuildDoesNotInventAVersion covers the plain `go build`
// every contributor and test harness uses. It used to report the hardcoded
// "0.1.0", a version no build ever was, which made `version` worthless as a
// diagnostic. Now it says "dev" and names the commit it came from.
func TestVersion_UnstampedBuildDoesNotInventAVersion(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}

	version, fields := versionFields(t, ccvaultBinary(t))

	if version != "dev" {
		t.Errorf("unstamped version = %q, want %q", version, "dev")
	}
	for _, key := range []string{"commit", "built"} {
		if _, ok := fields[key]; !ok {
			t.Errorf("version output has no %q field: %+v", key, fields)
		}
	}

	// Go stamps VCS data into any build made from a repository, so the
	// commit must be a real revision rather than the unknown placeholder.
	//
	// This checks the shape and not a specific revision on purpose. Which
	// revision Go stamps depends on which repository root its VCS probe
	// settles on, and that is not always this checkout's HEAD: build inside
	// a linked worktree nested under the main checkout -- how the agents
	// working on this repo are set up -- and the probe walks up to the
	// outer repository and stamps its HEAD instead. Pinning an exact
	// revision would fail there while proving nothing extra.
	commit := strings.TrimSuffix(fields["commit"], "+dirty")
	if !isHexRevision(commit) {
		t.Errorf("commit = %q, want a hex revision from the embedded VCS data", fields["commit"])
	}
}

// isHexRevision reports whether s is a full-length git object name.
func isHexRevision(s string) bool {
	if len(s) != 40 {
		return false
	}
	return strings.IndexFunc(s, func(r rune) bool {
		return !strings.ContainsRune("0123456789abcdef", r)
	}) == -1
}
