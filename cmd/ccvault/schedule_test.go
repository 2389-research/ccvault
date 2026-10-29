// ABOUTME: Tests the CLI surface of `ccvault schedule` — the rendering, the JSON shape, the argv the job gets, and the exit verdict
// ABOUTME: The report's judgement is tested in internal/schedule; this covers what a person and a script actually read

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/2389-research/ccvault/internal/schedule"
	"github.com/spf13/cobra"
)

// registeredReport is a healthy report to render from. Each test below breaks
// one thing and asserts the rendering changes with it.
func registeredReport() schedule.Report {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	return schedule.Assemble(schedule.Facts{
		Platform:  "launchd",
		Supported: true,
		Label:     schedule.DefaultLabel,
		Registration: schedule.Registration{
			Found:         true,
			UnitPath:      "/Users/d/Library/LaunchAgents/ai.2389.ccvault.sync.plist",
			Program:       "/Users/d/go/bin/ccvault",
			Arguments:     []string{"/Users/d/go/bin/ccvault", "sync"},
			Interval:      time.Hour,
			Runs:          12,
			RunsKnown:     true,
			LastExitCode:  0,
			LastExitKnown: true,
			LastRun:       now.Add(-30 * time.Minute),
			LastRunKnown:  true,
		},
		ProgramResolved: "/Users/d/go/bin/ccvault",
		PathProgram:     "/Users/d/go/bin/ccvault",
		PathResolved:    "/Users/d/go/bin/ccvault",
		LastSynced:      now.Add(-30 * time.Minute),
		LogPath:         "/Users/d/.ccvault/schedule.log",
		Now:             now,
	})
}

func TestPrintScheduleStatus_ShowsAllFourFactsWhenHealthy(t *testing.T) {
	out := captureStdout(t, func() { printScheduleStatus(registeredReport()) })

	for _, want := range []string{
		"Registered:    yes",
		"Binary exists: yes",
		"Matches PATH:  yes",
		"exited 0",
		"last advanced",
		"Everything checks out.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status output is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Problems:") {
		t.Errorf("a healthy report printed a Problems section:\n%s", out)
	}
}

// The PATH mismatch is the failure a reader most needs named on the line that
// reports it, not only in the problem list underneath — it is #105 automated,
// and the fix is to re-run install with the other binary.
func TestPrintScheduleStatus_NamesTheOtherBinaryOnTheMatchLine(t *testing.T) {
	r := registeredReport()
	r.PathProgram = "/opt/homebrew/bin/ccvault"
	r.PathResolved = "/opt/homebrew/Cellar/ccvault/0.4.0/bin/ccvault"
	r = schedule.Assemble(r.Facts)

	out := captureStdout(t, func() { printScheduleStatus(r) })

	if !strings.Contains(out, "Matches PATH:  no (PATH has /opt/homebrew/bin/ccvault)") {
		t.Errorf("the match line does not name the binary on PATH:\n%s", out)
	}
	if !strings.Contains(out, "[binary-mismatch]") {
		t.Errorf("the problem list does not carry the code:\n%s", out)
	}
}

// An unsupported platform says so and stops. It must not print four facts it
// cannot establish, and it must not advise `schedule install`.
func TestPrintScheduleStatus_UnsupportedPlatformSaysSoAndStops(t *testing.T) {
	r := schedule.Assemble(schedule.Facts{Platform: "windows", Supported: false, Now: time.Now()})

	out := captureStdout(t, func() { printScheduleStatus(r) })

	if !strings.Contains(out, "cannot register a scheduled sync here yet") {
		t.Errorf("output does not say the platform is unsupported:\n%s", out)
	}
	for _, unwanted := range []string{"Registered:", "Binary exists:", "Matches PATH:", "schedule install"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("output claims %q on a platform with no scheduler:\n%s", unwanted, out)
		}
	}
}

// "Never run" must not render as a time or as a success. A fresh install sits
// in exactly this state for an hour.
func TestLastRunLine_DistinguishesNeverRunFromSucceeded(t *testing.T) {
	r := registeredReport()
	r.Registration.LastRunKnown = false
	r.Registration.LastExitKnown = false
	r.Registration.LastRun = time.Time{}

	if got := lastRunLine(r); !strings.Contains(got, "never") {
		t.Errorf("lastRunLine() = %q, want it to say the job has not run", got)
	}

	r = registeredReport()
	r.Registration.LastExitKnown = false
	if got := lastRunLine(r); !strings.Contains(got, "outcome not reported") {
		t.Errorf("lastRunLine() = %q, want it to say the outcome is unknown rather than imply success", got)
	}

	r = registeredReport()
	r.Registration.LastRunKnown = false
	if got := lastRunLine(r); !strings.Contains(got, "time not reported") {
		t.Errorf("lastRunLine() = %q, want it to say the time is unknown", got)
	}
}

func TestArchiveLine_NeverSyncedIsNotAnAge(t *testing.T) {
	r := registeredReport()
	r.LastSynced = time.Time{}
	if got := archiveLine(r); got != "never synced" {
		t.Errorf("archiveLine() = %q, want %q rather than an age measured from year 1", got, "never synced")
	}
}

// Every fact present on every run: a consumer checking "registered" must not
// have to tell an absent key from a false one.
func TestScheduleStatusJSON_CarriesEveryFactAndTheProblemCodes(t *testing.T) {
	r := registeredReport()
	r.Registration.Program = "/Users/d/.local/share/mise/installs/go/1.25.5/bin/ccvault"
	r.ProgramResolved = ""
	r = schedule.Assemble(r.Facts)

	raw, err := json.Marshal(scheduleStatusJSON(r))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out struct {
		Platform   string `json:"platform"`
		Supported  bool   `json:"supported"`
		Registered bool   `json:"registered"`
		Healthy    bool   `json:"healthy"`
		Binary     struct {
			RegisteredPath string `json:"registered_path"`
			Exists         bool   `json:"exists"`
			MatchesPath    bool   `json:"matches_path"`
		} `json:"binary"`
		LastRun struct {
			Known     bool `json:"known"`
			Succeeded bool `json:"succeeded"`
			ExitCode  int  `json:"exit_code"`
		} `json:"last_run"`
		Archive struct {
			Stale bool `json:"stale"`
		} `json:"archive"`
		Problems []struct {
			Code   string `json:"code"`
			Detail string `json:"detail"`
		} `json:"problems"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if out.Platform != "launchd" || !out.Supported || !out.Registered {
		t.Errorf("platform/supported/registered = %q/%v/%v", out.Platform, out.Supported, out.Registered)
	}
	if out.Healthy {
		t.Error("healthy = true for a report with a missing binary")
	}
	if out.Binary.Exists {
		t.Error("binary.exists = true for a path that does not resolve")
	}
	if !strings.Contains(out.Binary.RegisteredPath, "1.25.5") {
		t.Errorf("binary.registered_path = %q, want the dead path", out.Binary.RegisteredPath)
	}
	if len(out.Problems) == 0 || out.Problems[0].Code != schedule.ProblemBinaryMissing {
		t.Errorf("problems = %+v, want %s first", out.Problems, schedule.ProblemBinaryMissing)
	}
	if out.Problems[0].Detail == "" {
		t.Error("the problem carries a code and no detail")
	}
}

// An unsupported platform emits the keys that mean something and omits the
// three facts it cannot establish, rather than emitting them as false.
func TestScheduleStatusJSON_UnsupportedPlatformOmitsWhatItCannotKnow(t *testing.T) {
	r := schedule.Assemble(schedule.Facts{Platform: "windows", Supported: false, Now: time.Now()})

	out := scheduleStatusJSON(r)

	if out["supported"] != false {
		t.Errorf("supported = %v, want false", out["supported"])
	}
	for _, key := range []string{"binary", "last_run", "unit_path"} {
		if _, ok := out[key]; ok {
			t.Errorf("JSON carries %q on a platform with no scheduler", key)
		}
	}
	if _, ok := out["archive"]; !ok {
		t.Error("JSON omits archive staleness, which is knowable on every platform")
	}
}

// newScheduleFlagSet mirrors the persistent flags every subcommand inherits
// from the root, so scheduleExtraArgs can be driven without Execute.
func newScheduleFlagSet(t *testing.T) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "install"}
	cmd.Flags().String("data-dir", "", "")
	cmd.Flags().String("config", "", "")
	cmd.Flags().String("claude-home", "", "")
	return cmd
}

// #171: the job runs plain `ccvault sync`. Nothing is appended unless the
// caller named a different archive, in which case the unit has to say so or
// it would sync something other than what was asked for.
func TestScheduleExtraArgs_IsEmptyForADefaultInstall(t *testing.T) {
	cmd := newScheduleFlagSet(t)
	if got := scheduleExtraArgs(cmd); len(got) != 0 {
		t.Errorf("scheduleExtraArgs() = %v, want nothing — the job runs plain `ccvault sync`", got)
	}
}

func TestScheduleExtraArgs_CarriesTheArchiveOverrides(t *testing.T) {
	cmd := newScheduleFlagSet(t)
	for flag, value := range map[string]string{
		"data-dir":    "/tmp/fixture",
		"config":      "/tmp/fixture/config.toml",
		"claude-home": "/tmp/fixture/claude",
	} {
		if err := cmd.Flags().Set(flag, value); err != nil {
			t.Fatalf("set --%s: %v", flag, err)
		}
	}

	got := strings.Join(scheduleExtraArgs(cmd), " ")
	want := "--data-dir /tmp/fixture --config /tmp/fixture/config.toml --claude-home /tmp/fixture/claude"
	if got != want {
		t.Errorf("scheduleExtraArgs() = %q, want %q", got, want)
	}

	// And the whole point: whatever they are, the job is still a plain sync.
	job := schedule.NewJob("/bin/ccvault", "/tmp/l.log", scheduleExtraArgs(cmd)...)
	if err := job.Validate(); err != nil {
		t.Errorf("the job built from those flags does not validate: %v", err)
	}
}

// resolveOrEmpty's "" is load-bearing: schedule.Assemble reads it as "the
// registered binary is gone", which is half of fact 2.
func TestResolveOrEmpty(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "ccvault")
	if err := os.WriteFile(real, []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // G306: test fixture
		t.Fatalf("write: %v", err)
	}
	link := filepath.Join(dir, "ccvault-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	// A symlink and its target must resolve to the same string, or a Homebrew
	// install reads as a PATH mismatch forever.
	if resolveOrEmpty(link) != resolveOrEmpty(real) {
		t.Errorf("resolveOrEmpty(symlink) = %q, resolveOrEmpty(target) = %q; they must agree",
			resolveOrEmpty(link), resolveOrEmpty(real))
	}
	if got := resolveOrEmpty(filepath.Join(dir, "not-there")); got != "" {
		t.Errorf("resolveOrEmpty(missing) = %q, want empty", got)
	}
	if got := resolveOrEmpty(""); got != "" {
		t.Errorf("resolveOrEmpty(\"\") = %q, want empty", got)
	}
}

// Reads the running test binary's own path, which is the mechanism install
// uses to decide what to register.
func TestScheduleBinaryPath_IsAbsoluteAndResolved(t *testing.T) {
	got, err := scheduleBinaryPath()
	if err != nil {
		t.Fatalf("scheduleBinaryPath() error = %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("scheduleBinaryPath() = %q, want an absolute path — a unit file has no working directory", got)
	}
	if resolved := resolveOrEmpty(got); resolved != got {
		t.Errorf("scheduleBinaryPath() = %q but resolves to %q; it must already be resolved so status can compare it against PATH", got, resolved)
	}
}

// The whole command tree is reachable and the flags status needs are wired.
// A subcommand added to the var block and not to init() is invisible.
func TestScheduleCommand_IsWiredUp(t *testing.T) {
	sub := map[string]*cobra.Command{}
	for _, c := range scheduleCmd.Commands() {
		sub[c.Name()] = c
	}
	for _, name := range []string{"install", "status", "uninstall"} {
		c, ok := sub[name]
		if !ok {
			t.Errorf("`ccvault schedule %s` is not registered", name)
			continue
		}
		if c.Flags().Lookup("label") == nil {
			t.Errorf("`schedule %s` has no --label flag; install is idempotent on it, so all three need it", name)
		}
	}
	if scheduleStatusCmd.Flags().Lookup("json") == nil {
		t.Error("`schedule status` has no --json flag")
	}
	var found bool
	for _, c := range rootCmd.Commands() {
		if c.Name() == "schedule" {
			found = true
		}
	}
	if !found {
		t.Error("`schedule` is not attached to the root command")
	}
}

func TestJoinArgs(t *testing.T) {
	cases := map[string][]string{
		"":                  nil,
		"sync":              {"sync"},
		"sync --source cc":  {"sync", "--source", "cc"},
		"/bin/ccvault sync": {"/bin/ccvault", "sync"},
	}
	for want, in := range cases {
		if got := joinArgs(in); got != want {
			t.Errorf("joinArgs(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestYesNo(t *testing.T) {
	if yesNo(true) != "yes" || yesNo(false) != "no" {
		t.Errorf("yesNo = %q/%q, want yes/no", yesNo(true), yesNo(false))
	}
}
