// ABOUTME: Tests the launchd backend's argv, order of operations, post-install verification and output parsing
// ABOUTME: Against a fake launchctl, so it runs on Linux CI too — the real launchd is exercised by hand, see docs/schedule.md

package schedule

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeRunner records what was asked of the scheduler CLI and answers from a
// script, so install/uninstall/inspect can be driven through every branch
// without a scheduler.
type fakeRunner struct {
	// calls is every invocation, in order, rendered as "name arg arg".
	calls []string

	// reply answers a call. The key is the rendered call; a key of "" is the
	// fallback. Missing keys answer exit 0 with empty output.
	reply map[string]fakeReply

	// err, when non-nil, is returned for every call: the command could not
	// be run at all.
	err error
}

type fakeReply struct {
	out  string
	code int
}

func (f *fakeRunner) Run(name string, args ...string) (string, int, error) {
	call := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, call)
	if f.err != nil {
		return "", -1, f.err
	}
	if r, ok := f.reply[call]; ok {
		return r.out, r.code, nil
	}
	if r, ok := f.reply[""]; ok {
		return r.out, r.code, nil
	}
	return "", 0, nil
}

// notFoundOutput is what `launchctl print` says for a label nothing holds.
// Copied from a real run on macOS 15 (exit status 113).
const notFoundOutput = `Bad request.
Could not find service "ai.2389.ccvault.sync" in domain for user gui: 501
`

// printOutput is `launchctl print` for a registered agent, trimmed to the
// shape of a real run on macOS 15.
func printOutput(program, logPath string, runs int, lastExit string) string {
	return `gui/501/ai.2389.ccvault.sync = {
	active count = 0
	path = /Users/d/Library/LaunchAgents/ai.2389.ccvault.sync.plist
	type = LaunchAgent
	state = not running

	program = ` + program + `
	arguments = {
		` + program + `
		sync
	}

	stdout path = ` + logPath + `
	stderr path = ` + logPath + `
	inherited environment = {
		SSH_AUTH_SOCK => /var/run/com.apple.launchd.YxH1eqRC8n/Listeners
	}

	default environment = {
		PATH => /usr/bin:/bin:/usr/sbin:/sbin
	}

	domain = gui/501 [100023]
	minimum runtime = 10
	exit timeout = 5
	runs = ` + itoa(runs) + `
	last exit code = ` + lastExit + `

	run interval = 3600 seconds

	properties = inferred program
}
`
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func newTestLaunchd(t *testing.T, r *fakeRunner) *launchdManager {
	t.Helper()
	return &launchdManager{agentsDir: filepath.Join(t.TempDir(), "LaunchAgents"), domain: "gui/501", run: r}
}

// The format `launchctl print` emits is the only thing standing between
// `status` and the facts it reports. Parsed from a real run's output.
func TestParseLaunchctlPrint_ReadsEveryFactStatusNeeds(t *testing.T) {
	reg := parseLaunchctlPrint(printOutput("/Users/d/go/bin/ccvault", "/Users/d/.ccvault/schedule.log", 42, "0"))

	if got, want := reg.Program, "/Users/d/go/bin/ccvault"; got != want {
		t.Errorf("Program = %q, want %q", got, want)
	}
	if got, want := strings.Join(reg.Arguments, " "), "/Users/d/go/bin/ccvault sync"; got != want {
		t.Errorf("Arguments = %q, want %q", got, want)
	}
	if got, want := reg.UnitPath, "/Users/d/Library/LaunchAgents/ai.2389.ccvault.sync.plist"; got != want {
		t.Errorf("UnitPath = %q, want %q", got, want)
	}
	if got, want := reg.LogPath, "/Users/d/.ccvault/schedule.log"; got != want {
		t.Errorf("LogPath = %q, want %q", got, want)
	}
	if got, want := reg.Interval, time.Hour; got != want {
		t.Errorf("Interval = %s, want %s", got, want)
	}
	if !reg.RunsKnown || reg.Runs != 42 {
		t.Errorf("Runs = %d (known %v), want 42 known", reg.Runs, reg.RunsKnown)
	}
	if !reg.LastExitKnown || reg.LastExitCode != 0 {
		t.Errorf("LastExitCode = %d (known %v), want 0 known", reg.LastExitCode, reg.LastExitKnown)
	}
}

// "(never exited)" must not parse as exit code 0. 0 means the sync
// succeeded; a fresh install has not run at all, and conflating them is how
// `status` would report a green light for a job that has never worked.
func TestParseLaunchctlPrint_NeverExitedIsNotSuccess(t *testing.T) {
	reg := parseLaunchctlPrint(printOutput("/bin/ccvault", "/tmp/l.log", 0, "(never exited)"))

	if reg.LastExitKnown {
		t.Error("LastExitKnown = true for \"(never exited)\"")
	}
	if reg.LastExitCode != 0 {
		t.Errorf("LastExitCode = %d; the field must stay zero AND unknown", reg.LastExitCode)
	}
}

func TestParseLaunchctlPrint_ReadsANonZeroExit(t *testing.T) {
	reg := parseLaunchctlPrint(printOutput("/bin/ccvault", "/tmp/l.log", 3, "1"))
	if !reg.LastExitKnown || reg.LastExitCode != 1 {
		t.Errorf("LastExitCode = %d (known %v), want 1 known", reg.LastExitCode, reg.LastExitKnown)
	}
}

// launchd's environment dump uses "KEY => value", which a naive split on the
// first '=' turns into a key of "OSLogRateLimit " and a value of "> 64".
// Splitting on " = " is what keeps those lines out of the parse.
func TestParseLaunchctlPrint_IgnoresEnvironmentArrowLines(t *testing.T) {
	out := `gui/501/x = {
	program = /bin/ccvault
	environment = {
		PATH => /usr/bin:/bin
		runs => 999
	}
	runs = 7
}
`
	reg := parseLaunchctlPrint(out)
	if reg.Runs != 7 {
		t.Errorf("Runs = %d, want 7 — the 'runs => 999' environment line was parsed as the field", reg.Runs)
	}
}

// launchd omits `program` when the plist has none and it cannot infer one;
// argv[0] is what it actually executes, so that is the fallback.
func TestParseLaunchctlPrint_FallsBackToArgvZero(t *testing.T) {
	out := `gui/501/x = {
	arguments = {
		/opt/homebrew/bin/ccvault
		sync
	}
}
`
	reg := parseLaunchctlPrint(out)
	if got, want := reg.Program, "/opt/homebrew/bin/ccvault"; got != want {
		t.Errorf("Program = %q, want %q", got, want)
	}
}

func TestSplitLaunchdField(t *testing.T) {
	cases := []struct {
		line, key, value string
		ok               bool
	}{
		{"program = /bin/ccvault", "program", "/bin/ccvault", true},
		{"run interval = 3600 seconds", "run interval", "3600 seconds", true},
		{"PATH => /usr/bin", "PATH =", "", false},
		{"state = not running", "state", "not running", true},
		{"no separator here", "", "", false},
		{"trailing = ", "", "", false},
	}
	for _, c := range cases {
		key, value, ok := splitLaunchdField(c.line)
		if ok != c.ok || (ok && (key != c.key || value != c.value)) {
			t.Errorf("splitLaunchdField(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.line, key, value, ok, c.key, c.value, c.ok)
		}
	}
}

// Fact 1, the not-registered case: launchd's 113 refusal is a fact, and must
// come back as a zero Registration with no error.
func TestLaunchdInspect_NotRegisteredIsNotAnError(t *testing.T) {
	r := &fakeRunner{reply: map[string]fakeReply{"": {out: notFoundOutput, code: 113}}}
	m := newTestLaunchd(t, r)

	reg, err := m.Inspect(DefaultLabel)
	if err != nil {
		t.Fatalf("Inspect() error = %v, want nil — nothing registered is an answer", err)
	}
	if reg.Found {
		t.Error("Found = true for a label launchd could not find")
	}
}

// Any other refusal is a real error. Returning "not registered" for it would
// make `status` advise `schedule install` when the real problem is something
// else entirely.
func TestLaunchdInspect_OtherRefusalsAreErrors(t *testing.T) {
	r := &fakeRunner{reply: map[string]fakeReply{"": {out: "Operation not permitted", code: 1}}}
	m := newTestLaunchd(t, r)

	if _, err := m.Inspect(DefaultLabel); err == nil {
		t.Error("Inspect() = nil error for an unexplained launchctl failure")
	}
}

func TestLaunchdInspect_UnrunnableLaunchctlIsAnError(t *testing.T) {
	r := &fakeRunner{err: errors.New("exec: \"launchctl\": executable file not found in $PATH")}
	m := newTestLaunchd(t, r)

	if _, err := m.Inspect(DefaultLabel); err == nil {
		t.Error("Inspect() = nil error when launchctl could not be run at all")
	}
}

// launchd keeps no last-run timestamp, so the log's mtime is it. A log that
// is not there yet means the job has not run, which must read as unknown.
func TestLaunchdInspect_ReadsLastRunFromTheLogMtime(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "schedule.log")
	r := &fakeRunner{reply: map[string]fakeReply{
		"": {out: printOutput("/bin/ccvault", logPath, 1, "0")},
	}}
	m := newTestLaunchd(t, r)

	// No log yet.
	reg, err := m.Inspect(DefaultLabel)
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if reg.LastRunKnown {
		t.Error("LastRunKnown = true with no log file on disk")
	}

	// Log present, with a known mtime.
	if err := os.WriteFile(logPath, []byte("synced\n"), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}
	stamp := time.Date(2026, 10, 28, 9, 15, 0, 0, time.UTC)
	if err := os.Chtimes(logPath, stamp, stamp); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	reg, err = m.Inspect(DefaultLabel)
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if !reg.LastRunKnown {
		t.Fatal("LastRunKnown = false with the log on disk")
	}
	if !reg.LastRun.Equal(stamp) {
		t.Errorf("LastRun = %s, want the log's mtime %s", reg.LastRun, stamp)
	}
}

// Install writes the plist, boots out any previous registration, bootstraps,
// and then checks. The bootout-before-bootstrap is what makes a second
// install succeed instead of failing with "service already loaded".
func TestLaunchdInstall_WritesThePlistThenBootstrapsThenVerifies(t *testing.T) {
	r := &fakeRunner{reply: map[string]fakeReply{
		"": {out: printOutput("/usr/local/bin/ccvault", "/tmp/l.log", 0, "(never exited)")},
	}}
	m := newTestLaunchd(t, r)
	j := NewJob("/usr/local/bin/ccvault", "/tmp/l.log")

	if err := m.Install(j); err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	plistPath := filepath.Join(m.agentsDir, "ai.2389.ccvault.sync.plist")
	want := []string{
		"launchctl bootout gui/501/ai.2389.ccvault.sync",
		"launchctl bootstrap gui/501 " + plistPath,
		"launchctl print gui/501/ai.2389.ccvault.sync",
	}
	if strings.Join(r.calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("launchctl calls =\n%s\nwant\n%s", strings.Join(r.calls, "\n"), strings.Join(want, "\n"))
	}

	// Never --system, never sudo: #171 settled on user-level units only.
	for _, call := range r.calls {
		if strings.Contains(call, "sudo") || strings.Contains(call, "system/") {
			t.Errorf("call %q escalates out of the user domain", call)
		}
	}

	written, err := os.ReadFile(plistPath) //nolint:gosec // G304: path built from t.TempDir()
	if err != nil {
		t.Fatalf("read the plist Install wrote: %v", err)
	}
	expected, err := RenderLaunchdPlist(j)
	if err != nil {
		t.Fatalf("RenderLaunchdPlist: %v", err)
	}
	if string(written) != expected {
		t.Errorf("the plist on disk is not what RenderLaunchdPlist produces:\n%s", written)
	}
}

// The verification step, and the reason it exists. launchctl bootstrap can
// exit 0 and leave nothing registered; without the re-inspect, install would
// print a success message for a job that does not exist.
func TestLaunchdInstall_FailsWhenBootstrapSucceedsButNothingIsRegistered(t *testing.T) {
	r := &fakeRunner{reply: map[string]fakeReply{
		"launchctl print gui/501/ai.2389.ccvault.sync": {out: notFoundOutput, code: 113},
	}}
	m := newTestLaunchd(t, r)

	err := m.Install(NewJob("/usr/local/bin/ccvault", ""))
	if err == nil {
		t.Fatal("Install() = nil for a bootstrap that registered nothing")
	}
	if !strings.Contains(err.Error(), "not registered") {
		t.Errorf("Install() error = %q, want it to say the job is not registered", err)
	}
}

func TestLaunchdInstall_SurfacesABootstrapRefusal(t *testing.T) {
	r := &fakeRunner{reply: map[string]fakeReply{
		"": {out: "Load failed: 5: Input/output error", code: 5},
	}}
	m := newTestLaunchd(t, r)

	err := m.Install(NewJob("/usr/local/bin/ccvault", ""))
	if err == nil {
		t.Fatal("Install() = nil for a bootstrap that exited 5")
	}
	if !strings.Contains(err.Error(), "Load failed") {
		t.Errorf("Install() error = %q, want launchctl's own message in it", err)
	}
}

func TestLaunchdInstall_RefusesADestructiveJobBeforeTouchingDisk(t *testing.T) {
	r := &fakeRunner{}
	m := newTestLaunchd(t, r)

	if err := m.Install(NewJob("/usr/local/bin/ccvault", "", "--rebuild")); err == nil {
		t.Fatal("Install() = nil for `sync --rebuild`")
	}
	if len(r.calls) != 0 {
		t.Errorf("launchctl was called %v for a job that should never have been written", r.calls)
	}
	if _, err := os.Stat(m.agentsDir); err == nil {
		t.Error("Install() created the LaunchAgents directory for a job it refused")
	}
}

// Idempotent: installing twice leaves one registration and one plist.
func TestLaunchdInstall_IsIdempotent(t *testing.T) {
	r := &fakeRunner{reply: map[string]fakeReply{
		"": {out: printOutput("/usr/local/bin/ccvault", "/tmp/l.log", 0, "(never exited)")},
	}}
	m := newTestLaunchd(t, r)
	j := NewJob("/usr/local/bin/ccvault", "/tmp/l.log")

	for i := range 2 {
		if err := m.Install(j); err != nil {
			t.Fatalf("Install() #%d error = %v", i+1, err)
		}
	}

	entries, err := os.ReadDir(m.agentsDir)
	if err != nil {
		t.Fatalf("read agents dir: %v", err)
	}
	if len(entries) != 1 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("LaunchAgents holds %v, want exactly the one plist — a leftover temp file counts", names)
	}
}

// Both halves, always: a plist left in ~/Library/LaunchAgents is
// re-bootstrapped at the next login, so a bootout alone uninstalls the job
// only until the user reboots.
func TestLaunchdUninstall_BootsOutAndDeletesThePlist(t *testing.T) {
	plistOut := printOutput("/usr/local/bin/ccvault", "/tmp/l.log", 1, "0")
	r := &fakeRunner{reply: map[string]fakeReply{"": {out: plistOut}}}
	m := newTestLaunchd(t, r)
	if err := m.Install(NewJob("/usr/local/bin/ccvault", "/tmp/l.log")); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	plistPath := filepath.Join(m.agentsDir, "ai.2389.ccvault.sync.plist")

	// A real bootout changes what launchd reports, so the fake has to as
	// well: Inspect finds the job, bootout succeeds, and the post-check
	// Inspect finds it gone. A fake that answered the same thing throughout
	// would let a broken Uninstall pass.
	r.calls = nil
	m.run = &sequencedRunner{outputs: []fakeReply{
		{out: plistOut},                  // Inspect, before
		{},                               // bootout
		{out: notFoundOutput, code: 113}, // Inspect, after
	}, record: &r.calls}

	if err := m.Uninstall(DefaultLabel); err != nil {
		t.Fatalf("Uninstall() error = %v", err)
	}
	if _, err := os.Stat(plistPath); !os.IsNotExist(err) {
		t.Errorf("the plist is still at %s after Uninstall", plistPath)
	}
	if !strings.Contains(strings.Join(r.calls, "\n"), "launchctl bootout gui/501/ai.2389.ccvault.sync") {
		t.Errorf("Uninstall() did not boot the job out; calls = %v", r.calls)
	}
}

func TestLaunchdUninstall_NothingRegisteredIsErrNotRegistered(t *testing.T) {
	r := &fakeRunner{reply: map[string]fakeReply{"": {out: notFoundOutput, code: 113}}}
	m := newTestLaunchd(t, r)

	err := m.Uninstall(DefaultLabel)
	if !errors.Is(err, ErrNotRegistered) {
		t.Errorf("Uninstall() = %v, want ErrNotRegistered", err)
	}
}

// A plist on disk that launchd has not loaded is still a job: it loads at the
// next login. Uninstall has to remove it even though nothing is registered.
func TestLaunchdUninstall_RemovesAnUnloadedPlist(t *testing.T) {
	r := &fakeRunner{reply: map[string]fakeReply{"": {out: notFoundOutput, code: 113}}}
	m := newTestLaunchd(t, r)
	if err := os.MkdirAll(m.agentsDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	plistPath := filepath.Join(m.agentsDir, "ai.2389.ccvault.sync.plist")
	if err := os.WriteFile(plistPath, []byte("<plist/>"), 0o600); err != nil {
		t.Fatalf("write plist: %v", err)
	}

	if err := m.Uninstall(DefaultLabel); err != nil {
		t.Fatalf("Uninstall() error = %v, want it to clean up an unloaded plist", err)
	}
	if _, err := os.Stat(plistPath); !os.IsNotExist(err) {
		t.Error("the unloaded plist survived Uninstall and would load at the next login")
	}
}

// Uninstall verifies too: a bootout that exits 0 and leaves the job running
// must not report success.
func TestLaunchdUninstall_FailsWhenTheJobSurvives(t *testing.T) {
	plistOut := printOutput("/usr/local/bin/ccvault", "/tmp/l.log", 1, "0")
	m := newTestLaunchd(t, &fakeRunner{})
	m.run = &fakeRunner{reply: map[string]fakeReply{"": {out: plistOut}}}

	err := m.Uninstall(DefaultLabel)
	if err == nil {
		t.Fatal("Uninstall() = nil for a job still registered afterwards")
	}
	if !strings.Contains(err.Error(), "still registered") {
		t.Errorf("Uninstall() error = %q, want it to say the job survived", err)
	}
}

func TestLaunchdManager_RejectsALabelThatIsAPath(t *testing.T) {
	r := &fakeRunner{}
	m := newTestLaunchd(t, r)

	if _, err := m.Inspect("../../../etc/evil"); err == nil {
		t.Error("Inspect() accepted a traversal label")
	}
	if err := m.Uninstall("../../../etc/evil"); err == nil {
		t.Error("Uninstall() accepted a traversal label")
	}
	if len(r.calls) != 0 {
		t.Errorf("launchctl was called %v with a traversal label", r.calls)
	}
}

func TestLaunchdPlatform(t *testing.T) {
	if got := newTestLaunchd(t, &fakeRunner{}).Platform(); got != "launchd" {
		t.Errorf("Platform() = %q, want %q", got, "launchd")
	}
}

// writeFileAtomic is what stops a half-written plist from sitting in
// LaunchAgents looking installed.
func TestWriteFileAtomic_LeavesNoTempFileBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.plist")

	if err := writeFileAtomic(path, []byte("first"), 0o600); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}
	if err := writeFileAtomic(path, []byte("second"), 0o600); err != nil {
		t.Fatalf("writeFileAtomic (overwrite): %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "x.plist" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("directory holds %v, want just x.plist", names)
	}
	got, err := os.ReadFile(path) //nolint:gosec // G304: path built from t.TempDir()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "second" {
		t.Errorf("content = %q, want the second write", got)
	}
}

func TestWriteFileAtomic_FailsWhenTheDirectoryIsNotThere(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope", "x.plist")
	if err := writeFileAtomic(path, []byte("x"), 0o600); err == nil {
		t.Error("writeFileAtomic() = nil for a missing directory")
	}
}

// sequencedRunner answers calls in order, which is how a state transition
// (registered, then booted out, then gone) is modelled without a scheduler.
type sequencedRunner struct {
	outputs []fakeReply
	n       int
	record  *[]string
}

func (s *sequencedRunner) Run(name string, args ...string) (string, int, error) {
	if s.record != nil {
		*s.record = append(*s.record, strings.Join(append([]string{name}, args...), " "))
	}
	if s.n >= len(s.outputs) {
		return "", 0, nil
	}
	r := s.outputs[s.n]
	s.n++
	return r.out, r.code, nil
}
