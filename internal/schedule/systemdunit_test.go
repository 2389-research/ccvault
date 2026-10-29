// ABOUTME: Tests the systemd .service and .timer text byte for byte, including specifier and whitespace escaping
// ABOUTME: Pure — these run on macOS, which matters because the registration half cannot be verified there

package schedule

import (
	"strings"
	"testing"
	"time"
)

func TestRenderSystemdService_IsTheWholeFile(t *testing.T) {
	j := NewJob("/home/dylanr/go/bin/ccvault", "/home/dylanr/.ccvault/schedule.log")
	j.Label = "ccvault-sync"

	got, err := RenderSystemdService(j)
	if err != nil {
		t.Fatalf("RenderSystemdService() error = %v", err)
	}

	want := `[Unit]
Description=ccvault incremental conversation archive sync
Documentation=https://github.com/2389-research/ccvault

[Service]
Type=oneshot
ExecStart=/home/dylanr/go/bin/ccvault sync
Nice=5
IOSchedulingClass=idle
StandardOutput=append:/home/dylanr/.ccvault/schedule.log
StandardError=append:/home/dylanr/.ccvault/schedule.log
`
	if got != want {
		t.Errorf("RenderSystemdService() mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestRenderSystemdTimer_IsTheWholeFile(t *testing.T) {
	j := NewJob("/home/dylanr/go/bin/ccvault", "")
	j.Label = "ccvault-sync"

	got, err := RenderSystemdTimer(j)
	if err != nil {
		t.Fatalf("RenderSystemdTimer() error = %v", err)
	}

	want := `[Unit]
Description=Run ccvault-sync.service every 1h0m0s

[Timer]
Unit=ccvault-sync.service
OnBootSec=5min
OnUnitActiveSec=3600s
AccuracySec=1min

[Install]
WantedBy=timers.target
`
	if got != want {
		t.Errorf("RenderSystemdTimer() mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// Type=oneshot is what makes `systemctl --user show -p ExecMainStatus` report
// the sync's exit code — which is fact 3 of `schedule status`. A long-running
// default type would report the service as failed on every normal exit.
func TestRenderSystemdService_IsOneshotAndNotEnableable(t *testing.T) {
	got, err := RenderSystemdService(NewJob("/bin/ccvault", ""))
	if err != nil {
		t.Fatalf("RenderSystemdService() error = %v", err)
	}
	if !strings.Contains(got, "Type=oneshot") {
		t.Error("the service is not Type=oneshot; its exit status would be unreadable")
	}
	if strings.Contains(got, "[Install]") {
		t.Error("the service carries an [Install] section; only the timer is enabled")
	}
}

// systemd expands %h, %n and friends inside a value before anything else
// looks at it, so a '%' in a path has to double or the unit runs a different
// program than the one installed.
func TestRenderSystemdService_DoublesSpecifierPercents(t *testing.T) {
	got, err := RenderSystemdService(NewJob("/home/100%real/bin/ccvault", ""))
	if err != nil {
		t.Fatalf("RenderSystemdService() error = %v", err)
	}
	if !strings.Contains(got, "ExecStart=/home/100%%real/bin/ccvault sync\n") {
		t.Errorf("the '%%' was not doubled; got:\n%s", got)
	}
}

// systemd splits ExecStart on whitespace, so a space in the path has to be
// quoted or the unit runs "/home/my" with "data/bin/ccvault" as argv[1].
func TestRenderSystemdService_QuotesArgumentsWithWhitespace(t *testing.T) {
	got, err := RenderSystemdService(NewJob("/home/my data/bin/ccvault", ""))
	if err != nil {
		t.Fatalf("RenderSystemdService() error = %v", err)
	}
	if !strings.Contains(got, `ExecStart="/home/my data/bin/ccvault" sync`+"\n") {
		t.Errorf("the path with a space was not quoted; got:\n%s", got)
	}
}

func TestSystemdEscape(t *testing.T) {
	cases := map[string]string{
		"/usr/bin/ccvault": "/usr/bin/ccvault",
		"sync":             "sync",
		"":                 `""`,
		"/a b/c":           `"/a b/c"`,
		`/a"b`:             `"/a\"b"`,
		`/a\b`:             `"/a\\b"`,
		"/a%b":             "/a%%b",
		"/a%b c":           `"/a%%b c"`,
		"/a;b":             `"/a;b"`,
		"/a\tb":            "\"/a\tb\"",
	}
	for in, want := range cases {
		if got := systemdEscape(in); got != want {
			t.Errorf("systemdEscape(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRenderSystemdTimer_RendersTheIntervalInSeconds(t *testing.T) {
	j := NewJob("/bin/ccvault", "")
	j.Interval = 15 * time.Minute
	got, err := RenderSystemdTimer(j)
	if err != nil {
		t.Fatalf("RenderSystemdTimer() error = %v", err)
	}
	if !strings.Contains(got, "OnUnitActiveSec=900s\n") {
		t.Errorf("OnUnitActiveSec is not 900s; got:\n%s", got)
	}
}

// Persistent=true only makes OnCalendar catch up. Setting it beside
// OnUnitActiveSec would be a directive that reads like a guarantee and does
// nothing — the exact shape of defect #160 and #170.
func TestRenderSystemdTimer_DoesNotClaimPersistence(t *testing.T) {
	got, err := RenderSystemdTimer(NewJob("/bin/ccvault", ""))
	if err != nil {
		t.Fatalf("RenderSystemdTimer() error = %v", err)
	}
	if strings.Contains(got, "Persistent=") {
		t.Error("the timer sets Persistent=, which does nothing without OnCalendar")
	}
}

func TestRenderSystemdUnits_RefuseAnInvalidJob(t *testing.T) {
	bad := NewJob("/bin/ccvault", "", "--full")
	if _, err := RenderSystemdService(bad); err == nil {
		t.Error("RenderSystemdService() rendered a unit for `sync --full`")
	}
	if _, err := RenderSystemdTimer(bad); err == nil {
		t.Error("RenderSystemdTimer() rendered a timer for `sync --full`")
	}
}

func TestSystemdUnitNames(t *testing.T) {
	if got, want := SystemdServiceName("ccvault-sync"), "ccvault-sync.service"; got != want {
		t.Errorf("SystemdServiceName() = %q, want %q", got, want)
	}
	if got, want := SystemdTimerName("ccvault-sync"), "ccvault-sync.timer"; got != want {
		t.Errorf("SystemdTimerName() = %q, want %q", got, want)
	}
}
