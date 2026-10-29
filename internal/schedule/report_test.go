// ABOUTME: Tests that Assemble detects every failure mode `ccvault schedule status` claims to detect
// ABOUTME: One test per claim — not registered, binary gone, binary not the one on PATH, run failed, archive stale

package schedule

import (
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

// healthyFacts is a schedule with nothing wrong with it: registered, pointing
// at a binary that exists and is the one on PATH, last run succeeded, archive
// synced minutes ago. Each test below breaks exactly one thing.
func healthyFacts() Facts {
	return Facts{
		Platform:  "launchd",
		Supported: true,
		Label:     DefaultLabel,
		Registration: Registration{
			Found:         true,
			UnitPath:      "/Users/d/Library/LaunchAgents/ai.2389.ccvault.sync.plist",
			Program:       "/Users/d/go/bin/ccvault",
			Arguments:     []string{"/Users/d/go/bin/ccvault", "sync"},
			Interval:      time.Hour,
			Runs:          42,
			RunsKnown:     true,
			LastExitCode:  0,
			LastExitKnown: true,
			LastRun:       testNow.Add(-20 * time.Minute),
			LastRunKnown:  true,
		},
		ProgramResolved: "/Users/d/go/bin/ccvault",
		PathProgram:     "/Users/d/go/bin/ccvault",
		PathResolved:    "/Users/d/go/bin/ccvault",
		LastSynced:      testNow.Add(-20 * time.Minute),
		LogPath:         "/Users/d/.ccvault/schedule.log",
		Now:             testNow,
	}
}

func TestAssemble_HealthyScheduleHasNoProblems(t *testing.T) {
	r := Assemble(healthyFacts())

	if !r.Healthy() {
		t.Fatalf("Healthy() = false for a sound schedule; problems = %+v", r.Problems)
	}
	if !r.Registered {
		t.Error("Registered = false")
	}
	if !r.ProgramExists {
		t.Error("ProgramExists = false")
	}
	if !r.PathMatches {
		t.Error("PathMatches = false")
	}
	if !r.LastRunSucceeded {
		t.Error("LastRunSucceeded = false")
	}
	if r.Stale {
		t.Error("Stale = true for an archive synced 20 minutes ago")
	}
	if got, want := r.Staleness, 20*time.Minute; got != want {
		t.Errorf("Staleness = %s, want %s", got, want)
	}
	if n := r.Notice(); n != "" {
		t.Errorf("Notice() = %q, want empty — stats must be silent when clean", n)
	}
}

// Failure 1: no unit registered.
func TestAssemble_DetectsNoUnitRegistered(t *testing.T) {
	f := healthyFacts()
	f.Registration = Registration{}

	r := Assemble(f)

	if r.Registered {
		t.Error("Registered = true with a zero Registration")
	}
	detail, ok := r.Problem(ProblemNotRegistered)
	if !ok {
		t.Fatalf("no %s problem reported; problems = %+v", ProblemNotRegistered, r.Problems)
	}
	if !strings.Contains(detail, "schedule install") {
		t.Errorf("detail = %q, want it to name the command that fixes it", detail)
	}
	// With no job, the binary facts are not claims this report gets to make.
	if r.ProgramExists || r.PathMatches {
		t.Error("an unregistered job reported facts about its binary")
	}
	if _, ok := r.Problem(ProblemBinaryMissing); ok {
		t.Error("an unregistered job also complained its binary was missing — one condition, two complaints")
	}
}

// Failure 2: unit registered but the binary path no longer exists. This is
// the mise/GOBIN trap from #171 — GOBIN embeds the Go version, so bumping Go
// moves the path and the unit keeps pointing at the old one.
func TestAssemble_DetectsRegisteredBinaryGone(t *testing.T) {
	f := healthyFacts()
	f.Registration.Program = "/Users/d/.local/share/mise/installs/go/1.25.5/bin/ccvault"
	f.ProgramResolved = "" // the path does not resolve: it is not there
	f.PathProgram = "/Users/d/.local/share/mise/installs/go/1.26.0/bin/ccvault"
	f.PathResolved = f.PathProgram

	r := Assemble(f)

	if r.ProgramExists {
		t.Error("ProgramExists = true for a path that does not resolve")
	}
	detail, ok := r.Problem(ProblemBinaryMissing)
	if !ok {
		t.Fatalf("no %s problem reported; problems = %+v", ProblemBinaryMissing, r.Problems)
	}
	if !strings.Contains(detail, "1.25.5") {
		t.Errorf("detail = %q, want it to name the dead path", detail)
	}
	if !strings.Contains(detail, "schedule install") {
		t.Errorf("detail = %q, want it to name the fix", detail)
	}
	// A path that isn't there cannot be compared against PATH, and reporting
	// both would be two complaints about one condition.
	if _, ok := r.Problem(ProblemBinaryMismatch); ok {
		t.Error("a missing binary also reported a PATH mismatch")
	}
}

// Failure 3: the binary exists but is not the ccvault on PATH. A stale
// scheduled binary against a newer archive is #105 automated, and #105's
// guard refuses to write — with nobody watching.
func TestAssemble_DetectsBinaryPresentButNotTheOneOnPath(t *testing.T) {
	f := healthyFacts()
	f.Registration.Program = "/Users/d/.local/share/mise/installs/go/1.25.5/bin/ccvault"
	f.ProgramResolved = f.Registration.Program
	f.PathProgram = "/opt/homebrew/bin/ccvault"
	f.PathResolved = "/opt/homebrew/Cellar/ccvault/0.4.0/bin/ccvault"

	r := Assemble(f)

	if !r.ProgramExists {
		t.Error("ProgramExists = false for a path that resolves")
	}
	if r.PathMatches {
		t.Error("PathMatches = true for two different binaries")
	}
	detail, ok := r.Problem(ProblemBinaryMismatch)
	if !ok {
		t.Fatalf("no %s problem reported; problems = %+v", ProblemBinaryMismatch, r.Problems)
	}
	for _, want := range []string{"1.25.5", "/opt/homebrew/bin/ccvault", "schedule install"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail = %q, want it to contain %q", detail, want)
		}
	}
}

// A Homebrew install puts a symlink on PATH and the Cellar path in the unit.
// Comparing the raw strings would call that a mismatch forever; comparing
// resolved paths is what makes the check mean "the same binary".
func TestAssemble_SymlinkedPathAndUnitPathAreTheSameBinary(t *testing.T) {
	f := healthyFacts()
	f.Registration.Program = "/opt/homebrew/Cellar/ccvault/0.4.0/bin/ccvault"
	f.ProgramResolved = "/opt/homebrew/Cellar/ccvault/0.4.0/bin/ccvault"
	f.PathProgram = "/opt/homebrew/bin/ccvault"
	f.PathResolved = "/opt/homebrew/Cellar/ccvault/0.4.0/bin/ccvault"

	r := Assemble(f)

	if !r.PathMatches {
		t.Errorf("PathMatches = false for a symlink and its target; problems = %+v", r.Problems)
	}
	if !r.Healthy() {
		t.Errorf("Healthy() = false; problems = %+v", r.Problems)
	}
}

// "Is the scheduled binary current?" is unanswerable with no ccvault on PATH.
// An unanswerable fact must not render as a passing one.
func TestAssemble_DetectsNoCcvaultOnPath(t *testing.T) {
	f := healthyFacts()
	f.PathProgram = ""
	f.PathResolved = ""

	r := Assemble(f)

	if r.PathMatches {
		t.Error("PathMatches = true with nothing on PATH to match")
	}
	if _, ok := r.Problem(ProblemNoPathBinary); !ok {
		t.Fatalf("no %s problem reported; problems = %+v", ProblemNoPathBinary, r.Problems)
	}
	// It belongs in `schedule status`, which asked. It says nothing about the
	// archive, so it must not nag from `stats`.
	if n := r.Notice(); n != "" {
		t.Errorf("Notice() = %q, want empty — a bare PATH gap is not a stats notice", n)
	}
}

// Failure 4: the last scheduled run failed.
func TestAssemble_DetectsFailedLastRun(t *testing.T) {
	f := healthyFacts()
	f.Registration.LastExitCode = 1
	f.Registration.LastExitKnown = true

	r := Assemble(f)

	if r.LastRunSucceeded {
		t.Error("LastRunSucceeded = true for exit code 1")
	}
	detail, ok := r.Problem(ProblemLastRunFailed)
	if !ok {
		t.Fatalf("no %s problem reported; problems = %+v", ProblemLastRunFailed, r.Problems)
	}
	if !strings.Contains(detail, "exited 1") {
		t.Errorf("detail = %q, want it to name the exit code", detail)
	}
	if !strings.Contains(detail, "/Users/d/.ccvault/schedule.log") {
		t.Errorf("detail = %q, want it to name the log that explains why", detail)
	}
}

// A job installed a minute ago has not exited. That is the right answer, not
// a failure — and it must not be rendered as exit code 0 either.
func TestAssemble_FreshInstallThatHasNotRunYetIsNotAFailure(t *testing.T) {
	f := healthyFacts()
	f.Registration.LastExitKnown = false
	f.Registration.LastExitCode = 0
	f.Registration.Runs = 0
	f.Registration.LastRunKnown = false
	f.Registration.LastRun = time.Time{}

	r := Assemble(f)

	if _, ok := r.Problem(ProblemLastRunFailed); ok {
		t.Error("a job that has never exited was reported as a failed run")
	}
	if r.LastRunSucceeded {
		t.Error("LastRunSucceeded = true for a job that has never run; unknown must not read as success")
	}
	if !r.Healthy() {
		t.Errorf("Healthy() = false for a fresh install; problems = %+v", r.Problems)
	}
}

// Failure 5: the archive is stale. Measured from source_files.synced_at, not
// from the timer — a timer firing hourly into a binary that refuses to write
// looks perfect from the scheduler's side.
func TestAssemble_DetectsStaleArchive(t *testing.T) {
	f := healthyFacts()
	f.LastSynced = testNow.Add(-72 * time.Hour)

	r := Assemble(f)

	if !r.Stale {
		t.Error("Stale = false for an archive 72h behind")
	}
	detail, ok := r.Problem(ProblemArchiveStale)
	if !ok {
		t.Fatalf("no %s problem reported; problems = %+v", ProblemArchiveStale, r.Problems)
	}
	if !strings.Contains(detail, "3 days") {
		t.Errorf("detail = %q, want the age in days", detail)
	}
	// This one the user can act on, so it is exactly what `stats` says.
	if n := r.Notice(); !strings.Contains(n, "3 days") {
		t.Errorf("Notice() = %q, want it to carry the staleness", n)
	}
}

// A timer that fires on the hour but a laptop shut overnight must not produce
// a warning that means nothing. StaleAfter is a day for that reason.
func TestAssemble_OvernightGapIsNotStale(t *testing.T) {
	f := healthyFacts()
	f.LastSynced = testNow.Add(-14 * time.Hour)

	r := Assemble(f)

	if r.Stale {
		t.Error("Stale = true for a 14h gap; an hourly job on a laptop shut overnight is normal")
	}
	if !r.Healthy() {
		t.Errorf("Healthy() = false; problems = %+v", r.Problems)
	}
}

func TestAssemble_StalenessBoundaryIsStaleAfter(t *testing.T) {
	cases := []struct {
		age   time.Duration
		stale bool
	}{
		{StaleAfter - time.Second, false},
		{StaleAfter, false},
		{StaleAfter + time.Second, true},
	}
	for _, c := range cases {
		f := healthyFacts()
		f.LastSynced = testNow.Add(-c.age)
		if got := Assemble(f).Stale; got != c.stale {
			t.Errorf("age %s: Stale = %v, want %v", c.age, got, c.stale)
		}
	}
}

func TestAssemble_NeverSyncedArchiveIsStale(t *testing.T) {
	f := healthyFacts()
	f.LastSynced = time.Time{}

	r := Assemble(f)

	if !r.Stale {
		t.Error("Stale = false for an archive that has never been synced")
	}
	detail, ok := r.Problem(ProblemArchiveStale)
	if !ok {
		t.Fatalf("no %s problem reported; problems = %+v", ProblemArchiveStale, r.Problems)
	}
	if !strings.Contains(detail, "never been synced") {
		t.Errorf("detail = %q, want it to say the archive has never been synced rather than report a 2026-year age", detail)
	}
	// The bug this guards: time.Time{} minus Now is ~2026 years, and
	// rendering that as a staleness is worse than saying nothing.
	if r.Staleness != 0 {
		t.Errorf("Staleness = %s, want 0 rather than the age of the zero time", r.Staleness)
	}
}

// An unsupported platform reports that and stops. Guessing at the other three
// facts is the failure this feature exists to avoid.
func TestAssemble_UnsupportedPlatformReportsOnlyThat(t *testing.T) {
	f := healthyFacts()
	f.Supported = false
	f.Platform = "windows"

	r := Assemble(f)

	if len(r.Problems) != 1 {
		t.Fatalf("Problems = %+v, want exactly the unsupported-platform problem", r.Problems)
	}
	detail, _ := r.Problem(ProblemUnsupported)
	if !strings.Contains(detail, "windows") {
		t.Errorf("detail = %q, want it to name the platform", detail)
	}
	if r.Registered || r.ProgramExists || r.PathMatches || r.LastRunSucceeded {
		t.Error("an unsupported platform asserted facts it cannot establish")
	}
	// Nothing the user can do about it, so it must not nag on every stats run.
	if n := r.Notice(); n != "" {
		t.Errorf("Notice() = %q, want empty on an unsupported platform", n)
	}
}

// The OS can report a job registered and decline to say what it runs. That is
// not a pass.
func TestAssemble_DetectsRegistrationWithNoProgram(t *testing.T) {
	f := healthyFacts()
	f.Registration.Program = ""
	f.ProgramResolved = ""

	r := Assemble(f)

	detail, ok := r.Problem(ProblemBinaryMissing)
	if !ok {
		t.Fatalf("no %s problem reported; problems = %+v", ProblemBinaryMissing, r.Problems)
	}
	if !strings.Contains(detail, "will not say") {
		t.Errorf("detail = %q, want it to distinguish 'the OS would not say' from 'the file is gone'", detail)
	}
}

func TestReportNotice_ListsEveryActionableProblem(t *testing.T) {
	f := healthyFacts()
	f.Registration = Registration{}
	f.LastSynced = testNow.Add(-30 * 24 * time.Hour)

	notice := Assemble(f).Notice()

	if !strings.Contains(notice, "schedule install") {
		t.Errorf("Notice() = %q, want the not-registered line", notice)
	}
	if !strings.Contains(notice, "30 days") {
		t.Errorf("Notice() = %q, want the staleness line", notice)
	}
	if !strings.Contains(notice, "notify = false") {
		t.Errorf("Notice() = %q, want it to name how to turn itself off", notice)
	}
}

func TestRoundDuration(t *testing.T) {
	cases := map[time.Duration]string{
		30 * time.Second:              "30s",
		90 * time.Second:              "2m0s",
		3 * time.Hour:                 "3h0m0s",
		47*time.Hour + 29*time.Minute: "47h0m0s",
		72 * time.Hour:                "3 days",
		30 * 24 * time.Hour:           "30 days",
	}
	for in, want := range cases {
		if got := roundDuration(in); got != want {
			t.Errorf("roundDuration(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestLogHint(t *testing.T) {
	if got := logHint(""); got != "" {
		t.Errorf("logHint(\"\") = %q, want empty", got)
	}
	if got, want := logHint("/tmp/x.log"), " — see /tmp/x.log"; got != want {
		t.Errorf("logHint() = %q, want %q", got, want)
	}
}
