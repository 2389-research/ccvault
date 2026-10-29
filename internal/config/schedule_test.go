// ABOUTME: Tests the [schedule] table — its defaults, and the in-place TOML edit that remembers a declined offer
// ABOUTME: setScheduleBool is pure, so every shape a real config file can be in is a case here

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// notify defaults on: the notice is silent unless something is wrong, and
// someone who has never run `ccvault schedule` is exactly who it is for.
func TestLoad_ScheduleNotifyDefaultsOn(t *testing.T) {
	cfg, err := LoadWith(Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("LoadWith: %v", err)
	}
	if !cfg.Schedule.Notify {
		t.Error("Schedule.Notify = false with no config file; the staleness notice must be on by default")
	}
	if cfg.Schedule.Declined {
		t.Error("Schedule.Declined = true with no config file")
	}
}

func TestLoad_ScheduleTableIsRead(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, ConfigFileName), "[schedule]\nnotify = false\ndeclined = true\n")

	cfg, err := LoadWith(Options{DataDir: dir})
	if err != nil {
		t.Fatalf("LoadWith: %v", err)
	}
	if cfg.Schedule.Notify {
		t.Error("Schedule.Notify = true despite notify = false in the file")
	}
	if !cfg.Schedule.Declined {
		t.Error("Schedule.Declined = false despite declined = true in the file")
	}
}

// A setting written back has to go to the file that was read. A run pointed
// at a throwaway archive must write its own config file there, not the real
// one — agents do that dozens of times a session.
func TestWritableConfigPath_IsTheFileThatWasRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ConfigFileName)
	write(t, path, "[schedule]\nnotify = false\n")

	cfg, err := LoadWith(Options{DataDir: dir})
	if err != nil {
		t.Fatalf("LoadWith: %v", err)
	}
	if cfg.ConfigFile != path {
		t.Errorf("ConfigFile = %q, want %q", cfg.ConfigFile, path)
	}
	if got := cfg.WritableConfigPath(); got != path {
		t.Errorf("WritableConfigPath() = %q, want %q", got, path)
	}
}

func TestWritableConfigPath_FallsBackToTheDataDirectory(t *testing.T) {
	dir := t.TempDir()
	cfg, err := LoadWith(Options{DataDir: dir})
	if err != nil {
		t.Fatalf("LoadWith: %v", err)
	}
	if cfg.ConfigFile != "" {
		t.Errorf("ConfigFile = %q, want empty when no file was found", cfg.ConfigFile)
	}
	if got, want := cfg.WritableConfigPath(), filepath.Join(dir, ConfigFileName); got != want {
		t.Errorf("WritableConfigPath() = %q, want %q", got, want)
	}
}

func TestRememberScheduleDeclined_CreatesTheFileAndReadsBack(t *testing.T) {
	dir := t.TempDir()
	cfg, err := LoadWith(Options{DataDir: dir})
	if err != nil {
		t.Fatalf("LoadWith: %v", err)
	}

	if err := cfg.RememberScheduleDeclined(); err != nil {
		t.Fatalf("RememberScheduleDeclined: %v", err)
	}

	// The real assertion: a fresh Load sees it. A write that does not survive
	// a reload would make the offer repeat forever.
	reloaded, err := LoadWith(Options{DataDir: dir})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !reloaded.Schedule.Declined {
		t.Error("Schedule.Declined = false after RememberScheduleDeclined; the offer would be made again")
	}
	// And it must not have turned the notice off as a side effect.
	if !reloaded.Schedule.Notify {
		t.Error("Schedule.Notify = false after declining; declining the schedule is not declining the notice")
	}
}

// The state has to be findable. "Inspectable rather than hidden" is the
// reason it is in config.toml and not in a dotfile.
func TestRememberScheduleDeclined_WritesSomethingAUserCanRead(t *testing.T) {
	dir := t.TempDir()
	cfg, err := LoadWith(Options{DataDir: dir})
	if err != nil {
		t.Fatalf("LoadWith: %v", err)
	}
	if err := cfg.RememberScheduleDeclined(); err != nil {
		t.Fatalf("RememberScheduleDeclined: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, ConfigFileName)) //nolint:gosec // G304: t.TempDir()
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	text := string(raw)
	for _, want := range []string{"[schedule]", "declined = true", "Delete the line to be asked again"} {
		if !strings.Contains(text, want) {
			t.Errorf("the written config does not contain %q:\n%s", want, text)
		}
	}
}

// Nothing a user wrote may be lost. A tool that destroys comments on write is
// one nobody hand-edits again, which is why this is a targeted edit and not
// viper.WriteConfig.
func TestSetScheduleBool_PreservesEverythingElse(t *testing.T) {
	before := `# my archive lives on the big disk
data_dir = "/mnt/big/ccvault"

# the only tree worth indexing
[[sources]]
name = "claude-code"
type = "claude-code"
path = "/Users/d/.claude"
`
	after := setScheduleBool(before, "declined", true)

	for _, want := range []string{
		"# my archive lives on the big disk",
		`data_dir = "/mnt/big/ccvault"`,
		"# the only tree worth indexing",
		"[[sources]]",
		`path = "/Users/d/.claude"`,
	} {
		if !strings.Contains(after, want) {
			t.Errorf("the edit lost %q:\n%s", want, after)
		}
	}
	if !strings.Contains(after, "[schedule]\ndeclined = true\n") {
		t.Errorf("the [schedule] table was not appended:\n%s", after)
	}
}

func TestSetScheduleBool_EmptyFile(t *testing.T) {
	got := setScheduleBool("", "declined", true)
	if !strings.Contains(got, "[schedule]\ndeclined = true\n") {
		t.Errorf("setScheduleBool(\"\") = %q", got)
	}
	if strings.HasPrefix(got, "\n") {
		t.Errorf("a config written from nothing starts with a blank line:\n%q", got)
	}
}

// The table is there and the key is there: replace the value where it sits,
// so a comment the user wrote above it stays above it.
func TestSetScheduleBool_ReplacesTheKeyInPlace(t *testing.T) {
	before := `[schedule]
# I turned this off on purpose
notify = false
declined = false
`
	after := setScheduleBool(before, "declined", true)

	if !strings.Contains(after, "declined = true") {
		t.Errorf("the key was not updated:\n%s", after)
	}
	if strings.Contains(after, "declined = false") {
		t.Errorf("the old value survived alongside the new one:\n%s", after)
	}
	if !strings.Contains(after, "# I turned this off on purpose\nnotify = false") {
		t.Errorf("the user's comment moved away from the key it describes:\n%s", after)
	}
	if strings.Count(after, "[schedule]") != 1 {
		t.Errorf("a second [schedule] table was added:\n%s", after)
	}
}

func TestSetScheduleBool_AddsTheKeyToAnExistingTable(t *testing.T) {
	before := "[schedule]\nnotify = false\n"
	after := setScheduleBool(before, "declined", true)

	if !strings.Contains(after, "declined = true") {
		t.Errorf("the key was not added:\n%s", after)
	}
	if !strings.Contains(after, "notify = false") {
		t.Errorf("the existing key was lost:\n%s", after)
	}
	if strings.Count(after, "[schedule]") != 1 {
		t.Errorf("a second [schedule] table was added:\n%s", after)
	}
}

// The key belongs to [schedule] and nothing else. A `declined` under some
// other table must not be mistaken for it, and the edit must not land inside
// that table either.
func TestSetScheduleBool_IgnoresASimilarKeyInAnotherTable(t *testing.T) {
	before := `[other]
declined = false

[schedule]
notify = true
`
	after := setScheduleBool(before, "declined", true)

	if !strings.Contains(after, "[other]\ndeclined = false") {
		t.Errorf("the other table's key was rewritten:\n%s", after)
	}
	if !strings.Contains(after, "[schedule]\ndeclined = true") {
		t.Errorf("the key did not land in [schedule]:\n%s", after)
	}
}

// A [schedule] table followed by another table bounds the search. Without
// the bound, the key after it would be found and rewritten.
func TestSetScheduleBool_StopsAtTheNextTableHeader(t *testing.T) {
	before := `[schedule]
notify = true

[[sources]]
declined = false
`
	after := setScheduleBool(before, "declined", true)

	if !strings.Contains(after, "[[sources]]\ndeclined = false") {
		t.Errorf("the sources table's key was rewritten:\n%s", after)
	}
	if !strings.Contains(after, "[schedule]\ndeclined = true") {
		t.Errorf("the key did not land in [schedule]:\n%s", after)
	}
}

func TestSetScheduleBool_IsIdempotent(t *testing.T) {
	once := setScheduleBool("", "declined", true)
	twice := setScheduleBool(once, "declined", true)
	if once != twice {
		t.Errorf("a second identical edit changed the file:\n--- once ---\n%s\n--- twice ---\n%s", once, twice)
	}
}

func TestSetScheduleBool_CanSetFalse(t *testing.T) {
	got := setScheduleBool("[schedule]\ndeclined = true\n", "declined", false)
	if !strings.Contains(got, "declined = false") {
		t.Errorf("setScheduleBool(..., false) = %q", got)
	}
}

// A commented-out key is not a key. Rewriting the comment would leave the
// setting unset while looking set.
func TestSetScheduleBool_DoesNotRewriteACommentedKey(t *testing.T) {
	before := "[schedule]\n# declined = true\n"
	after := setScheduleBool(before, "declined", true)

	if !strings.Contains(after, "# declined = true") {
		t.Errorf("the commented line was rewritten:\n%s", after)
	}
	if !strings.Contains(after, "[schedule]\ndeclined = true") {
		t.Errorf("a real key was not added:\n%s", after)
	}
}

func TestScheduleKeyOnLine(t *testing.T) {
	cases := map[string]string{
		"declined = true":   "declined",
		"  notify=false":    "notify",
		`"notify" = false`:  "notify",
		"# declined = true": "",
		"":                  "",
		"[schedule]":        "",
		"not an assignment": "",
	}
	for line, want := range cases {
		if got := scheduleKeyOnLine(line); got != want {
			t.Errorf("scheduleKeyOnLine(%q) = %q, want %q", line, got, want)
		}
	}
}

// Writing a setting that is already set must not churn the file's mtime —
// `ccvault stats` runs often, and a config file whose mtime moves on every
// run looks like something is editing it.
func TestSetScheduleSetting_NoWriteWhenAlreadySet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ConfigFileName)
	write(t, path, "[schedule]\ndeclined = true\n")

	cfg, err := LoadWith(Options{DataDir: dir})
	if err != nil {
		t.Fatalf("LoadWith: %v", err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	if err := cfg.RememberScheduleDeclined(); err != nil {
		t.Fatalf("RememberScheduleDeclined: %v", err)
	}

	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("the config file was rewritten even though the setting already said what we wanted")
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
