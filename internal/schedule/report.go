// ABOUTME: The four facts `ccvault schedule status` reports, and the pure function that turns observations into a verdict
// ABOUTME: Nothing here touches the OS — Facts is gathered by the platform manager, Assemble only judges it

package schedule

import (
	"fmt"
	"strings"
	"time"
)

// Registration is what the OS says it currently holds for a label.
//
// A zero Registration means "the OS has nothing under this label", which is a
// fact and not an error. Every field beyond Found is whatever the OS was
// willing to say; the paired *Known booleans exist because "the job has never
// run" and "the OS would not tell me whether it has run" are different
// answers and must not both render as a zero.
type Registration struct {
	// Found is whether the OS has a job registered under this label.
	Found bool

	// UnitPath is the file the OS names as the job's definition: the plist
	// path under launchd, the timer unit path under systemd.
	UnitPath string

	// Program is the program path the OS has registered — read back from the
	// OS rather than from the file ccvault wrote, because the two can
	// disagree and only the first one runs.
	Program string

	// Arguments is the registered argument vector, program included.
	Arguments []string

	// LogPath is where the OS says the job's output goes, when it says.
	// Read back rather than assumed, for the same reason Program is: the
	// registered value is the one that is actually in effect.
	LogPath string

	// Interval is the period the OS has registered, when it says.
	Interval time.Duration

	// Runs is how many times the job has run, when the OS counts.
	Runs      int
	RunsKnown bool

	// LastExitCode is the job's exit status on its most recent run.
	// LastExitKnown is false when it has never exited — a fresh install, or a
	// run still in flight.
	LastExitCode  int
	LastExitKnown bool

	// LastRun is when the job most recently ran, when that is discoverable.
	// Under launchd it comes from the mtime of the log the job appends to;
	// under systemd from the service's own exit timestamp.
	LastRun      time.Time
	LastRunKnown bool
}

// Facts is everything `schedule status` observed, before any judgement.
//
// Gathered by the platform manager and the caller; consumed by Assemble. The
// split is what makes every verdict in Report testable without a scheduler,
// a binary on disk or an archive.
type Facts struct {
	// Platform names the mechanism ("launchd", "systemd"), and Supported is
	// whether ccvault can schedule here at all.
	Platform  string
	Supported bool

	// Label is the job ccvault looked for.
	Label string

	// Registration is the OS's answer. Fact 1.
	Registration Registration

	// ProgramResolved is Registration.Program with symlinks resolved, or ""
	// when the path does not exist. Fact 2's first half.
	ProgramResolved string

	// PathProgram is the `ccvault` found on PATH, and PathResolved is that
	// path with symlinks resolved. Both "" when PATH holds no ccvault.
	// Fact 2's second half — resolved on both sides because a Homebrew
	// install puts a symlink on PATH and the Cellar path in the unit.
	PathProgram  string
	PathResolved string

	// LastSynced is the newest source_files.synced_at in the archive, zero
	// when the archive has never been synced. Fact 4.
	LastSynced time.Time

	// LogPath is where the job's output is captured, for the user to read.
	LogPath string

	// Now is the instant staleness is measured against. Injected so the
	// staleness verdict is a pure function of its inputs.
	Now time.Time
}

// Problem is one thing `schedule status` found wrong.
//
// Coded as well as described so the `stats` notice, the JSON output and the
// exit status all agree about what counts as a problem rather than each
// re-deciding from prose.
type Problem struct {
	Code   string
	Detail string
}

// Problem codes. Stable strings — `stats --json` consumers key off them.
const (
	ProblemUnsupported    = "unsupported-platform"
	ProblemNotRegistered  = "not-registered"
	ProblemBinaryMissing  = "binary-missing"
	ProblemBinaryMismatch = "binary-mismatch"
	ProblemNoPathBinary   = "no-ccvault-on-path"
	ProblemLastRunFailed  = "last-run-failed"
	ProblemArchiveStale   = "archive-stale"
)

// Report is the verdict: the four facts, plus what is wrong with them.
type Report struct {
	Facts

	// Fact 1.
	Registered bool

	// Fact 2.
	ProgramExists bool
	PathMatches   bool

	// Fact 3.
	LastRunSucceeded bool

	// Fact 4.
	Staleness time.Duration
	Stale     bool

	Problems []Problem
}

// Healthy reports whether a scheduled sync is registered and nothing about it
// is wrong.
func (r Report) Healthy() bool { return len(r.Problems) == 0 }

// Problem returns the detail for a code, and whether the report holds it.
func (r Report) Problem(code string) (string, bool) {
	for _, p := range r.Problems {
		if p.Code == code {
			return p.Detail, true
		}
	}
	return "", false
}

// Assemble turns observations into a verdict.
//
// Pure: same Facts in, same Report out, no clock and no filesystem. That is
// the whole reason Facts exists as a type — every failure mode `status` claims
// to detect is a test case here, on any platform, rather than a state someone
// has to reproduce on a Mac.
//
// The order problems are appended in is the order they are printed, worst
// first: a job that is not registered makes everything below it moot, and a
// job pointed at a binary that no longer exists has not been running
// regardless of what its last exit code says.
func Assemble(f Facts) Report {
	r := Report{Facts: f}

	if !f.Supported {
		r.Problems = append(r.Problems, Problem{
			Code:   ProblemUnsupported,
			Detail: fmt.Sprintf("ccvault cannot register a scheduled sync on %s yet", f.Platform),
		})
		// Nothing below can be established on a platform with no mechanism,
		// and guessing would be the failure this feature exists to avoid.
		return r
	}

	// Fact 1: is a unit actually registered with the OS.
	r.Registered = f.Registration.Found
	if !r.Registered {
		r.Problems = append(r.Problems, Problem{
			Code:   ProblemNotRegistered,
			Detail: fmt.Sprintf("no %s job is registered under %q; run `ccvault schedule install`", f.Platform, f.Label),
		})
	}

	// Fact 2: does the registered binary still exist, and still match PATH.
	//
	// Only meaningful for a job that exists. Asking whether a non-existent
	// job's binary is current is how a status command ends up with two
	// complaints about one condition.
	if r.Registered {
		r.ProgramExists = f.ProgramResolved != ""
		switch {
		case f.Registration.Program == "":
			r.Problems = append(r.Problems, Problem{
				Code:   ProblemBinaryMissing,
				Detail: "the OS reports the job registered but will not say what program it runs",
			})
		case !r.ProgramExists:
			r.Problems = append(r.Problems, Problem{
				Code: ProblemBinaryMissing,
				Detail: fmt.Sprintf(
					"the registered binary %s no longer exists, so every scheduled run fails; run `ccvault schedule install` to re-point it",
					f.Registration.Program),
			})
		}

		if r.ProgramExists {
			switch f.PathResolved {
			case "":
				// Not a defect in the schedule, but it means ccvault cannot
				// say whether the scheduled binary is the current one — and
				// an unanswerable fact must not read as a passing one.
				r.Problems = append(r.Problems, Problem{
					Code:   ProblemNoPathBinary,
					Detail: "no ccvault on PATH, so the scheduled binary cannot be compared against the one you run",
				})
			case f.ProgramResolved:
				r.PathMatches = true
			default:
				r.Problems = append(r.Problems, Problem{
					Code: ProblemBinaryMismatch,
					Detail: fmt.Sprintf(
						"the scheduled binary (%s) is not the ccvault on your PATH (%s); the schedule is running an older build. Run `ccvault schedule install` to re-point it",
						f.Registration.Program, f.PathProgram),
				})
			}
		}
	}

	// Fact 3: when did it last run, and did it succeed.
	//
	// LastExitKnown false is "it has not exited yet", which for a fresh
	// install is the correct answer and not a problem.
	if r.Registered && f.Registration.LastExitKnown {
		r.LastRunSucceeded = f.Registration.LastExitCode == 0
		if !r.LastRunSucceeded {
			r.Problems = append(r.Problems, Problem{
				Code: ProblemLastRunFailed,
				Detail: fmt.Sprintf("the last scheduled run exited %d%s",
					f.Registration.LastExitCode, logHint(f.LogPath)),
			})
		}
	}

	// Fact 4: how stale is the archive.
	//
	// Measured from source_files.synced_at and not from the timer, because
	// the archive advancing is the outcome the schedule exists to produce.
	// A timer that fires hourly into a binary that refuses to write (#105)
	// looks perfect from the scheduler's side and is exactly the state this
	// is here to catch.
	switch {
	case f.LastSynced.IsZero():
		r.Staleness = 0
		r.Stale = true
		r.Problems = append(r.Problems, Problem{
			Code:   ProblemArchiveStale,
			Detail: "the archive has never been synced; run `ccvault sync`",
		})
	default:
		r.Staleness = f.Now.Sub(f.LastSynced)
		if r.Staleness > StaleAfter {
			r.Stale = true
			r.Problems = append(r.Problems, Problem{
				Code: ProblemArchiveStale,
				Detail: fmt.Sprintf("the archive last advanced %s ago (%s); anything Claude Code has pruned since is gone",
					roundDuration(r.Staleness), f.LastSynced.Local().Format("2006-01-02 15:04")),
			})
		}
	}

	return r
}

// logHint names the log file in a message, when there is one to name.
func logHint(logPath string) string {
	if logPath == "" {
		return ""
	}
	return fmt.Sprintf(" — see %s", logPath)
}

// roundDuration renders an age the way a person reads one. Exact
// nanoseconds in "the archive last advanced 31h17m4.221s ago" is precision
// nobody asked for and makes the number harder to take in.
func roundDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return d.Round(time.Second).String()
	case d < time.Hour:
		return d.Round(time.Minute).String()
	case d < 48*time.Hour:
		return d.Round(time.Hour).String()
	default:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
}

// Notice is the one-line staleness warning `ccvault stats` prints, or "" when
// there is nothing worth saying.
//
// Silent when clean is the whole contract — it follows #106 (`--strict`) and
// #155 (`repair-tokens`): say nothing when the archive is fine, and name the
// command that fixes it when it is not. A notice that printed on every run
// would be trained away inside a week.
func (r Report) Notice() string {
	if r.Healthy() {
		return ""
	}
	// An unsupported platform is not something the user can act on, so it is
	// not worth a line on every stats run.
	if _, ok := r.Problem(ProblemUnsupported); ok {
		return ""
	}
	// Likewise: "no ccvault on PATH" alone says nothing about the archive.
	// It belongs in `schedule status`, which was asked for it.
	actionable := make([]Problem, 0, len(r.Problems))
	for _, p := range r.Problems {
		if p.Code == ProblemNoPathBinary {
			continue
		}
		actionable = append(actionable, p)
	}
	if len(actionable) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("Scheduled sync:\n")
	for _, p := range actionable {
		fmt.Fprintf(&b, "  - %s\n", p.Detail)
	}
	b.WriteString("  Run `ccvault schedule status` for detail, or set notify = false under [schedule] in your\n")
	b.WriteString("  config file to stop this notice.\n")
	return b.String()
}
