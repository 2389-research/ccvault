// ABOUTME: The one seam between the platform backends and the real launchctl/systemctl binaries
// ABOUTME: An interface rather than a direct exec call so install/uninstall/inspect logic is testable off-platform

package schedule

import (
	"errors"
	"os/exec"
)

// commandRunner runs a scheduler CLI and reports what it said.
//
// The seam exists because the interesting part of registration is not the
// exec — it is the order of operations, the argv, the parse of the output and
// the re-check afterwards, none of which need a real scheduler to be wrong.
// With this interface those are tested on any platform; without it they would
// only ever run on the host they target.
type commandRunner interface {
	// Run executes name with args and returns its combined output and exit
	// status. A non-zero exit is reported in code with a nil error: a command
	// that ran and refused is an answer, not a failure to ask. err is
	// non-nil only when the command could not be run at all.
	Run(name string, args ...string) (output string, code int, err error)
}

// execRunner is the real thing.
type execRunner struct{}

func (execRunner) Run(name string, args ...string) (string, int, error) {
	// #nosec G204 -- name is a package-level literal ("launchctl",
	// "systemctl") and every argument is either a literal or a path this
	// package built and Job.Validate has already constrained.
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return string(out), exitErr.ExitCode(), nil
		}
		return string(out), -1, err
	}
	return string(out), 0, nil
}
