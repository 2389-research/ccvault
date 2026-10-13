// ABOUTME: Drives the real binary's `version` command for each way ccvault gets built
// ABOUTME: Pins that release ldflags land and that an unstamped build does not invent a version

package integration

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
// diagnostic. Now it says "dev", and every provenance field it prints is
// either a fact the binary actually carries or an admission that it does not.
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

	// The anti-invention guarantee: the commit this binary reports is
	// exactly the revision the toolchain embedded in it, or the honest
	// placeholder when it embedded none. Never something made up in
	// between.
	//
	// The embedded value is read back out of the binary rather than taken
	// from git, because whether Go stamps VCS data at all is a property of
	// the build environment, not of ccvault. `-buildvcs=auto` stamps
	// nothing when it cannot run its probe -- a linked git worktree, where
	// every agent working on this repo builds, is one such case, and a
	// source archive with no .git is another. Asserting that the stamping
	// happened tests the environment; asserting that the report agrees
	// with whatever was stamped tests ccvault, and holds in both.
	wantCommit := embeddedCommit(t, ccvaultBinary(t))
	if fields["commit"] != wantCommit {
		t.Errorf("commit = %q, want %q (the revision embedded in the binary)", fields["commit"], wantCommit)
	}

	// An unstamped build cannot know when it was built. It may name the
	// commit timestamp for orientation, but the "unknown" has to stay --
	// a bare date here would read as a build time nobody recorded.
	if !strings.HasPrefix(fields["built"], "unknown") {
		t.Errorf("built = %q, want it to begin with %q: an unstamped build has no build date", fields["built"], "unknown")
	}
}

// embeddedCommit reports the commit `ccvault version` must print for bin,
// derived from the VCS settings the Go toolchain embedded in it: the full
// revision, "+dirty" when the tree was modified, or "unknown" when the build
// carries no VCS data at all.
func embeddedCommit(t *testing.T, bin string) string {
	t.Helper()

	//nolint:gosec // G204: bin is a binary this test just built
	out, err := exec.Command("go", "version", "-m", bin).CombinedOutput()
	if err != nil {
		t.Fatalf("go version -m %s: %v\n%s", bin, err, out)
	}

	settings := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "build" {
			continue
		}
		if key, value, found := strings.Cut(fields[1], "="); found {
			settings[key] = value
		}
	}

	revision, ok := settings["vcs.revision"]
	if !ok {
		return "unknown"
	}
	if !isHexRevision(revision) {
		t.Fatalf("embedded vcs.revision = %q, which is not a git object name", revision)
	}
	if settings["vcs.modified"] == "true" {
		return revision + "+dirty"
	}
	return revision
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

var commitStampPattern = regexp.MustCompile(`-X main\.commit=(\S+)`)

// makeCommitStamp reads the commit the Makefile would stamp, by asking make
// to print the build command rather than run it. Nothing is compiled and no
// binary is written, so this stays fast and leaves the tree alone.
func makeCommitStamp(t *testing.T, makeArgs ...string) string {
	t.Helper()

	if _, err := exec.LookPath("make"); err != nil {
		t.Skipf("make unavailable: %v", err)
	}

	args := append([]string{"-n", "build"}, makeArgs...)
	//nolint:gosec // G204: args are literals from this test
	cmd := exec.Command("make", args...)
	cmd.Dir = repoRoot(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make %s: %v\n%s", strings.Join(args, " "), err, out)
	}

	match := commitStampPattern.FindStringSubmatch(string(out))
	if match == nil {
		t.Fatalf("make %s printed no -X main.commit stamp:\n%s", strings.Join(args, " "), out)
	}
	return match[1]
}

// gitOutput runs a git command in the repository the Makefile builds from.
func gitOutput(t *testing.T, args ...string) string {
	t.Helper()

	//nolint:gosec // G204: args are literals from this test
	cmd := exec.Command("git", args...)
	cmd.Dir = repoRoot(t)
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("git %s unavailable: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out))
}

// dirtyTheTree adds an untracked file to the repository for the duration of
// the test. The name is deliberately not covered by .gitignore, since an
// ignored file would leave `git status` clean and prove nothing.
//
// The name has no extension and says what it is, because the probe only has
// to be invisible to one gitignore to break the test. The first version used
// a ".tmp" suffix, which the repository ignores nothing about but the
// author's global config does -- so it passed in CI, where hosted runners
// have no global gitignore, and failed on the machine. Green in CI and red
// locally is the worst direction for a failure: it teaches people to
// distrust their own test runs.
//
// Nearly every pattern a global gitignore carries is an extension glob
// (*.tmp, *.log, *.bak, *.swp, *~), a directory, or a dotfile, so an
// extension-free name at the repository root survives all of them. That is
// an argument about likelihood though, not a guarantee, so the assumption is
// checked rather than trusted.
func dirtyTheTree(t *testing.T) {
	t.Helper()

	probe := filepath.Join(repoRoot(t), "makefile-dirty-probe-do-not-ignore")

	// Ask git rather than guess. check-ignore answers from every source that
	// applies -- the repository's .gitignore, .git/info/exclude and the
	// user's core.excludesFile -- so a future pattern that would swallow the
	// probe is reported by name instead of surfacing as a confusing failure.
	if rule := gitIgnoreRule(t, probe); rule != "" {
		t.Skipf("probe %s is covered by a gitignore rule and cannot dirty the tree: %s", probe, rule)
	}

	if err := os.WriteFile(probe, []byte("probe\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", probe, err)
	}
	t.Cleanup(func() { _ = os.Remove(probe) })

	// The probe is only useful if the real `git status --porcelain` -- the
	// command the Makefile runs -- reports it. Checked so the test can never
	// pass vacuously against a tree git still considers clean.
	if gitOutput(t, "status", "--porcelain") == "" {
		t.Fatalf("%s did not register as a change, so there is no dirty tree to test against", probe)
	}
}

// gitIgnoreRule returns the gitignore rule covering path, as
// "<source>:<line>:<pattern>", or "" when nothing ignores it.
func gitIgnoreRule(t *testing.T, path string) string {
	t.Helper()

	//nolint:gosec // G204: path is the probe this harness built from repoRoot
	cmd := exec.Command("git", "check-ignore", "-v", path)
	cmd.Dir = repoRoot(t)
	out, err := cmd.Output()
	if err == nil {
		// Exit 0: the path is ignored, and stdout names the rule.
		return strings.TrimSpace(string(out))
	}

	// Exit 1 is the answer "not ignored", not a failure. Anything else means
	// git could not answer, and a guess is worse than saying so.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return ""
	}
	t.Skipf("git check-ignore could not answer for %s: %v", path, err)
	return ""
}

// TestMakeBuild_CommitStampAdmitsADirtyTree pins the halves of the version
// output agreeing with each other. VERSION comes from `git describe --dirty`
// and says so when the tree has changes; the commit stamp said nothing, so
// one `ccvault version` gave two answers -- a version admitting it was dirty
// beside a commit claiming to be clean.
//
// It mattered more here than it would elsewhere: resolveBuild gives an
// ldflags stamp priority over the ReadBuildInfo fallback, and that fallback
// does append "+dirty". Without this, `make build` was strictly less honest
// than a plain `go build`, which is backwards.
func TestMakeBuild_CommitStampAdmitsADirtyTree(t *testing.T) {
	head := gitOutput(t, "rev-parse", "HEAD")

	// Only assert the clean case when the tree actually is clean; a
	// developer running this mid-edit should not see a spurious failure.
	if gitOutput(t, "status", "--porcelain") == "" {
		if got := makeCommitStamp(t); got != head {
			t.Errorf("clean tree stamped commit %q, want bare HEAD %q", got, head)
		}
	}

	dirtyTheTree(t)

	if got := makeCommitStamp(t); got != head+"+dirty" {
		t.Errorf("dirty tree stamped commit %q, want %q", got, head+"+dirty")
	}
}

// TestMakeBuild_ExplicitCommitIsStampedVerbatim covers the other half: a
// caller passing COMMIT in must get exactly that, with no second "+dirty"
// glued on by the Makefile. The dirty tree is what makes the test meaningful
// -- that is the only condition under which doubling could happen.
func TestMakeBuild_ExplicitCommitIsStampedVerbatim(t *testing.T) {
	dirtyTheTree(t)

	const want = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef+dirty"
	if got := makeCommitStamp(t, "COMMIT="+want); got != want {
		t.Errorf("explicit COMMIT stamped as %q, want %q", got, want)
	}
}
