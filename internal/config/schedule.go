// ABOUTME: The [schedule] config table — whether stats nags about staleness, and whether quickstart's offer was declined
// ABOUTME: Both live in config.toml on purpose, so the state that suppresses a prompt is a line you can read and delete

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ScheduleConfig is the [schedule] table.
//
// Deliberately in the config file rather than in a dotfile or a database row.
// #171's phrasing for the notice is "suppressible via config so the dismissal
// is inspectable rather than buried in hidden state", and the same argument
// applies to the declined flag: a prompt that stops appearing for a reason
// you cannot find is worse than one that keeps appearing.
type ScheduleConfig struct {
	// Notify controls the staleness notice in `ccvault stats`. Default true;
	// set notify = false to silence it.
	Notify bool `mapstructure:"notify"`

	// Declined records that quickstart's offer to install the schedule was
	// turned down, so it is not asked again. Delete the line to be asked
	// again.
	Declined bool `mapstructure:"declined"`
}

// ScheduleNotifyKey and ScheduleDeclinedKey are the viper keys for the two
// settings, named here so the defaults and the writer cannot drift apart.
const (
	ScheduleNotifyKey   = "schedule.notify"
	ScheduleDeclinedKey = "schedule.declined"
)

// ConfigFileName is the file name searched for on the config path.
const ConfigFileName = "config.toml"

// WritableConfigPath is the file a setting should be written to: the one that
// was actually read, or config.toml in the data directory when there was none.
//
// Never a guess at the default path: a run pointed at a throwaway archive must
// write its own config file there and not touch the real one.
func (c *Config) WritableConfigPath() string {
	if c.ConfigFile != "" {
		return c.ConfigFile
	}
	return filepath.Join(c.DataDir, ConfigFileName)
}

// RememberScheduleDeclined records that the scheduled-sync offer was turned
// down, by writing declined = true into the [schedule] table.
//
// Edits the file in place rather than re-serialising it. Viper's WriteConfig
// would round-trip the whole document through its own encoder and drop every
// comment in it — including the ones a user wrote to explain their own
// [[sources]] list. A config file this tool rewrites destructively is one
// nobody will hand-edit again.
func (c *Config) RememberScheduleDeclined() error {
	return c.setScheduleSetting(ScheduleDeclinedKey, true)
}

// setScheduleSetting reads the config file, applies one [schedule] key, and
// writes it back.
func (c *Config) setScheduleSetting(key string, value bool) error {
	path := c.WritableConfigPath()
	short := strings.TrimPrefix(key, "schedule.")

	existing, err := os.ReadFile(path) //nolint:gosec // G304: path is the config file this run resolved
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}

	updated := setScheduleBool(string(existing), short, value)
	if updated == string(existing) {
		// Already says what we want; writing would only churn the mtime.
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	// #nosec G703 -- path is WritableConfigPath(): either the config file this
	// run actually read (viper's ConfigFileUsed) or config.toml inside the
	// data directory this run resolved. Both are the user's own declaration of
	// where ccvault's files live, which is the same category G304 is disabled
	// across this repo for. Writing anywhere else would be the bug; writing
	// here is the whole point.
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// scheduleTableHeader is the TOML table these settings live in.
const scheduleTableHeader = "[schedule]"

// setScheduleBool returns text with one key in the [schedule] table set to
// value, adding the table when it is not there.
//
// Pure, and the only place the file format is decided, so every shape a real
// config file can be in is a test case: no file at all, a file with no
// [schedule] table, one with the table and the key, one with the table and a
// different key, and one where the table is last in the file.
//
// Deliberately a targeted edit and not a parse-and-re-emit. Anything a user
// wrote — comments, key order, their own spacing — survives untouched, which
// is the difference between a config file they will keep editing and one they
// will stop trusting.
func setScheduleBool(text, key string, value bool) string {
	rendered := key + " = " + strconv.FormatBool(value)
	lines := strings.Split(text, "\n")

	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == scheduleTableHeader {
			start = i
			break
		}
	}

	// No [schedule] table: append one, with a note saying what the keys mean.
	// The note is the point of keeping this state in the config file at all.
	if start < 0 {
		var b strings.Builder
		b.WriteString(strings.TrimRight(text, "\n"))
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("# Scheduled background sync (`ccvault schedule`).\n")
		b.WriteString("#   notify   = false stops `ccvault stats` mentioning a stale archive.\n")
		b.WriteString("#   declined = true stops `ccvault quickstart` offering to install the\n")
		b.WriteString("#              schedule. Delete the line to be asked again.\n")
		b.WriteString(scheduleTableHeader + "\n")
		b.WriteString(rendered + "\n")
		return b.String()
	}

	// The table ends at the next table header, or at the end of the file.
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if trimmed := strings.TrimSpace(lines[i]); strings.HasPrefix(trimmed, "[") {
			end = i
			break
		}
	}

	// Replace the key in place if it is already there, so a hand-written
	// comment above it stays above it.
	for i := start + 1; i < end; i++ {
		if scheduleKeyOnLine(lines[i]) == key {
			lines[i] = rendered
			return strings.Join(lines, "\n")
		}
	}

	// Otherwise insert it directly under the header, where it is easiest to
	// find.
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:start+1]...)
	out = append(out, rendered)
	out = append(out, lines[start+1:]...)
	return strings.Join(out, "\n")
}

// scheduleKeyOnLine returns the bare key a TOML assignment line sets, or ""
// for a comment, a blank line or anything else.
func scheduleKeyOnLine(line string) string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return ""
	}
	name, _, found := strings.Cut(trimmed, "=")
	if !found {
		return ""
	}
	// Quoted keys are legal TOML; unquoting here keeps `"notify" = false`
	// from being treated as a different key than `notify = false`.
	return strings.Trim(strings.TrimSpace(name), `"'`)
}
