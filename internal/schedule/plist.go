// ABOUTME: Renders a launchd LaunchAgent plist for a Job — pure text, no filesystem, no launchctl
// ABOUTME: Separated from launchd.go so the file's exact content is testable on any platform

package schedule

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"strings"
)

// RenderLaunchdPlist returns the LaunchAgent plist that runs this job.
//
// Pure by design. Registration needs a Mac; the bytes that get registered do
// not, and the bytes are where a mistake is silent — a plist launchd rejects
// at bootstrap time is at least loud.
//
// StartInterval rather than StartCalendarInterval: launchd fires a missed
// StartInterval once the machine wakes, which is what an archival sync wants.
// A calendar entry would skip the tick outright.
//
// RunAtLoad is deliberately absent. The agent loads at login, and syncing
// every time the user logs in is work nobody asked for; the first tick comes
// an interval later. `install` runs one sync itself so the archive is not
// left waiting for it.
func RenderLaunchdPlist(j Job) (string, error) {
	if err := j.Validate(); err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n")
	b.WriteString("<dict>\n")

	b.WriteString("\t<key>Label</key>\n")
	fmt.Fprintf(&b, "\t<string>%s</string>\n", plistEscape(j.Label))

	b.WriteString("\t<key>ProgramArguments</key>\n")
	b.WriteString("\t<array>\n")
	for _, arg := range append([]string{j.BinaryPath}, j.Args...) {
		fmt.Fprintf(&b, "\t\t<string>%s</string>\n", plistEscape(arg))
	}
	b.WriteString("\t</array>\n")

	b.WriteString("\t<key>StartInterval</key>\n")
	fmt.Fprintf(&b, "\t<integer>%d</integer>\n", int64(j.Interval.Seconds()))

	if j.LogPath != "" {
		b.WriteString("\t<key>StandardOutPath</key>\n")
		fmt.Fprintf(&b, "\t<string>%s</string>\n", plistEscape(j.LogPath))
		b.WriteString("\t<key>StandardErrorPath</key>\n")
		fmt.Fprintf(&b, "\t<string>%s</string>\n", plistEscape(j.LogPath))
	}

	// Nice to the rest of the machine: an hourly archival sync has no reason
	// to compete with whatever the user is actually doing.
	b.WriteString("\t<key>LowPriorityIO</key>\n")
	b.WriteString("\t<true/>\n")
	b.WriteString("\t<key>Nice</key>\n")
	b.WriteString("\t<integer>5</integer>\n")

	b.WriteString("</dict>\n")
	b.WriteString("</plist>\n")
	return b.String(), nil
}

// plistEscape escapes a string for an XML text node.
//
// xml.EscapeText rather than a hand-rolled replacer: a path can legitimately
// contain '&' or '<', and getting the escape wrong produces a plist launchd
// silently declines to load.
func plistEscape(s string) string {
	var buf bytes.Buffer
	// EscapeText's only error path is the writer's, and bytes.Buffer has none.
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}

// LaunchdPlistName is the filename a label's plist takes in
// ~/Library/LaunchAgents.
func LaunchdPlistName(label string) string {
	return label + ".plist"
}
