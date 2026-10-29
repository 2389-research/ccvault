// ABOUTME: Renders the systemd user .service and .timer units for a Job — pure text, no filesystem, no systemctl
// ABOUTME: Separated from systemd.go so the files' exact content is testable on any platform

package schedule

import (
	"fmt"
	"strings"
)

// SystemdServiceName and SystemdTimerName are the unit filenames a label
// takes in ~/.config/systemd/user.
//
// The label is used verbatim as the unit stem. Validate has already confined
// it to characters a unit name accepts.
func SystemdServiceName(label string) string { return label + ".service" }

// SystemdTimerName is the timer unit paired with SystemdServiceName.
func SystemdTimerName(label string) string { return label + ".timer" }

// RenderSystemdService returns the oneshot service unit that runs the sync.
//
// Type=oneshot, so systemd treats the process exiting as the unit having
// finished rather than as a crash, and so `systemctl --user show` reports its
// exit status — which is how `schedule status` learns whether the last run
// succeeded.
//
// No [Install] section: the service is started by the timer, never enabled on
// its own. Enabling it would mean "run a sync at every boot", which is not
// what was asked for.
func RenderSystemdService(j Job) (string, error) {
	if err := j.Validate(); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("[Unit]\n")
	b.WriteString("Description=ccvault incremental conversation archive sync\n")
	b.WriteString("Documentation=https://github.com/2389-research/ccvault\n")
	b.WriteString("\n")
	b.WriteString("[Service]\n")
	b.WriteString("Type=oneshot\n")
	fmt.Fprintf(&b, "ExecStart=%s\n", systemdExecLine(append([]string{j.BinaryPath}, j.Args...)))
	// An hourly archival sync has no business competing with interactive work.
	b.WriteString("Nice=5\n")
	b.WriteString("IOSchedulingClass=idle\n")
	if j.LogPath != "" {
		fmt.Fprintf(&b, "StandardOutput=append:%s\n", systemdEscape(j.LogPath))
		fmt.Fprintf(&b, "StandardError=append:%s\n", systemdEscape(j.LogPath))
	}
	return b.String(), nil
}

// RenderSystemdTimer returns the timer unit that drives the service.
//
// OnBootSec plus OnUnitActiveSec rather than OnCalendar: the interval is a
// duration, and expressing a duration as a calendar expression only works for
// the ones that divide an hour evenly.
//
// The cost of that choice is that Persistent=true would do nothing here — it
// only makes OnCalendar catch up a missed elapse — so it is not set. A machine
// asleep across a tick resumes on the next one rather than firing immediately
// on wake, and the gap shows up as archive staleness in `schedule status`.
// That is the honest trade, and it is why staleness is reported from
// source_files.synced_at rather than inferred from the timer.
func RenderSystemdTimer(j Job) (string, error) {
	if err := j.Validate(); err != nil {
		return "", err
	}
	seconds := int64(j.Interval.Seconds())
	var b strings.Builder
	b.WriteString("[Unit]\n")
	fmt.Fprintf(&b, "Description=Run %s every %s\n", SystemdServiceName(j.Label), j.Interval)
	b.WriteString("\n")
	b.WriteString("[Timer]\n")
	fmt.Fprintf(&b, "Unit=%s\n", SystemdServiceName(j.Label))
	// Not zero: a sync racing the rest of login is the one tick guaranteed to
	// land while the machine is busiest.
	b.WriteString("OnBootSec=5min\n")
	fmt.Fprintf(&b, "OnUnitActiveSec=%ds\n", seconds)
	// Lets systemd coalesce this timer with others rather than waking the
	// machine for it alone.
	b.WriteString("AccuracySec=1min\n")
	b.WriteString("\n")
	b.WriteString("[Install]\n")
	b.WriteString("WantedBy=timers.target\n")
	return b.String(), nil
}

// systemdExecLine renders an argv as an ExecStart value.
//
// systemd splits ExecStart on whitespace and honours backslash escapes, so an
// argument containing a space has to be quoted. Quoting every argument that
// needs it rather than all of them keeps the common line readable, which
// matters because this file is something a user is expected to read.
func systemdExecLine(argv []string) string {
	parts := make([]string, len(argv))
	for i, arg := range argv {
		parts[i] = systemdEscape(arg)
	}
	return strings.Join(parts, " ")
}

// systemdEscape renders a single word as a unit-file value.
//
// Two separate escapes, in this order:
//
//   - '%' doubles, because systemd expands %h, %i, %n and friends in a value
//     before it ever looks at quoting. A home directory with a '%' in it is
//     unusual but a path is not ours to assume about, and the failure mode is
//     a unit that runs a different program than the one installed.
//   - whitespace and quote characters force the whole word into double quotes
//     with backslash escaping, the form systemd documents.
//
// Words needing neither are returned as-is: this file is meant to be read by
// the person whose machine it runs on, and quoting every argument hides the
// command behind punctuation.
func systemdEscape(s string) string {
	if s == "" {
		return `""`
	}
	s = strings.ReplaceAll(s, "%", "%%")
	if !strings.ContainsAny(s, " \t\n\"'\\;") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		if r == '"' || r == '\\' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}
