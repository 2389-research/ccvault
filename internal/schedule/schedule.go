// ABOUTME: The job ccvault registers with the host OS to run `ccvault sync` on a timer, and the rules it must satisfy
// ABOUTME: Platform-neutral types only — unit-file text lives in plist.go/systemdunit.go, registration in launchd.go/systemd.go

package schedule

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// DefaultInterval is how often the registered job runs.
//
// Hourly is not about margin — Claude Code keeps transcripts on disk for
// roughly 38 days, so a weekly job would still lose nothing. It is about
// `schedule status` being able to answer "did it run?" with something recent.
// At 15-40s an incremental sync the cost is negligible.
const DefaultInterval = time.Hour

// DefaultLabel identifies the job to the OS: the launchd label on macOS, the
// systemd unit stem on Linux.
//
// Reverse-DNS because launchd labels share one flat namespace per user, and
// stable across versions because it is the key `install` is idempotent on.
const DefaultLabel = "ai.2389.ccvault.sync"

// StaleAfter is how long the archive may go without advancing before
// `schedule status` and the `stats` notice call it stale.
//
// A day, against an hourly job, so the ordinary reasons a tick is missed — a
// laptop shut overnight, a machine asleep for an afternoon — do not produce a
// warning that means nothing. Against Claude Code's ~38-day retention it is
// still 37 days of room to notice and act.
const StaleAfter = 24 * time.Hour

// ErrUnsupportedPlatform is returned by every operation on a platform where
// ccvault does not know how to register a job.
//
// It is deliberately a refusal rather than a no-op that reports success. Three
// things in this repo have shipped reporting success while doing nothing
// (#160, #151, #170); a background job that silently never runs would be the
// worst of the four, because the whole point of it is that nobody is watching.
var ErrUnsupportedPlatform = errors.New("ccvault does not support scheduled sync on this platform yet")

// ErrNotRegistered is returned by Uninstall when there is no job to remove.
var ErrNotRegistered = errors.New("no ccvault scheduled sync is registered")

// Job is one registration: which binary to run, with which arguments, how
// often, under what name.
type Job struct {
	// Label identifies the job to the OS. See DefaultLabel.
	Label string

	// BinaryPath is the absolute path to the ccvault binary, resolved at
	// install time and baked into the unit file.
	//
	// There is no shim. A unit that invoked `ccvault` through a wrapper would
	// need the wrapper to outlive every reinstall, and the failure it would
	// protect against is install-route-specific: `go install` gives
	// ~/go/bin/ccvault and Homebrew /opt/homebrew/bin/ccvault, both stable,
	// while a GOBIN under mise embeds the Go version and moves when Go is
	// bumped. So the path is recorded as-is and `schedule status` is
	// responsible for noticing when it has gone stale.
	BinaryPath string

	// Args is the full argument vector the job runs, starting at "sync".
	//
	// Validate refuses --full and --rebuild here rather than trusting callers
	// not to pass them: --rebuild wipes the archive and --full re-parses it
	// whole, and both must stay human-initiated (#36). A scheduled job has no
	// audience for a confirmation prompt.
	Args []string

	// LogPath is where the job's output is captured, on platforms that need
	// to be told. Optional; empty means the platform's own default (the
	// journal, under systemd).
	LogPath string

	// Interval is how often the job runs. Whole seconds only — launchd's
	// StartInterval and systemd's OnUnitActiveSec are both integral.
	Interval time.Duration
}

// NewJob builds the standard hourly sync job for a binary path.
func NewJob(binaryPath, logPath string, extraArgs ...string) Job {
	return Job{
		Label:      DefaultLabel,
		BinaryPath: binaryPath,
		Args:       append([]string{"sync"}, extraArgs...),
		LogPath:    logPath,
		Interval:   DefaultInterval,
	}
}

// forbiddenArgs are the flags a scheduled job must never carry, in every
// spelling Go's flag package and cobra accept.
//
// Matched on the flag name rather than the whole argument so that
// --rebuild=true and --rebuild are both caught.
var forbiddenArgs = []string{"full", "rebuild"}

// Validate reports why this job cannot be registered, or nil.
func (j Job) Validate() error {
	if err := validateLabel(j.Label); err != nil {
		return err
	}
	if j.BinaryPath == "" {
		return errors.New("binary path is empty")
	}
	if !filepath.IsAbs(j.BinaryPath) {
		return fmt.Errorf("binary path %q is not absolute; a unit file cannot resolve a relative path", j.BinaryPath)
	}
	if len(j.Args) == 0 || j.Args[0] != "sync" {
		return fmt.Errorf("a scheduled job must run `sync`, got %v", j.Args)
	}
	for _, arg := range j.Args[1:] {
		if name, ok := flagName(arg); ok {
			for _, forbidden := range forbiddenArgs {
				if name == forbidden {
					return fmt.Errorf("refusing to schedule `sync --%s`: it must stay human-initiated", forbidden)
				}
			}
		}
	}
	if j.LogPath != "" && !filepath.IsAbs(j.LogPath) {
		return fmt.Errorf("log path %q is not absolute", j.LogPath)
	}
	if j.Interval <= 0 {
		return fmt.Errorf("interval %s is not positive", j.Interval)
	}
	if j.Interval%time.Second != 0 {
		return fmt.Errorf("interval %s is not a whole number of seconds", j.Interval)
	}
	return nil
}

// flagName extracts the flag name from an argument, or reports that the
// argument is not a flag. "--rebuild=true" and "-rebuild" both yield
// "rebuild".
func flagName(arg string) (string, bool) {
	if !strings.HasPrefix(arg, "-") {
		return "", false
	}
	name := strings.TrimLeft(arg, "-")
	if name == "" {
		return "", false
	}
	if i := strings.IndexByte(name, '='); i >= 0 {
		name = name[:i]
	}
	return name, true
}

// validateLabel rejects anything that is not safe in all three of a launchd
// label, a systemd unit name and a filename.
//
// Deliberately narrower than any one platform allows. The label becomes a
// path component (~/Library/LaunchAgents/<label>.plist), so a separator or a
// traversal component in it would write a file somewhere nobody asked for.
func validateLabel(label string) error {
	if label == "" {
		return errors.New("label is empty")
	}
	for _, r := range label {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '-', r == '_':
		default:
			return fmt.Errorf("label %q contains %q; only letters, digits, '.', '-' and '_' are allowed", label, r)
		}
	}
	if label == "." || label == ".." {
		return fmt.Errorf("label %q is a path component, not a name", label)
	}
	return nil
}
