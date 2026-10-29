// ABOUTME: The `ccvault schedule install | status | uninstall` commands — hourly background sync, and the report on whether it is actually working
// ABOUTME: status is the centre of this: it reports what the OS, the filesystem, PATH and the archive say, and lets them disagree out loud

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/2389-research/ccvault/internal/config"
	"github.com/2389-research/ccvault/internal/schedule"
	"github.com/spf13/cobra"
)

// ScheduleLogName is the file the registered job appends its output to,
// inside the data directory.
//
// Inside the data directory and not a system log location because that is
// where everything else ccvault writes lives, and because `schedule status`
// can then name one path the user can read without privileges.
const ScheduleLogName = "schedule.log"

var scheduleCmd = &cobra.Command{
	Use:   "schedule",
	Short: "Run ccvault sync automatically, hourly, in the background",
	Long: `Register an hourly background sync with this machine's own scheduler, and
report on whether it is actually working.

ccvault only archives what it has seen. Claude Code keeps transcripts on
disk for roughly 38 days, so a month of not running sync is a month that
cannot be recovered afterwards. The point of this command is that archive
completeness stops depending on remembering.

  ccvault schedule install     register the hourly job
  ccvault schedule status      report what is registered and whether it works
  ccvault schedule uninstall   remove it

The job is a user-level unit and never needs sudo: a launchd LaunchAgent on
macOS, a systemd --user timer on Linux. It runs plain "ccvault sync" and
never "sync --full" or "sync --rebuild", both of which are expensive or
destructive and stay human-initiated.

status is the part worth running. It reports reality rather than intent --
four facts, each checked against the system rather than against a config
file:

  1. whether a unit is actually registered with the OS
  2. whether the registered binary still exists, and is still the ccvault
     on your PATH
  3. when the job last ran, and whether it succeeded
  4. how far behind the archive is (source_files.synced_at versus now)

Fact 2 is the one that bites. A unit hardcodes a program path, and some
install routes move: a GOBIN under mise embeds the Go version, so bumping
Go leaves the unit running a ccvault that no longer matches the archive.
That binary will refuse to write rather than corrupt anything -- and a
background job's refusal has no audience unless something reports it.

Scheduling is implemented on macOS and Linux. Everywhere else these
commands say so rather than pretending.`,
}

var scheduleInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Register the hourly background sync",
	// A refusal (unsupported platform, destructive flags, a scheduler that
	// declined) is a verdict, not a misuse of the command.
	SilenceUsage: true,
	Long: `Register an hourly "ccvault sync" with this machine's scheduler.

Idempotent: installing twice leaves one unit. The binary path is resolved
now, from the ccvault you are running, and written into the unit -- there is
no shim. If that is not the ccvault on your PATH, install says so, because
the schedule would then be running a different build than you do.

The first run happens one interval from now, not immediately. Installing
does not sync; run "ccvault sync" if you want the archive current today.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		label, _ := cmd.Flags().GetString("label")

		manager, err := schedule.New()
		if err != nil {
			return err
		}

		cfg, err := loadConfig(cmd)
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}

		binary, err := scheduleBinaryPath()
		if err != nil {
			return err
		}

		logPath := filepath.Join(cfg.DataDir, ScheduleLogName)
		job := schedule.NewJob(binary, logPath, scheduleExtraArgs(cmd)...)
		job.Label = label

		if err := manager.Install(job); err != nil {
			return err
		}

		reg, err := manager.Inspect(label)
		if err != nil {
			return fmt.Errorf("read back the registration: %w", err)
		}

		fmt.Printf("Registered an hourly ccvault sync with %s.\n\n", manager.Platform())
		fmt.Printf("  Label:     %s\n", label)
		if reg.UnitPath != "" {
			fmt.Printf("  Unit:      %s\n", reg.UnitPath)
		}
		fmt.Printf("  Runs:      %s %s\n", binary, joinArgs(job.Args))
		fmt.Printf("  Every:     %s\n", job.Interval)
		fmt.Printf("  Output:    %s\n", logPath)
		fmt.Println()

		// Said at install time, not left for status to discover: if the
		// schedule is pinned to a different build than the user runs, this is
		// the moment it is cheapest to fix.
		if onPath, lookErr := exec.LookPath("ccvault"); lookErr == nil {
			if resolveOrEmpty(onPath) != resolveOrEmpty(binary) {
				fmt.Printf("Note: the ccvault on your PATH is %s, which is NOT the binary just\n", onPath)
				fmt.Printf("registered. The schedule will run %s.\n", binary)
				fmt.Println("Re-run `schedule install` with the binary you mean to keep using.")
				fmt.Println()
			}
		}

		fmt.Printf("The first run is in about %s. Run `ccvault sync` now if you want the\n", job.Interval)
		fmt.Println("archive current today, and `ccvault schedule status` to check on it later.")
		return nil
	},
}

var scheduleStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Report whether the scheduled sync is registered and working",
	// A failed check is a verdict, not a misuse of the command.
	SilenceUsage: true,
	Long: `Report the four facts that say whether a scheduled sync is actually working:
whether a unit is registered with the OS, whether its binary still exists
and still matches the ccvault on your PATH, when it last ran and whether
that run succeeded, and how far behind the archive is.

Everything here is read from the system, never from a config file. Config
holds intent; this holds reality, and the two must be able to disagree out
loud.

Exits non-zero when anything is wrong -- including when nothing is
registered -- so "ccvault schedule status || ccvault schedule install" does
the right thing in a script.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		label, _ := cmd.Flags().GetString("label")
		asJSON, _ := cmd.Flags().GetBool("json")

		report, err := scheduleReport(cmd, label)
		if err != nil {
			return err
		}

		if asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			if err := enc.Encode(scheduleStatusJSON(report)); err != nil {
				return err
			}
		} else {
			printScheduleStatus(report)
		}

		if !report.Healthy() {
			// The verdict has been printed; this is what makes it a verdict
			// rather than prose. Same reasoning as `stats --strict` (#106):
			// a check whose result only exists in the output is one a script
			// and an agent both miss.
			return fmt.Errorf("%d problem(s) found with the scheduled sync", len(report.Problems))
		}
		return nil
	},
}

var scheduleUninstallCmd = &cobra.Command{
	Use:          "uninstall",
	Short:        "Remove the scheduled sync",
	SilenceUsage: true,
	Long: `Remove the registered job and delete its unit file.

Both, always: a launchd plist left in ~/Library/LaunchAgents loads again at
the next login, so removing only the registration uninstalls the job until
you reboot.

Finding nothing to remove is not an error -- the postcondition is the same
either way.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		label, _ := cmd.Flags().GetString("label")

		manager, err := schedule.New()
		if err != nil {
			return err
		}

		err = manager.Uninstall(label)
		switch {
		case errors.Is(err, schedule.ErrNotRegistered):
			fmt.Printf("No scheduled sync was registered under %q; nothing to remove.\n", label)
			return nil
		case err != nil:
			return err
		}
		fmt.Printf("Removed the scheduled sync registered under %q.\n", label)
		return nil
	},
}

// scheduleBinaryPath is the ccvault to register: the one running right now,
// with symlinks resolved.
//
// Resolved rather than taken raw, because the point of recording it is that
// `status` can later compare it against the ccvault on PATH and say whether
// they are the same binary. Two spellings of one file must not read as two
// files.
func scheduleBinaryPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("find this binary's own path, which the unit file has to name: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		// A path os.Executable gave us that will not resolve is strange
		// enough to report rather than register.
		return "", fmt.Errorf("resolve %s: %w", exe, err)
	}
	return resolved, nil
}

// scheduleExtraArgs is what the registered `sync` carries beyond its own
// name.
//
// The default is nothing — #171 settled on plain `ccvault sync`. The two
// exceptions are the flags that would otherwise make the registered job sync
// a different archive than the one the user was looking at when they typed
// install: an explicit --data-dir or --config is carried through so the unit
// is coherent with the request. Neither is destructive; --full and --rebuild
// are refused by Job.Validate regardless of how they arrive.
func scheduleExtraArgs(cmd *cobra.Command) []string {
	var args []string
	if dataDir, _ := cmd.Flags().GetString("data-dir"); dataDir != "" {
		args = append(args, "--data-dir", dataDir)
	}
	if configFile, _ := cmd.Flags().GetString("config"); configFile != "" {
		args = append(args, "--config", configFile)
	}
	if claudeHome, _ := cmd.Flags().GetString("claude-home"); claudeHome != "" {
		args = append(args, "--claude-home", claudeHome)
	}
	return args
}

// scheduleReport gathers the four facts and judges them.
//
// All the impure work is here — asking the OS, stat-ing paths, looking up
// PATH, reading the archive — so that schedule.Assemble stays a pure
// function of what was found.
func scheduleReport(cmd *cobra.Command, label string) (schedule.Report, error) {
	facts := schedule.Facts{
		Platform:  schedule.Platform(),
		Supported: schedule.Supported(),
		Label:     label,
		Now:       time.Now(),
	}

	// Fact 4 first, because it is the one fact available even on a platform
	// with no scheduler, and because a config that will not load is a reason
	// to stop rather than to report three facts out of four.
	cfg, err := loadConfig(cmd)
	if err != nil {
		return schedule.Report{}, fmt.Errorf("load config: %w", err)
	}
	facts.LogPath = filepath.Join(cfg.DataDir, ScheduleLogName)

	if database, dbErr := openArchiveForRead(cfg.DataDir); dbErr == nil {
		defer func() { _ = database.Close() }()
		if lastSynced, syncErr := database.LastSyncedAt(); syncErr == nil {
			facts.LastSynced = lastSynced
		}
		// A read error leaves LastSynced zero, which Assemble reports as
		// "never synced" — conservative, and visible.
	}

	if !facts.Supported {
		return schedule.Assemble(facts), nil
	}

	manager, err := schedule.New()
	if err != nil {
		return schedule.Report{}, err
	}
	reg, err := manager.Inspect(label)
	if err != nil {
		// Not swallowed into "nothing registered": the whole value of this
		// command is that it does not guess.
		return schedule.Report{}, fmt.Errorf("ask %s what it has registered: %w", manager.Platform(), err)
	}
	facts.Registration = reg
	facts.ProgramResolved = resolveOrEmpty(reg.Program)
	if reg.LogPath != "" {
		// The registered log path beats the computed one — it is what the job
		// actually writes to.
		facts.LogPath = reg.LogPath
	}

	if onPath, lookErr := exec.LookPath("ccvault"); lookErr == nil {
		facts.PathProgram = onPath
		facts.PathResolved = resolveOrEmpty(onPath)
	}

	return schedule.Assemble(facts), nil
}

// resolveOrEmpty resolves symlinks, or returns "" when the path is not there.
//
// "" is load-bearing: schedule.Assemble reads it as "the registered binary
// does not exist", which is fact 2's first half.
func resolveOrEmpty(path string) string {
	if path == "" {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return ""
	}
	return resolved
}

// printScheduleStatus renders the report for a person.
func printScheduleStatus(r schedule.Report) {
	fmt.Println("ccvault Scheduled Sync")
	fmt.Println("======================")
	fmt.Println()

	if !r.Supported {
		fmt.Printf("Platform:      %s — ccvault cannot register a scheduled sync here yet\n", r.Platform)
		fmt.Println()
		fmt.Println("Run `ccvault sync` yourself, or from whatever scheduler this platform has.")
		return
	}

	fmt.Printf("Mechanism:     %s (user-level, no sudo)\n", r.Platform)
	fmt.Printf("Label:         %s\n", r.Label)
	fmt.Printf("Registered:    %s\n", yesNo(r.Registered))
	if r.Registered {
		if r.Registration.UnitPath != "" {
			fmt.Printf("Unit:          %s\n", r.Registration.UnitPath)
		}
		fmt.Printf("Runs:          %s\n", joinArgs(r.Registration.Arguments))
		if r.Registration.Interval > 0 {
			fmt.Printf("Every:         %s\n", r.Registration.Interval)
		}
		fmt.Printf("Binary exists: %s\n", yesNo(r.ProgramExists))
		fmt.Printf("Matches PATH:  %s%s\n", yesNo(r.PathMatches), pathSuffix(r))
		fmt.Printf("Last run:      %s\n", lastRunLine(r))
		if r.Registration.RunsKnown {
			fmt.Printf("Run count:     %d\n", r.Registration.Runs)
		}
		if r.LogPath != "" {
			fmt.Printf("Output:        %s\n", r.LogPath)
		}
	}
	fmt.Printf("Archive:       %s\n", archiveLine(r))
	fmt.Println()

	if r.Healthy() {
		fmt.Println("Everything checks out.")
		return
	}
	fmt.Println("Problems:")
	for _, p := range r.Problems {
		fmt.Printf("  [%s] %s\n", p.Code, p.Detail)
	}
}

// lastRunLine renders fact 3 without letting "unknown" look like "succeeded".
func lastRunLine(r schedule.Report) string {
	switch {
	case !r.Registration.LastRunKnown && !r.Registration.LastExitKnown:
		return "never (the job has not run yet)"
	case !r.Registration.LastExitKnown:
		return fmt.Sprintf("%s, outcome not reported by %s",
			r.Registration.LastRun.Local().Format("2006-01-02 15:04"), r.Platform)
	case !r.Registration.LastRunKnown:
		return fmt.Sprintf("time not reported by %s, exited %d", r.Platform, r.Registration.LastExitCode)
	default:
		return fmt.Sprintf("%s, exited %d",
			r.Registration.LastRun.Local().Format("2006-01-02 15:04"), r.Registration.LastExitCode)
	}
}

// archiveLine renders fact 4.
func archiveLine(r schedule.Report) string {
	if r.LastSynced.IsZero() {
		return "never synced"
	}
	return fmt.Sprintf("last advanced %s (%s ago)",
		r.LastSynced.Local().Format("2006-01-02 15:04"), r.Staleness.Round(time.Minute))
}

// pathSuffix names the two binaries when they differ, on the line that says
// they differ, so the reader does not have to go hunting in the problem list.
func pathSuffix(r schedule.Report) string {
	if r.PathMatches || !r.Registered {
		return ""
	}
	if r.PathProgram == "" {
		return " (no ccvault on PATH to compare against)"
	}
	return fmt.Sprintf(" (PATH has %s)", r.PathProgram)
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// joinArgs renders an argument vector for display.
func joinArgs(args []string) string {
	out := ""
	for i, a := range args {
		if i > 0 {
			out += " "
		}
		out += a
	}
	return out
}

// scheduleStatusJSON renders the report for a machine.
//
// Every fact is present on every run, including the ones that are false,
// because a consumer checking "registered" must not have to distinguish an
// absent key from a false one. problems[] carries the stable codes.
func scheduleStatusJSON(r schedule.Report) map[string]interface{} {
	problems := make([]map[string]string, 0, len(r.Problems))
	for _, p := range r.Problems {
		problems = append(problems, map[string]string{"code": p.Code, "detail": p.Detail})
	}

	out := map[string]interface{}{
		"platform":   r.Platform,
		"supported":  r.Supported,
		"label":      r.Label,
		"registered": r.Registered,
		"healthy":    r.Healthy(),
		"problems":   problems,
	}
	if r.Supported {
		out["binary"] = map[string]interface{}{
			"registered_path": r.Registration.Program,
			"exists":          r.ProgramExists,
			"path_lookup":     r.PathProgram,
			"matches_path":    r.PathMatches,
		}
		lastRun := map[string]interface{}{
			"known":           r.Registration.LastRunKnown,
			"exit_code_known": r.Registration.LastExitKnown,
			"succeeded":       r.LastRunSucceeded,
		}
		if r.Registration.LastRunKnown {
			lastRun["at"] = r.Registration.LastRun.Format(time.RFC3339)
		}
		if r.Registration.LastExitKnown {
			lastRun["exit_code"] = r.Registration.LastExitCode
		}
		if r.Registration.RunsKnown {
			lastRun["run_count"] = r.Registration.Runs
		}
		out["last_run"] = lastRun
		if r.Registration.UnitPath != "" {
			out["unit_path"] = r.Registration.UnitPath
		}
		if r.Registration.Interval > 0 {
			out["interval_seconds"] = int64(r.Registration.Interval.Seconds())
		}
		if r.LogPath != "" {
			out["log_path"] = r.LogPath
		}
	}
	archive := map[string]interface{}{"stale": r.Stale}
	if !r.LastSynced.IsZero() {
		archive["last_synced"] = r.LastSynced.Format(time.RFC3339)
		archive["staleness_seconds"] = int64(r.Staleness.Seconds())
	}
	out["archive"] = archive
	return out
}

// scheduleOfferInputs is everything the decision to ask depends on.
//
// A struct, and the decision a pure function over it, because the rule has
// five clauses and four of them are "do not ask" — the kind of logic that is
// easy to get subtly wrong and impossible to test through a prompt.
type scheduleOfferInputs struct {
	// Supported is whether this platform has a mechanism at all.
	Supported bool

	// DataDirGiven is whether --data-dir was passed on this invocation.
	DataDirGiven bool

	// StdinTTY is whether a prompt could be answered.
	StdinTTY bool

	// Declined is schedule.declined from the config file.
	Declined bool

	// AlreadyRegistered is whether a unit is already there.
	AlreadyRegistered bool
}

// shouldOfferSchedule decides whether quickstart offers to install the
// schedule, and says why not when it does not.
//
// Each refusal is from #171, and the third is the one that matters most here:
//
//   - Unsupported platform: there is nothing to offer.
//   - Already registered: nothing to do.
//   - --data-dir: agents drive ccvault against throwaway archives dozens of
//     times in a single session of this repo's own development. Offering per
//     run would accumulate units pointing at deleted temp directories, each
//     hourly-retrying a sync against nothing, and it would stay invisible
//     until there were forty of them.
//   - Not a TTY: ccvault is mostly consumed by agents and the MCP server, and
//     a blocking prompt would hang them. Same helper #36 uses so `--full`
//     refuses to run unattended.
//   - Declined before: asking again would make "no" mean "not this time",
//     which is not what was said.
//
// The reason is returned rather than discarded so quickstart can say what it
// skipped and why. A step that silently does not happen is one nobody can
// debug.
func shouldOfferSchedule(in scheduleOfferInputs) (bool, string) {
	switch {
	case !in.Supported:
		return false, fmt.Sprintf("ccvault cannot schedule a background sync on %s yet.", schedule.Platform())
	case in.AlreadyRegistered:
		return false, "A scheduled sync is already registered. `ccvault schedule status` reports on it."
	case in.DataDirGiven:
		return false, "Skipping the scheduled-sync offer: --data-dir names a throwaway archive, and a unit pointing at one would outlive it."
	case !in.StdinTTY:
		return false, "Skipping the scheduled-sync offer: stdin is not a terminal. Run `ccvault schedule install` to set it up."
	case in.Declined:
		return false, "Scheduled sync was declined earlier (declined = true under [schedule]). Delete that line to be asked again."
	default:
		return true, ""
	}
}

// offerScheduleInstall is quickstart's offer, defaulting to yes.
//
// Never registers silently: installing an OS-level background job without
// asking is doing something the user did not request. Declining is written to
// the config file so the offer is not repeated — and written somewhere the
// user can find and delete, rather than into hidden state.
//
// A failure to install, or to remember a decline, is reported and not
// returned: quickstart's job is to get someone started, and neither is worth
// failing that over.
func offerScheduleInstall(cmd *cobra.Command, cfg *config.Config) {
	dataDirGiven, _ := cmd.Flags().GetString("data-dir")

	in := scheduleOfferInputs{
		Supported:    schedule.Supported(),
		DataDirGiven: dataDirGiven != "",
		StdinTTY:     isStdinTTY(),
		Declined:     cfg.Schedule.Declined,
	}
	// Only asked on a platform that has an answer, and only when the cheap
	// refusals have not already settled it — Inspect shells out.
	if in.Supported && !in.DataDirGiven && in.StdinTTY && !in.Declined {
		if manager, err := schedule.New(); err == nil {
			if reg, inspectErr := manager.Inspect(schedule.DefaultLabel); inspectErr == nil {
				in.AlreadyRegistered = reg.Found
			}
		}
	}

	ask, reason := shouldOfferSchedule(in)
	if !ask {
		if reason != "" {
			fmt.Println(reason)
			fmt.Println()
		}
		return
	}

	fmt.Println("Keep the archive current automatically?")
	fmt.Println()
	fmt.Println("  ccvault only archives what it has seen, and Claude Code keeps transcripts")
	fmt.Println("  on disk for about 38 days. An hourly background sync means completeness")
	fmt.Printf("  stops depending on remembering. It is a user-level %s job, needs no\n", schedule.Platform())
	fmt.Println("  sudo, runs plain `ccvault sync`, and `ccvault schedule uninstall` removes it.")
	fmt.Println()
	fmt.Print("Install it? [Y/n] ")

	var answer string
	// A read error is an empty answer, which is the default: yes. Fscanln
	// returns an error for a bare newline, which is exactly the Enter press
	// the default exists for.
	_, _ = fmt.Fscanln(os.Stdin, &answer)
	if declinedAnswer(answer) {
		fmt.Println()
		fmt.Println("Not installed. Run `ccvault schedule install` whenever you want it.")
		if err := cfg.RememberScheduleDeclined(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not record that you declined, so you may be asked again: %v\n", err)
		}
		fmt.Println()
		return
	}

	binary, err := scheduleBinaryPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not work out which ccvault to schedule: %v\n", err)
		return
	}
	job := schedule.NewJob(binary, filepath.Join(cfg.DataDir, ScheduleLogName))
	manager, err := schedule.New()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
		return
	}
	if err := manager.Install(job); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not install the scheduled sync: %v\n", err)
		fmt.Fprintln(os.Stderr, "Run `ccvault schedule install` to see the full error.")
		return
	}

	fmt.Println()
	fmt.Printf("Installed. %s will run `ccvault sync` every %s, starting in about %s.\n",
		manager.Platform(), job.Interval, job.Interval)
	fmt.Println("Check on it any time with `ccvault schedule status`.")
	fmt.Println()
}

// declinedAnswer reads a [Y/n] answer. Anything that is not a clear no is
// yes, because the prompt said the default was yes.
func declinedAnswer(answer string) bool {
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "n", "no":
		return true
	default:
		return false
	}
}

// scheduleNotice is the staleness warning `ccvault stats` prints, or "" when
// there is nothing worth saying.
//
// Silent when clean is the whole contract — the same shape as #106's
// `--strict` hint and #155's `repair-tokens` hint: say nothing when the
// archive is fine, and name the command that fixes it when it is not. A
// notice on every run is one that gets trained away inside a week.
//
// Two things suppress it:
//
//   - notify = false under [schedule], which is a line in the config file
//     rather than hidden state, so a dismissal is inspectable.
//   - a --data-dir run. A throwaway archive is stale by construction and
//     carries no schedule, so the notice would fire on every one of the
//     dozens of fixture runs an agent makes in a session. Nagging that is
//     always wrong teaches people to stop reading.
//
// lastSynced is passed in rather than re-read: stats already has the archive
// open, and opening it twice to print one line would be work for nothing.
func scheduleNotice(cmd *cobra.Command, cfg *config.Config, lastSynced time.Time) string {
	if !cfg.Schedule.Notify {
		return ""
	}
	if dataDir, _ := cmd.Flags().GetString("data-dir"); dataDir != "" {
		return ""
	}
	return scheduleNoticeReport(cfg, lastSynced).Notice()
}

// scheduleNoticeReport gathers just enough to judge the schedule for the
// notice.
//
// Split out so the cheap parts stay testable: it does not open the archive,
// and on a platform with no scheduler it asks the OS nothing.
func scheduleNoticeReport(cfg *config.Config, lastSynced time.Time) schedule.Report {
	facts := schedule.Facts{
		Platform:   schedule.Platform(),
		Supported:  schedule.Supported(),
		Label:      schedule.DefaultLabel,
		LastSynced: lastSynced,
		LogPath:    filepath.Join(cfg.DataDir, ScheduleLogName),
		Now:        time.Now(),
	}
	if facts.Supported {
		if manager, err := schedule.New(); err == nil {
			if reg, inspectErr := manager.Inspect(schedule.DefaultLabel); inspectErr == nil {
				facts.Registration = reg
				facts.ProgramResolved = resolveOrEmpty(reg.Program)
				if reg.LogPath != "" {
					facts.LogPath = reg.LogPath
				}
			}
			// An Inspect failure leaves the zero Registration, which reads as
			// "nothing registered" — the message that is useful anyway, and
			// `schedule status` is where the real error belongs.
		}
		if onPath, lookErr := exec.LookPath("ccvault"); lookErr == nil {
			facts.PathProgram = onPath
			facts.PathResolved = resolveOrEmpty(onPath)
		}
	}
	return schedule.Assemble(facts)
}

// scheduleNoticeJSON renders the same verdict for `stats --json`.
//
// Problem codes rather than the prose, because a consumer wants to branch on
// them. Returns nil when there is nothing to say, so the key is absent on a
// clean run exactly as the printed notice is.
func scheduleNoticeJSON(cmd *cobra.Command, cfg *config.Config, lastSynced time.Time) []map[string]string {
	if !cfg.Schedule.Notify {
		return nil
	}
	if dataDir, _ := cmd.Flags().GetString("data-dir"); dataDir != "" {
		return nil
	}
	r := scheduleNoticeReport(cfg, lastSynced)
	if r.Notice() == "" {
		return nil
	}
	out := make([]map[string]string, 0, len(r.Problems))
	for _, p := range r.Problems {
		out = append(out, map[string]string{"code": p.Code, "detail": p.Detail})
	}
	return out
}
