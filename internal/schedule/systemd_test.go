// ABOUTME: Tests the systemd backend's argv, order of operations, post-install verification and systemctl-show parsing
// ABOUTME: Against a fake systemctl — registration itself is UNVERIFIED on a real Linux box; see docs/schedule.md

package schedule

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestSystemd(t *testing.T, r commandRunner) *systemdManager {
	t.Helper()
	return &systemdManager{unitsDir: filepath.Join(t.TempDir(), "systemd", "user"), run: r}
}

// showTimer is `systemctl --user show <label>.timer` output for a live timer.
func showTimer(loadState, activeState string) string {
	return "LoadState=" + loadState + "\nActiveState=" + activeState +
		"\nFragmentPath=/home/d/.config/systemd/user/ccvault-sync.timer\n"
}

// showService is `systemctl --user show <label>.service` output. ExecStart's
// structured rendering is copied from systemd's documented form.
func showService(program, exitTimestamp, exitStatus, stdout string) string {
	return "ExecStart={ path=" + program + " ; argv[]=" + program +
		" sync ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }\n" +
		"ExecMainStatus=" + exitStatus + "\n" +
		"ExecMainExitTimestamp=" + exitTimestamp + "\n" +
		"NRestarts=0\n" +
		"StandardOutput=" + stdout + "\n"
}

func TestParseSystemctlShow_ReadsKeyValueLines(t *testing.T) {
	props := parseSystemctlShow("LoadState=loaded\nActiveState=active\nFragmentPath=/x/y.timer\n")
	for key, want := range map[string]string{
		"LoadState":    "loaded",
		"ActiveState":  "active",
		"FragmentPath": "/x/y.timer",
	} {
		if got := props[key]; got != want {
			t.Errorf("props[%q] = %q, want %q", key, got, want)
		}
	}
}

// A value can hold '=' — ExecStart's own rendering does, and so does any
// environment assignment. Only the first '=' separates.
func TestParseSystemctlShow_KeepsEqualsSignsInsideValues(t *testing.T) {
	props := parseSystemctlShow("ExecStart={ path=/bin/ccvault ; argv[]=/bin/ccvault sync }\n")
	want := "{ path=/bin/ccvault ; argv[]=/bin/ccvault sync }"
	if got := props["ExecStart"]; got != want {
		t.Errorf("props[ExecStart] = %q, want %q", got, want)
	}
}

func TestParseSystemdExecStartArgv(t *testing.T) {
	cases := map[string]string{
		"{ path=/bin/ccvault ; argv[]=/bin/ccvault sync ; ignore_errors=no }": "/bin/ccvault sync",
		"{ path=/bin/ccvault ; argv[]=/bin/ccvault sync }":                    "/bin/ccvault sync",
		"{ argv[]=/bin/ccvault sync --source claude-code ; pid=0 }":           "/bin/ccvault sync --source claude-code",
		"":                      "",
		"{ path=/bin/ccvault }": "",
	}
	for in, want := range cases {
		got := strings.Join(parseSystemdExecStartArgv(in), " ")
		if got != want {
			t.Errorf("parseSystemdExecStartArgv(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseSystemdAppendTarget(t *testing.T) {
	cases := map[string]string{
		"append:/home/d/.ccvault/schedule.log": "/home/d/.ccvault/schedule.log",
		"journal":                              "",
		"inherit":                              "",
		"":                                     "",
	}
	for in, want := range cases {
		if got := parseSystemdAppendTarget(in); got != want {
			t.Errorf("parseSystemdAppendTarget(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseSystemdTimestamp(t *testing.T) {
	ts, ok := parseSystemdTimestamp("Wed 2026-10-28 09:15:00 UTC")
	if !ok {
		t.Fatal("parseSystemdTimestamp() refused a value systemctl emits")
	}
	if ts.Year() != 2026 || ts.Month() != time.October || ts.Day() != 28 {
		t.Errorf("parsed %s, want 2026-10-28", ts)
	}
}

// Empty and "n/a" are both "the service has never run". They must report
// not-known rather than the zero time, so Assemble can tell "never ran" from
// "ran at the epoch".
func TestParseSystemdTimestamp_NeverRanIsNotKnown(t *testing.T) {
	for _, value := range []string{"", "n/a", "  ", "nonsense"} {
		ts, ok := parseSystemdTimestamp(value)
		if ok {
			t.Errorf("parseSystemdTimestamp(%q) reported a known timestamp %s", value, ts)
		}
	}
}

// Fact 1, the not-registered case: LoadState=not-found is systemd saying
// nothing is there, which is an answer and not an error.
func TestSystemdInspect_NotFoundIsNotAnError(t *testing.T) {
	m := newTestSystemd(t, &fakeRunner{reply: map[string]fakeReply{
		"": {out: showTimer("not-found", "inactive")},
	}})

	reg, err := m.Inspect("ccvault-sync")
	if err != nil {
		t.Fatalf("Inspect() error = %v, want nil", err)
	}
	if reg.Found {
		t.Error("Found = true for LoadState=not-found")
	}
}

// A timer that is loaded but inactive will never fire. Reporting it as
// registered would make `status` green for a job that does not run — which is
// precisely the class of defect #171 built this command to catch.
func TestSystemdInspect_LoadedButInactiveIsNotRegistered(t *testing.T) {
	m := newTestSystemd(t, &fakeRunner{reply: map[string]fakeReply{
		"": {out: showTimer("loaded", "inactive")},
	}})

	reg, err := m.Inspect("ccvault-sync")
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if reg.Found {
		t.Error("Found = true for a loaded-but-inactive timer, which never fires")
	}
}

func TestSystemdInspect_ReadsEveryFactStatusNeeds(t *testing.T) {
	m := newTestSystemd(t, &fakeRunner{reply: map[string]fakeReply{
		"systemctl --user show ccvault-sync.timer --property=LoadState --property=ActiveState --property=FragmentPath": {
			out: showTimer("loaded", "active"),
		},
		"systemctl --user show ccvault-sync.service --property=ExecStart --property=ExecMainStatus --property=ExecMainExitTimestamp --property=NRestarts --property=StandardOutput": {
			out: showService("/home/d/go/bin/ccvault", "Wed 2026-10-28 09:15:00 UTC", "0", "append:/home/d/.ccvault/schedule.log"),
		},
	}})

	reg, err := m.Inspect("ccvault-sync")
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if !reg.Found {
		t.Fatal("Found = false for a loaded, active timer")
	}
	if got, want := reg.Program, "/home/d/go/bin/ccvault"; got != want {
		t.Errorf("Program = %q, want %q", got, want)
	}
	if got, want := strings.Join(reg.Arguments, " "), "/home/d/go/bin/ccvault sync"; got != want {
		t.Errorf("Arguments = %q, want %q", got, want)
	}
	if got, want := reg.UnitPath, "/home/d/.config/systemd/user/ccvault-sync.timer"; got != want {
		t.Errorf("UnitPath = %q, want %q", got, want)
	}
	if got, want := reg.LogPath, "/home/d/.ccvault/schedule.log"; got != want {
		t.Errorf("LogPath = %q, want %q", got, want)
	}
	if !reg.LastRunKnown || reg.LastRun.Year() != 2026 {
		t.Errorf("LastRun = %s (known %v), want the ExecMainExitTimestamp", reg.LastRun, reg.LastRunKnown)
	}
	if !reg.LastExitKnown || reg.LastExitCode != 0 {
		t.Errorf("LastExitCode = %d (known %v), want 0 known", reg.LastExitCode, reg.LastExitKnown)
	}
	// systemctl show reports the next elapse, not the configured period, so
	// Interval stays zero rather than being inferred.
	if reg.Interval != 0 {
		t.Errorf("Interval = %s, want 0 — systemd does not report the period and guessing it would be a fiction", reg.Interval)
	}
}

func TestSystemdInspect_ReadsAFailedRun(t *testing.T) {
	m := newTestSystemd(t, &fakeRunner{reply: map[string]fakeReply{
		"systemctl --user show ccvault-sync.timer --property=LoadState --property=ActiveState --property=FragmentPath": {
			out: showTimer("loaded", "active"),
		},
		"": {out: showService("/bin/ccvault", "Wed 2026-10-28 09:15:00 UTC", "1", "journal")},
	}})

	reg, err := m.Inspect("ccvault-sync")
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if !reg.LastExitKnown || reg.LastExitCode != 1 {
		t.Errorf("LastExitCode = %d (known %v), want 1 known", reg.LastExitCode, reg.LastExitKnown)
	}
}

// ExecMainStatus is "0" for a service that has never run as well as for one
// that succeeded. The absent timestamp is the only thing distinguishing them,
// so "never ran" must not read as "last run succeeded".
func TestSystemdInspect_NeverRunIsNotASuccessfulRun(t *testing.T) {
	m := newTestSystemd(t, &fakeRunner{reply: map[string]fakeReply{
		"systemctl --user show ccvault-sync.timer --property=LoadState --property=ActiveState --property=FragmentPath": {
			out: showTimer("loaded", "active"),
		},
		"": {out: showService("/bin/ccvault", "n/a", "0", "journal")},
	}})

	reg, err := m.Inspect("ccvault-sync")
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if reg.LastExitKnown {
		t.Error("LastExitKnown = true for a service that has never exited")
	}
	if reg.LastRunKnown {
		t.Error("LastRunKnown = true for a service that has never run")
	}
}

// The failure every headless machine produces. It must be reported as itself:
// "I cannot tell" and "there is no job" call for completely different
// responses from the user, and a container has no session bus at all.
func TestSystemdInspect_NoSessionBusIsItsOwnError(t *testing.T) {
	m := newTestSystemd(t, &fakeRunner{reply: map[string]fakeReply{
		"": {out: "Failed to connect to bus: No medium found", code: 1},
	}})

	_, err := m.Inspect("ccvault-sync")
	if err == nil {
		t.Fatal("Inspect() = nil error with no session bus")
	}
	if !strings.Contains(err.Error(), "session bus") {
		t.Errorf("Inspect() error = %q, want it to name the missing session bus", err)
	}
	if !strings.Contains(err.Error(), "cannot tell") {
		t.Errorf("Inspect() error = %q, want it to say ccvault cannot tell, not that nothing is registered", err)
	}
}

func TestSystemdInspect_UnrunnableSystemctlIsAnError(t *testing.T) {
	m := newTestSystemd(t, &fakeRunner{err: errors.New("exec: \"systemctl\": executable file not found in $PATH")})

	if _, err := m.Inspect("ccvault-sync"); err == nil {
		t.Error("Inspect() = nil error when systemctl could not be run at all")
	}
}

// Install writes both units, reloads, enables with --now, and verifies. The
// --now matters: without it the timer waits for the next login and the
// command would report an installed job that does not run.
func TestSystemdInstall_WritesBothUnitsEnablesNowAndVerifies(t *testing.T) {
	r := &fakeRunner{reply: map[string]fakeReply{
		"systemctl --user show ccvault-sync.timer --property=LoadState --property=ActiveState --property=FragmentPath": {
			out: showTimer("loaded", "active"),
		},
		"": {out: showService("/home/d/go/bin/ccvault", "n/a", "0", "journal")},
	}}
	m := newTestSystemd(t, r)
	j := NewJob("/home/d/go/bin/ccvault", "/home/d/.ccvault/schedule.log")
	j.Label = "ccvault-sync"

	if err := m.Install(j); err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	joined := strings.Join(r.calls, "\n")
	if !strings.Contains(joined, "systemctl --user daemon-reload") {
		t.Errorf("no daemon-reload; calls =\n%s", joined)
	}
	if !strings.Contains(joined, "systemctl --user enable --now ccvault-sync.timer") {
		t.Errorf("timer was not enabled with --now; calls =\n%s", joined)
	}
	// User scope only, no sudo: #171 settled on user-level units.
	for _, call := range r.calls {
		if strings.Contains(call, "--system") || strings.Contains(call, "sudo") {
			t.Errorf("call %q escalates out of user scope", call)
		}
	}

	for name, render := range map[string]func(Job) (string, error){
		"ccvault-sync.service": RenderSystemdService,
		"ccvault-sync.timer":   RenderSystemdTimer,
	} {
		path := filepath.Join(m.unitsDir, name)
		got, err := os.ReadFile(path) //nolint:gosec // G304: path built from t.TempDir()
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		want, err := render(j)
		if err != nil {
			t.Fatalf("render %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("%s on disk is not what the renderer produces:\n%s", name, got)
		}
	}
}

// The verification step. systemd can accept `enable --now` and still leave
// the timer inactive — a masked unit, a syntax error it only notices on load.
func TestSystemdInstall_FailsWhenTheTimerIsNotActiveAfterwards(t *testing.T) {
	m := newTestSystemd(t, &fakeRunner{reply: map[string]fakeReply{
		"": {out: showTimer("loaded", "inactive")},
	}})
	j := NewJob("/home/d/go/bin/ccvault", "")
	j.Label = "ccvault-sync"

	err := m.Install(j)
	if err == nil {
		t.Fatal("Install() = nil for a timer that is not active afterwards")
	}
	if !strings.Contains(err.Error(), "not active") {
		t.Errorf("Install() error = %q, want it to say the timer is not active", err)
	}
}

func TestSystemdInstall_SurfacesAnEnableRefusal(t *testing.T) {
	m := newTestSystemd(t, &fakeRunner{reply: map[string]fakeReply{
		"systemctl --user enable --now ccvault-sync.timer": {
			out: "Failed to enable unit: Unit file ccvault-sync.timer is masked.", code: 1,
		},
	}})
	j := NewJob("/home/d/go/bin/ccvault", "")
	j.Label = "ccvault-sync"

	err := m.Install(j)
	if err == nil {
		t.Fatal("Install() = nil for an enable that failed")
	}
	if !strings.Contains(err.Error(), "masked") {
		t.Errorf("Install() error = %q, want systemd's own message in it", err)
	}
}

func TestSystemdInstall_RefusesADestructiveJobBeforeTouchingDisk(t *testing.T) {
	r := &fakeRunner{}
	m := newTestSystemd(t, r)
	j := NewJob("/home/d/go/bin/ccvault", "", "--full")
	j.Label = "ccvault-sync"

	if err := m.Install(j); err == nil {
		t.Fatal("Install() = nil for `sync --full`")
	}
	if len(r.calls) != 0 {
		t.Errorf("systemctl was called %v for a job that should never have been written", r.calls)
	}
	if _, err := os.Stat(m.unitsDir); err == nil {
		t.Error("Install() created the unit directory for a job it refused")
	}
}

// Both unit files go, not just the timer: a .service left behind is a unit
// `status` keeps finding, and a .timer left behind is one `enable` away from
// being back.
func TestSystemdUninstall_DisablesAndRemovesBothUnits(t *testing.T) {
	timerActive := showTimer("loaded", "active")
	timerGone := showTimer("not-found", "inactive")
	service := showService("/home/d/go/bin/ccvault", "n/a", "0", "journal")

	// Install against an always-registered fake, so there is something real
	// on disk to remove; then swap in a sequenced one so the post-check sees
	// the timer actually gone. A fake that answered "active" throughout would
	// let a broken Uninstall pass.
	var calls []string
	m := newTestSystemd(t, &fakeRunner{reply: map[string]fakeReply{
		"systemctl --user show ccvault-sync.timer --property=LoadState --property=ActiveState --property=FragmentPath": {out: timerActive},
		"": {out: service},
	}})
	j := NewJob("/home/d/go/bin/ccvault", "")
	j.Label = "ccvault-sync"
	if err := m.Install(j); err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	calls = nil // ignore the Install traffic; the Uninstall sequence is what is asserted
	m.run = &sequencedRunner{outputs: []fakeReply{
		{out: timerActive}, // Inspect: timer
		{out: service},     // Inspect: service
		{},                 // disable --now
		{},                 // daemon-reload
		{out: timerGone},   // Inspect: after
	}, record: &calls}

	if err := m.Uninstall("ccvault-sync"); err != nil {
		t.Fatalf("Uninstall() error = %v", err)
	}

	for _, name := range []string{"ccvault-sync.service", "ccvault-sync.timer"} {
		if _, err := os.Stat(filepath.Join(m.unitsDir, name)); !os.IsNotExist(err) {
			t.Errorf("%s survived Uninstall", name)
		}
	}
	joined := strings.Join(calls, "\n")
	if !strings.Contains(joined, "systemctl --user disable --now ccvault-sync.timer") {
		t.Errorf("timer was not disabled; calls =\n%s", joined)
	}
	if !strings.Contains(joined, "systemctl --user daemon-reload") {
		t.Errorf("no daemon-reload after removing the units; calls =\n%s", joined)
	}
}

func TestSystemdUninstall_NothingRegisteredIsErrNotRegistered(t *testing.T) {
	m := newTestSystemd(t, &fakeRunner{reply: map[string]fakeReply{
		"": {out: showTimer("not-found", "inactive")},
	}})

	err := m.Uninstall("ccvault-sync")
	if !errors.Is(err, ErrNotRegistered) {
		t.Errorf("Uninstall() = %v, want ErrNotRegistered", err)
	}
}

// Unit files on disk that systemd has not loaded are still a job: one
// daemon-reload away from being live. Uninstall removes them.
func TestSystemdUninstall_RemovesUnloadedUnitFiles(t *testing.T) {
	m := newTestSystemd(t, &fakeRunner{reply: map[string]fakeReply{
		"": {out: showTimer("not-found", "inactive")},
	}})
	if err := os.MkdirAll(m.unitsDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	timerPath := filepath.Join(m.unitsDir, "ccvault-sync.timer")
	if err := os.WriteFile(timerPath, []byte("[Timer]\n"), 0o600); err != nil {
		t.Fatalf("write timer: %v", err)
	}

	if err := m.Uninstall("ccvault-sync"); err != nil {
		t.Fatalf("Uninstall() error = %v, want it to clean up unloaded units", err)
	}
	if _, err := os.Stat(timerPath); !os.IsNotExist(err) {
		t.Error("the unloaded timer survived Uninstall")
	}
}

func TestSystemdUninstall_FailsWhenTheTimerSurvives(t *testing.T) {
	m := newTestSystemd(t, &fakeRunner{reply: map[string]fakeReply{
		"systemctl --user show ccvault-sync.timer --property=LoadState --property=ActiveState --property=FragmentPath": {
			out: showTimer("loaded", "active"),
		},
		"": {out: showService("/bin/ccvault", "n/a", "0", "journal")},
	}})

	err := m.Uninstall("ccvault-sync")
	if err == nil {
		t.Fatal("Uninstall() = nil for a timer still active afterwards")
	}
	if !strings.Contains(err.Error(), "still active") {
		t.Errorf("Uninstall() error = %q, want it to say the timer survived", err)
	}
}

func TestSystemdManager_RejectsALabelThatIsAPath(t *testing.T) {
	r := &fakeRunner{}
	m := newTestSystemd(t, r)

	if _, err := m.Inspect("../../../etc/evil"); err == nil {
		t.Error("Inspect() accepted a traversal label")
	}
	if err := m.Uninstall("../../../etc/evil"); err == nil {
		t.Error("Uninstall() accepted a traversal label")
	}
	if len(r.calls) != 0 {
		t.Errorf("systemctl was called %v with a traversal label", r.calls)
	}
}

func TestSystemdPlatform(t *testing.T) {
	if got := newTestSystemd(t, &fakeRunner{}).Platform(); got != "systemd" {
		t.Errorf("Platform() = %q, want %q", got, "systemd")
	}
}

func TestSystemdUserUnitDir_HonoursXDGConfigHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/custom/config")
	dir, err := systemdUserUnitDir()
	if err != nil {
		t.Fatalf("systemdUserUnitDir() error = %v", err)
	}
	if got, want := dir, filepath.Join("/custom/config", "systemd", "user"); got != want {
		t.Errorf("systemdUserUnitDir() = %q, want %q", got, want)
	}
}

func TestSystemdUserUnitDir_FallsBackToDotConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	dir, err := systemdUserUnitDir()
	if err != nil {
		t.Fatalf("systemdUserUnitDir() error = %v", err)
	}
	if !strings.HasSuffix(dir, filepath.Join(".config", "systemd", "user")) {
		t.Errorf("systemdUserUnitDir() = %q, want it to end in .config/systemd/user", dir)
	}
}

func TestIsSystemdNoSessionBus(t *testing.T) {
	yes := []string{
		"Failed to connect to bus: No medium found",
		"Failed to connect to bus: $DBUS_SESSION_BUS_ADDRESS and $XDG_RUNTIME_DIR not defined",
	}
	for _, out := range yes {
		if !isSystemdNoSessionBus(out) {
			t.Errorf("isSystemdNoSessionBus(%q) = false", out)
		}
	}
	if isSystemdNoSessionBus("Unit ccvault-sync.timer is masked.") {
		t.Error("isSystemdNoSessionBus() matched a masked-unit error")
	}
}

func TestIsLaunchdNotFound(t *testing.T) {
	if !isLaunchdNotFound(notFoundOutput) {
		t.Error("isLaunchdNotFound() = false for launchd's own not-found output")
	}
	if isLaunchdNotFound("Operation not permitted") {
		t.Error("isLaunchdNotFound() matched a permissions error")
	}
}

func TestPlatform_AgreesWithSupported(t *testing.T) {
	// Not a tautology: it pins that the three entry points are consistent on
	// whatever platform the suite is running on, which is the thing a new
	// platform_*.go file could get wrong.
	name := Platform()
	if name == "" {
		t.Error("Platform() = empty")
	}
	m, err := New()
	switch {
	case Supported():
		if err != nil {
			t.Fatalf("New() error = %v on a platform reporting Supported() == true", err)
		}
		if m.Platform() != name {
			t.Errorf("New().Platform() = %q, Platform() = %q; they must agree", m.Platform(), name)
		}
	default:
		if !errors.Is(err, ErrUnsupportedPlatform) {
			t.Errorf("New() = %v on an unsupported platform, want ErrUnsupportedPlatform", err)
		}
	}
}

// Both constructors are tested here rather than only on the platform that
// selects them. On macOS newSystemdManager has no non-test caller and on
// Linux newLaunchdManager has none, so without these the `unused` linter is
// right about one of them on every build.
func TestNewSystemdManager_PointsAtTheUserUnitDirectory(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m, err := newSystemdManager()
	if err != nil {
		t.Fatalf("newSystemdManager() error = %v", err)
	}
	want := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "systemd", "user")
	if m.unitsDir != want {
		t.Errorf("unitsDir = %q, want %q", m.unitsDir, want)
	}
	if m.run == nil {
		t.Error("run is nil; the real manager must carry a runner")
	}
}

func TestNewLaunchdManager_PointsAtLaunchAgentsAndTheGUIDomain(t *testing.T) {
	m, err := newLaunchdManager(501)
	if err != nil {
		t.Fatalf("newLaunchdManager() error = %v", err)
	}
	if !strings.HasSuffix(m.agentsDir, filepath.Join("Library", "LaunchAgents")) {
		t.Errorf("agentsDir = %q, want it to end in Library/LaunchAgents", m.agentsDir)
	}
	// gui/<uid>, never system/: a LaunchDaemon would need sudo and run the
	// sync as root.
	if m.domain != "gui/501" {
		t.Errorf("domain = %q, want %q", m.domain, "gui/501")
	}
	if m.run == nil {
		t.Error("run is nil; the real manager must carry a runner")
	}
}
