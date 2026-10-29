// ABOUTME: The systemd --user backend — writes the .service/.timer pair, enables the timer, and reads back what systemd holds
// ABOUTME: Untagged on purpose: only platform_linux.go selects it, so this logic is tested on macOS too

package schedule

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// systemdManager registers a user timer under `systemctl --user`.
//
// User scope only, never `systemctl --system`: a system timer needs root and
// would run the sync as root, writing an archive the user cannot read. #171
// settled on user-level units.
//
// The cost of user scope is that it needs a user session bus. A machine with
// no logged-in session — the usual shape of a CI runner — has no
// DBUS_SESSION_BUS_ADDRESS and every `systemctl --user` call fails. That is
// reported as an error rather than as "no job registered", because the two
// want completely different responses from the user.
type systemdManager struct {
	// unitsDir is ~/.config/systemd/user. Injectable so tests write
	// somewhere disposable.
	unitsDir string

	run commandRunner
}

// newSystemdManager builds the manager for the calling user.
func newSystemdManager() (*systemdManager, error) {
	dir, err := systemdUserUnitDir()
	if err != nil {
		return nil, err
	}
	return &systemdManager{unitsDir: dir, run: execRunner{}}, nil
}

// systemdUserUnitDir is where a user's own unit files belong, honouring
// XDG_CONFIG_HOME when it is set.
func systemdUserUnitDir() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "systemd", "user"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory: %w", err)
	}
	return filepath.Join(home, ".config", "systemd", "user"), nil
}

func (m *systemdManager) Platform() string { return "systemd" }

// Install writes both units, reloads, and enables the timer.
//
// `enable --now` rather than `enable` then `start`: without --now the timer
// is registered for the next login and does nothing until then, which is a
// job that reports installed and does not run.
//
// Ends by re-inspecting, so a unit systemd accepted and then declined to
// load cannot be reported as a success.
func (m *systemdManager) Install(j Job) error {
	if err := j.Validate(); err != nil {
		return err
	}

	service, err := RenderSystemdService(j)
	if err != nil {
		return err
	}
	timer, err := RenderSystemdTimer(j)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.unitsDir, 0o750); err != nil {
		return fmt.Errorf("create %s: %w", m.unitsDir, err)
	}
	servicePath := filepath.Join(m.unitsDir, SystemdServiceName(j.Label))
	timerPath := filepath.Join(m.unitsDir, SystemdTimerName(j.Label))
	if err := writeFileAtomic(servicePath, []byte(service), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", servicePath, err)
	}
	if err := writeFileAtomic(timerPath, []byte(timer), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", timerPath, err)
	}

	if err := m.systemctl("daemon-reload"); err != nil {
		return err
	}
	if err := m.systemctl("enable", "--now", SystemdTimerName(j.Label)); err != nil {
		return err
	}

	reg, err := m.Inspect(j.Label)
	if err != nil {
		return fmt.Errorf("verify the registration systemd just accepted: %w", err)
	}
	if !reg.Found {
		return fmt.Errorf("systemctl --user enable reported success but %s is not active", SystemdTimerName(j.Label))
	}
	return nil
}

// Uninstall disables the timer and deletes both units.
//
// Unit files are removed as well as disabled: a .timer left in
// ~/.config/systemd/user is one `systemctl --user enable` away from being
// back, and `status` would keep finding its FragmentPath.
func (m *systemdManager) Uninstall(label string) error {
	if err := validateLabel(label); err != nil {
		return err
	}
	servicePath := filepath.Join(m.unitsDir, SystemdServiceName(label))
	timerPath := filepath.Join(m.unitsDir, SystemdTimerName(label))

	reg, err := m.Inspect(label)
	if err != nil {
		return err
	}
	filesPresent := false
	for _, p := range []string{servicePath, timerPath} {
		if _, statErr := os.Stat(p); statErr == nil {
			filesPresent = true
		}
	}
	if !reg.Found && !filesPresent {
		return ErrNotRegistered
	}

	if reg.Found {
		if err := m.systemctl("disable", "--now", SystemdTimerName(label)); err != nil {
			return err
		}
	}
	for _, p := range []string{timerPath, servicePath} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", p, err)
		}
	}
	if err := m.systemctl("daemon-reload"); err != nil {
		return err
	}

	after, err := m.Inspect(label)
	if err != nil {
		return fmt.Errorf("verify the job was removed: %w", err)
	}
	if after.Found {
		return fmt.Errorf("%s is still active after disable", SystemdTimerName(label))
	}
	return nil
}

// Inspect reads the timer's load state and the service's last exit, which
// together are facts 1 through 3.
//
// Two `systemctl show` calls rather than one: the timer says whether anything
// is registered, and the service says what it runs and how its last run
// ended. Asking the timer for ExecStart returns nothing, which would read as
// "the OS will not say what program it runs".
func (m *systemdManager) Inspect(label string) (Registration, error) {
	if err := validateLabel(label); err != nil {
		return Registration{}, err
	}

	timerOut, err := m.show(SystemdTimerName(label),
		"LoadState", "ActiveState", "FragmentPath")
	if err != nil {
		return Registration{}, err
	}
	timerProps := parseSystemctlShow(timerOut)

	// LoadState=not-found is systemd saying nothing is registered under this
	// name. That is a fact, not a failure.
	if timerProps["LoadState"] != "loaded" {
		return Registration{}, nil
	}

	var reg Registration
	reg.Found = timerProps["ActiveState"] == "active"
	reg.UnitPath = timerProps["FragmentPath"]
	if !reg.Found {
		// Loaded but inactive is a timer that will not fire. Reporting it as
		// registered would make `status` green for a job that never runs.
		return Registration{}, nil
	}

	serviceOut, err := m.show(SystemdServiceName(label),
		"ExecStart", "ExecMainStatus", "ExecMainExitTimestamp", "NRestarts", "StandardOutput")
	if err != nil {
		return Registration{}, err
	}
	svc := parseSystemctlShow(serviceOut)

	reg.Arguments = parseSystemdExecStartArgv(svc["ExecStart"])
	if len(reg.Arguments) > 0 {
		reg.Program = reg.Arguments[0]
	}
	reg.LogPath = parseSystemdAppendTarget(svc["StandardOutput"])

	// ExecMainStatus is "0" for a service that has never run as well as for
	// one that succeeded, so the timestamp is what distinguishes them. No
	// timestamp means it has not run, and that must not read as success.
	if ts, ok := parseSystemdTimestamp(svc["ExecMainExitTimestamp"]); ok {
		reg.LastRun = ts
		reg.LastRunKnown = true
		if code, err := strconv.Atoi(strings.TrimSpace(svc["ExecMainStatus"])); err == nil {
			reg.LastExitCode = code
			reg.LastExitKnown = true
		}
	}

	// Registration.Interval is left zero on purpose. `systemctl show` reports
	// the timer's next elapse, not its configured period, and deriving the
	// period from the next elapse would be a guess rendered as a fact. The
	// field's zero value means "the OS did not say", which is the truth.
	return reg, nil
}

// systemctl runs a --user subcommand and turns a refusal into an error.
func (m *systemdManager) systemctl(args ...string) error {
	full := append([]string{"--user"}, args...)
	out, code, err := m.run.Run("systemctl", full...)
	if err != nil {
		return fmt.Errorf("run systemctl --user %s: %w", strings.Join(args, " "), err)
	}
	if code != 0 {
		return fmt.Errorf("systemctl --user %s exited %d: %s", strings.Join(args, " "), code, strings.TrimSpace(out))
	}
	return nil
}

// show runs `systemctl --user show` for the named properties.
//
// `show` exits 0 and reports LoadState=not-found for a unit that does not
// exist, so a non-zero exit here is a real problem — most often no session
// bus — and is returned as one.
func (m *systemdManager) show(unit string, properties ...string) (string, error) {
	args := []string{"--user", "show", unit}
	for _, p := range properties {
		args = append(args, "--property="+p)
	}
	out, code, err := m.run.Run("systemctl", args...)
	if err != nil {
		return "", fmt.Errorf("run systemctl --user show %s: %w", unit, err)
	}
	if code != 0 {
		if isSystemdNoSessionBus(out) {
			return "", fmt.Errorf(
				"systemctl --user cannot reach a session bus, so ccvault cannot tell whether a scheduled sync is registered: %s",
				strings.TrimSpace(out))
		}
		return "", fmt.Errorf("systemctl --user show %s exited %d: %s", unit, code, strings.TrimSpace(out))
	}
	return out, nil
}

// isSystemdNoSessionBus recognises the failure every headless machine
// produces, so it can be reported as itself rather than as "no job".
func isSystemdNoSessionBus(out string) bool {
	lower := strings.ToLower(out)
	return strings.Contains(lower, "failed to connect to bus") ||
		strings.Contains(lower, "no medium found") ||
		strings.Contains(lower, "dbus_session_bus_address")
}

// parseSystemctlShow reads `systemctl show`'s KEY=VALUE lines.
//
// Pure. A value may itself contain '=', so only the first one splits.
func parseSystemctlShow(out string) map[string]string {
	props := make(map[string]string)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found || key == "" {
			continue
		}
		props[key] = value
	}
	return props
}

// parseSystemdExecStartArgv pulls the argument vector out of ExecStart's
// structured rendering:
//
//	{ path=/usr/bin/ccvault ; argv[]=/usr/bin/ccvault sync ; ignore_errors=no ; ... }
//
// Pure, and worth parsing rather than reading the unit file back: the unit on
// disk and the unit systemd loaded can differ by a daemon-reload, and only
// the loaded one runs.
func parseSystemdExecStartArgv(value string) []string {
	const marker = "argv[]="
	i := strings.Index(value, marker)
	if i < 0 {
		return nil
	}
	rest := value[i+len(marker):]
	// The field ends at the next " ; " separator, or at the closing brace.
	if j := strings.Index(rest, " ; "); j >= 0 {
		rest = rest[:j]
	}
	rest = strings.TrimSuffix(strings.TrimSpace(rest), "}")
	return strings.Fields(strings.TrimSpace(rest))
}

// parseSystemdAppendTarget pulls the path out of StandardOutput's
// "append:/path/to/file" form, or returns "" for journal and the rest.
func parseSystemdAppendTarget(value string) string {
	const prefix = "append:"
	if !strings.HasPrefix(value, prefix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(value, prefix))
}

// systemdTimestampLayouts are the renderings `systemctl show` uses for a
// timestamp property. The zone abbreviation is the machine's, so parsing is
// zone-naive and the result is interpreted in the local zone — which is
// where it came from.
var systemdTimestampLayouts = []string{
	"Mon 2006-01-02 15:04:05 MST",
	"Mon 2006-01-02 15:04:05 -0700",
	"Mon 2006-01-02 15:04:05",
}

// parseSystemdTimestamp reads an ExecMain*Timestamp value.
//
// An empty value means the service has never run, and is reported as
// not-known rather than as the zero time, so "never ran" and "ran at the
// epoch" stay distinguishable.
func parseSystemdTimestamp(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" || value == "n/a" {
		return time.Time{}, false
	}
	for _, layout := range systemdTimestampLayouts {
		if ts, err := time.ParseInLocation(layout, value, time.Local); err == nil {
			return ts, true
		}
	}
	return time.Time{}, false
}
