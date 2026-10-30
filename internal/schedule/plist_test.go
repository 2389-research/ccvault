// ABOUTME: Tests the launchd plist text byte for byte, including XML escaping of paths
// ABOUTME: Pure — runs and asserts identically on macOS, Linux and Windows

package schedule

import (
	"encoding/xml"
	"strings"
	"testing"
	"time"
)

func TestRenderLaunchdPlist_IsTheWholeFile(t *testing.T) {
	j := NewJob("/Users/dylanr/.local/share/mise/installs/go/1.25.5/bin/ccvault", "/Users/dylanr/.ccvault/schedule.log")

	got, err := RenderLaunchdPlist(j)
	if err != nil {
		t.Fatalf("RenderLaunchdPlist() error = %v", err)
	}

	want := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>ai.2389.ccvault.sync</string>
	<key>ProgramArguments</key>
	<array>
		<string>/Users/dylanr/.local/share/mise/installs/go/1.25.5/bin/ccvault</string>
		<string>sync</string>
	</array>
	<key>StartInterval</key>
	<integer>3600</integer>
	<key>StandardOutPath</key>
	<string>/Users/dylanr/.ccvault/schedule.log</string>
	<key>StandardErrorPath</key>
	<string>/Users/dylanr/.ccvault/schedule.log</string>
	<key>LowPriorityIO</key>
	<true/>
	<key>Nice</key>
	<integer>5</integer>
</dict>
</plist>
`
	if got != want {
		t.Errorf("RenderLaunchdPlist() mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// A plist launchd cannot parse is a job that silently never exists. The
// author's own GOBIN path is harmless, but a home directory containing '&' is
// not, and nothing downstream would report the difference.
func TestRenderLaunchdPlist_EscapesXMLInPaths(t *testing.T) {
	j := NewJob(`/Users/a&b/<bin>/ccvault`, `/Users/a&b/"log".txt`)

	got, err := RenderLaunchdPlist(j)
	if err != nil {
		t.Fatalf("RenderLaunchdPlist() error = %v", err)
	}

	if strings.Contains(got, "/Users/a&b/<bin>") {
		t.Error("the raw '&' and '<' reached the plist unescaped")
	}
	if !strings.Contains(got, "/Users/a&amp;b/&lt;bin&gt;/ccvault") {
		t.Errorf("binary path was not XML-escaped; got:\n%s", got)
	}

	// The real assertion: an XML parser can read it back and the path it
	// yields is the path we asked for.
	var parsed struct {
		Dict struct {
			Values []string `xml:"array>string"`
		} `xml:"dict"`
	}
	if err := xml.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("the rendered plist is not well-formed XML: %v", err)
	}
	if len(parsed.Dict.Values) != 2 || parsed.Dict.Values[0] != `/Users/a&b/<bin>/ccvault` {
		t.Errorf("ProgramArguments round-tripped as %q, want the original path back", parsed.Dict.Values)
	}
}

func TestRenderLaunchdPlist_OmitsLogKeysWhenThereIsNoLog(t *testing.T) {
	j := NewJob("/bin/ccvault", "")
	got, err := RenderLaunchdPlist(j)
	if err != nil {
		t.Fatalf("RenderLaunchdPlist() error = %v", err)
	}
	for _, key := range []string{"StandardOutPath", "StandardErrorPath"} {
		if strings.Contains(got, key) {
			t.Errorf("plist names %s with no log path configured", key)
		}
	}
}

// RunAtLoad is absent on purpose: an agent that syncs on every login does
// work nobody asked for, at the moment the machine is busiest.
func TestRenderLaunchdPlist_DoesNotRunAtLoad(t *testing.T) {
	got, err := RenderLaunchdPlist(NewJob("/bin/ccvault", ""))
	if err != nil {
		t.Fatalf("RenderLaunchdPlist() error = %v", err)
	}
	if strings.Contains(got, "RunAtLoad") {
		t.Error("plist sets RunAtLoad; install runs its own sync instead")
	}
	if strings.Contains(got, "StartCalendarInterval") {
		t.Error("plist uses StartCalendarInterval; StartInterval is what fires a missed tick after a wake")
	}
}

func TestRenderLaunchdPlist_CarriesExtraArgsInOrder(t *testing.T) {
	j := NewJob("/bin/ccvault", "", "--data-dir", "/tmp/fixture")
	got, err := RenderLaunchdPlist(j)
	if err != nil {
		t.Fatalf("RenderLaunchdPlist() error = %v", err)
	}
	want := "\t\t<string>/bin/ccvault</string>\n" +
		"\t\t<string>sync</string>\n" +
		"\t\t<string>--data-dir</string>\n" +
		"\t\t<string>/tmp/fixture</string>\n"
	if !strings.Contains(got, want) {
		t.Errorf("ProgramArguments lost or reordered the extra args; got:\n%s", got)
	}
}

func TestRenderLaunchdPlist_RendersTheIntervalInSeconds(t *testing.T) {
	j := NewJob("/bin/ccvault", "")
	j.Interval = 90 * time.Minute
	got, err := RenderLaunchdPlist(j)
	if err != nil {
		t.Fatalf("RenderLaunchdPlist() error = %v", err)
	}
	if !strings.Contains(got, "<integer>5400</integer>") {
		t.Errorf("StartInterval is not 5400 seconds; got:\n%s", got)
	}
}

// Generation refuses what registration would refuse, so a bad job never
// becomes a file on disk.
func TestRenderLaunchdPlist_RefusesAnInvalidJob(t *testing.T) {
	if _, err := RenderLaunchdPlist(NewJob("/bin/ccvault", "", "--rebuild")); err == nil {
		t.Error("RenderLaunchdPlist() rendered a plist for `sync --rebuild`")
	}
	if _, err := RenderLaunchdPlist(NewJob("relative", "")); err == nil {
		t.Error("RenderLaunchdPlist() rendered a plist for a relative binary path")
	}
}

func TestLaunchdPlistName(t *testing.T) {
	if got, want := LaunchdPlistName(DefaultLabel), "ai.2389.ccvault.sync.plist"; got != want {
		t.Errorf("LaunchdPlistName() = %q, want %q", got, want)
	}
}
