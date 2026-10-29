// ABOUTME: Tests Job validation — the guard that keeps --full and --rebuild out of a scheduled job, and the label out of the filesystem
// ABOUTME: Platform-independent: nothing here touches launchctl, systemctl or ~/Library

package schedule

import (
	"strings"
	"testing"
	"time"
)

func TestJobValidate_AcceptsTheStandardHourlySync(t *testing.T) {
	j := NewJob("/usr/local/bin/ccvault", "/Users/x/.ccvault/schedule.log")
	if err := j.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil for the job install actually registers", err)
	}
	if j.Interval != time.Hour {
		t.Errorf("Interval = %s, want 1h — #171 settled on hourly", j.Interval)
	}
	if got := strings.Join(j.Args, " "); got != "sync" {
		t.Errorf("Args = %q, want %q — the job runs plain sync", got, "sync")
	}
}

// The design's hardest line: "Runs plain ccvault sync. Never --full or
// --rebuild." Encoded as a refusal in the type rather than left to callers,
// because a scheduled --rebuild wipes the archive with nobody at the prompt.
func TestJobValidate_RefusesDestructiveSyncFlags(t *testing.T) {
	for _, arg := range []string{
		"--rebuild",
		"--full",
		"--rebuild=true",
		"--full=true",
		"-rebuild",
		"-full",
	} {
		j := NewJob("/usr/local/bin/ccvault", "", arg)
		err := j.Validate()
		if err == nil {
			t.Errorf("Validate() with %q = nil, want a refusal", arg)
			continue
		}
		if !strings.Contains(err.Error(), "human-initiated") {
			t.Errorf("Validate() with %q = %q, want the refusal to say why", arg, err)
		}
	}
}

func TestJobValidate_AllowsHarmlessSyncFlags(t *testing.T) {
	j := NewJob("/usr/local/bin/ccvault", "", "--source", "claude-code")
	if err := j.Validate(); err != nil {
		t.Errorf("Validate() with --source = %v, want nil; only --full and --rebuild are refused", err)
	}
}

func TestJobValidate_RequiresAnAbsoluteBinaryPath(t *testing.T) {
	for _, path := range []string{"", "ccvault", "./ccvault", "bin/ccvault"} {
		j := NewJob(path, "")
		if err := j.Validate(); err == nil {
			t.Errorf("Validate() with binary path %q = nil, want a refusal — a unit file has no working directory", path)
		}
	}
}

func TestJobValidate_RequiresSyncAsTheSubcommand(t *testing.T) {
	for _, args := range [][]string{nil, {}, {"stats"}, {"--source", "x"}} {
		j := Job{Label: DefaultLabel, BinaryPath: "/bin/ccvault", Args: args, Interval: time.Hour}
		if err := j.Validate(); err == nil {
			t.Errorf("Validate() with args %v = nil, want a refusal", args)
		}
	}
}

// A label becomes a path component — ~/Library/LaunchAgents/<label>.plist and
// ~/.config/systemd/user/<label>.timer. A separator or a traversal in it would
// write a unit file somewhere nobody named.
func TestJobValidate_RefusesALabelThatIsAPath(t *testing.T) {
	for _, label := range []string{
		"",
		"../../etc/cron.hourly/evil",
		"ccvault/sync",
		`ccvault\sync`,
		"ccvault sync",
		".",
		"..",
		"ccvault;sync",
	} {
		j := NewJob("/bin/ccvault", "")
		j.Label = label
		if err := j.Validate(); err == nil {
			t.Errorf("Validate() with label %q = nil, want a refusal", label)
		}
	}
}

func TestJobValidate_AcceptsReverseDNSAndUnitStyleLabels(t *testing.T) {
	for _, label := range []string{DefaultLabel, "ccvault-sync", "ccvault_sync", "ccvault.sync2"} {
		j := NewJob("/bin/ccvault", "")
		j.Label = label
		if err := j.Validate(); err != nil {
			t.Errorf("Validate() with label %q = %v, want nil", label, err)
		}
	}
}

func TestJobValidate_RefusesAnIntervalNoSchedulerCanExpress(t *testing.T) {
	cases := map[string]time.Duration{
		"zero":              0,
		"negative":          -time.Hour,
		"sub-second":        500 * time.Millisecond,
		"fractional second": time.Hour + 500*time.Millisecond,
	}
	for name, interval := range cases {
		j := NewJob("/bin/ccvault", "")
		j.Interval = interval
		if err := j.Validate(); err == nil {
			t.Errorf("Validate() with a %s interval (%s) = nil, want a refusal — StartInterval and OnUnitActiveSec are both integral seconds", name, interval)
		}
	}
}

func TestJobValidate_RefusesARelativeLogPath(t *testing.T) {
	j := NewJob("/bin/ccvault", "schedule.log")
	if err := j.Validate(); err == nil {
		t.Error("Validate() with a relative log path = nil, want a refusal")
	}
}

func TestFlagName(t *testing.T) {
	cases := []struct {
		arg  string
		name string
		ok   bool
	}{
		{"--rebuild", "rebuild", true},
		{"--rebuild=true", "rebuild", true},
		{"-r", "r", true},
		{"sync", "", false},
		{"", "", false},
		{"-", "", false},
		{"--", "", false},
	}
	for _, c := range cases {
		name, ok := flagName(c.arg)
		if name != c.name || ok != c.ok {
			t.Errorf("flagName(%q) = (%q, %v), want (%q, %v)", c.arg, name, ok, c.name, c.ok)
		}
	}
}
