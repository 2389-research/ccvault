// ABOUTME: The launchd backend — writes the LaunchAgent plist, bootstraps it, and reads back what launchd actually holds
// ABOUTME: Untagged on purpose: only platform_darwin.go selects it, so this logic is tested on Linux CI too

package schedule

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// launchdManager registers a LaunchAgent in the calling user's GUI domain.
//
// User-level only, never a daemon in /Library/LaunchDaemons: a daemon needs
// sudo, runs as root, and would archive conversations with the wrong
// ownership. #171 settled on user-level units for exactly that reason.
type launchdManager struct {
	// agentsDir is ~/Library/LaunchAgents. Injectable so tests write
	// somewhere disposable.
	agentsDir string

	// domain is the launchctl domain target, "gui/<uid>".
	domain string

	run commandRunner
}

// newLaunchdManager builds the manager for the calling user.
func newLaunchdManager(uid int) (*launchdManager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("find home directory: %w", err)
	}
	return &launchdManager{
		agentsDir: filepath.Join(home, "Library", "LaunchAgents"),
		domain:    fmt.Sprintf("gui/%d", uid),
		run:       execRunner{},
	}, nil
}

func (m *launchdManager) Platform() string { return "launchd" }

// plistPath is where this label's plist lives.
func (m *launchdManager) plistPath(label string) string {
	return filepath.Join(m.agentsDir, LaunchdPlistName(label))
}

// serviceTarget is the launchctl service target for this label.
func (m *launchdManager) serviceTarget(label string) string {
	return m.domain + "/" + label
}

// Install writes the plist and bootstraps it, replacing any existing
// registration under the same label.
//
// Idempotent by booting out first: `launchctl bootstrap` on an
// already-loaded label fails with "service already loaded", so a second
// install would otherwise report an error for a state that is exactly what
// was asked for.
//
// The last thing it does is re-inspect. A bootstrap that launchd accepted at
// the command line and then dropped would otherwise print a success message
// for a job that does not exist, which is the failure this whole feature is
// built around not repeating (#160, #151, #170).
func (m *launchdManager) Install(j Job) error {
	if err := j.Validate(); err != nil {
		return err
	}

	plist, err := RenderLaunchdPlist(j)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.agentsDir, 0o750); err != nil {
		return fmt.Errorf("create %s: %w", m.agentsDir, err)
	}
	path := m.plistPath(j.Label)
	if err := writeFileAtomic(path, []byte(plist), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	// Output ignored: "not loaded" here is the normal first-install case.
	_, _, _ = m.run.Run("launchctl", "bootout", m.serviceTarget(j.Label))

	out, code, err := m.run.Run("launchctl", "bootstrap", m.domain, path)
	if err != nil {
		return fmt.Errorf("run launchctl bootstrap: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("launchctl bootstrap %s exited %d: %s", path, code, strings.TrimSpace(out))
	}

	reg, err := m.Inspect(j.Label)
	if err != nil {
		return fmt.Errorf("verify the registration launchd just accepted: %w", err)
	}
	if !reg.Found {
		return fmt.Errorf("launchctl bootstrap reported success but %s is not registered in %s", j.Label, m.domain)
	}
	return nil
}

// Uninstall boots the job out and deletes its plist.
//
// Both halves, always: a plist left behind in ~/Library/LaunchAgents is
// re-bootstrapped at the next login, so removing only the registration
// uninstalls the job until the user reboots.
func (m *launchdManager) Uninstall(label string) error {
	if err := validateLabel(label); err != nil {
		return err
	}
	path := m.plistPath(label)

	reg, err := m.Inspect(label)
	if err != nil {
		return err
	}
	_, plistErr := os.Stat(path)
	plistPresent := plistErr == nil
	if !reg.Found && !plistPresent {
		return ErrNotRegistered
	}

	if reg.Found {
		out, code, err := m.run.Run("launchctl", "bootout", m.serviceTarget(label))
		if err != nil {
			return fmt.Errorf("run launchctl bootout: %w", err)
		}
		if code != 0 {
			return fmt.Errorf("launchctl bootout %s exited %d: %s", label, code, strings.TrimSpace(out))
		}
	}
	if plistPresent {
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove %s: %w", path, err)
		}
	}

	after, err := m.Inspect(label)
	if err != nil {
		return fmt.Errorf("verify the job was removed: %w", err)
	}
	if after.Found {
		return fmt.Errorf("%s is still registered in %s after bootout", label, m.domain)
	}
	return nil
}

// Inspect asks launchd what it holds, and reads the job's log mtime for the
// one fact launchd does not report: when it last ran.
func (m *launchdManager) Inspect(label string) (Registration, error) {
	if err := validateLabel(label); err != nil {
		return Registration{}, err
	}

	out, code, err := m.run.Run("launchctl", "print", m.serviceTarget(label))
	if err != nil {
		return Registration{}, fmt.Errorf("run launchctl print: %w", err)
	}
	if code != 0 {
		// 113 and the "Could not find service" text are launchd's way of
		// saying nothing is registered, which is a fact and not a failure.
		// Any other refusal is a real error and must not read as "no job".
		if isLaunchdNotFound(out) {
			return Registration{}, nil
		}
		return Registration{}, fmt.Errorf("launchctl print %s exited %d: %s", label, code, strings.TrimSpace(out))
	}

	reg := parseLaunchctlPrint(out)
	reg.Found = true
	if reg.LogPath != "" {
		if info, statErr := os.Stat(reg.LogPath); statErr == nil {
			// launchd keeps no last-run timestamp of its own. The job appends
			// its output to this file on every run, so the mtime is when it
			// last ran — and is zero-cost to read.
			reg.LastRun = info.ModTime()
			reg.LastRunKnown = true
		}
	}
	return reg, nil
}

// isLaunchdNotFound recognises launchd declining because nothing is
// registered, as opposed to declining for any other reason.
func isLaunchdNotFound(out string) bool {
	lower := strings.ToLower(out)
	return strings.Contains(lower, "could not find service") ||
		strings.Contains(lower, "no such process")
}

// parseLaunchctlPrint reads the fields `schedule status` needs out of
// `launchctl print`'s block format.
//
// Pure, and the reason the launchd backend is testable at all. The format is
// an indented key-value dump with nested braces; this reads the handful of
// keys that matter and ignores the rest, so a new line in some future macOS
// does not break it.
func parseLaunchctlPrint(out string) Registration {
	var reg Registration
	inArguments := false

	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)

		// arguments = { ... } is the one multi-line value that is read.
		if inArguments {
			if trimmed == "}" {
				inArguments = false
				continue
			}
			if trimmed != "" {
				reg.Arguments = append(reg.Arguments, trimmed)
			}
			continue
		}
		if trimmed == "arguments = {" {
			inArguments = true
			continue
		}

		key, value, ok := splitLaunchdField(trimmed)
		if !ok {
			continue
		}
		switch key {
		case "path":
			reg.UnitPath = value
		case "program":
			reg.Program = value
		case "stdout path":
			reg.LogPath = value
		case "run interval":
			// "3600 seconds"
			if secs, err := strconv.Atoi(strings.TrimSuffix(value, " seconds")); err == nil {
				reg.Interval = time.Duration(secs) * time.Second
			}
		case "runs":
			if n, err := strconv.Atoi(value); err == nil {
				reg.Runs = n
				reg.RunsKnown = true
			}
		case "last exit code":
			// Three shapes, observed on macOS 15:
			//
			//   last exit code = 0
			//   last exit code = 78: EX_CONFIG
			//   last exit code = (never exited)
			//
			// The annotated form is the one that matters and the one that was
			// nearly missed: a bare Atoi on "78: EX_CONFIG" fails, and a
			// failed run then read as "outcome not reported" — a real failure
			// rendered as an unanswerable question. Found by firing the job at
			// a deleted binary rather than by reading the man page.
			//
			// "(never exited)" stays unknown rather than becoming 0, because
			// 0 means the sync succeeded.
			if n, err := strconv.Atoi(leadingInt(value)); err == nil {
				reg.LastExitCode = n
				reg.LastExitKnown = true
			}
		}
	}

	// launchd reports `program` only when the plist sets Program or when it
	// infers one; fall back to argv[0], which is what it actually executes.
	if reg.Program == "" && len(reg.Arguments) > 0 {
		reg.Program = reg.Arguments[0]
	}
	return reg
}

// leadingInt returns the value up to the first ':', so launchd's annotated
// exit codes ("78: EX_CONFIG") parse as the number they lead with.
func leadingInt(value string) string {
	if i := strings.IndexByte(value, ':'); i >= 0 {
		return strings.TrimSpace(value[:i])
	}
	return value
}

// splitLaunchdField splits "key = value" on the first " = ".
//
// Not strings.Cut on "=" — a path or an environment value can contain one,
// and "OSLogRateLimit => 64" would otherwise parse as a key of
// "OSLogRateLimit " and a value of "> 64".
func splitLaunchdField(line string) (key, value string, ok bool) {
	i := strings.Index(line, " = ")
	if i < 0 {
		return "", "", false
	}
	key = strings.TrimSpace(line[:i])
	value = strings.TrimSpace(line[i+len(" = "):])
	if key == "" || value == "" {
		return "", "", false
	}
	return key, value, true
}

// writeFileAtomic writes to a sibling temp file and renames over the target.
//
// A plist half-written when the process dies is a plist launchd refuses to
// load, and it would sit there looking installed. Rename within the same
// directory is atomic, so the file either has the old content or the new.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		// No-op once the rename has succeeded.
		_ = os.Remove(tmpName)
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
