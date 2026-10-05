// ABOUTME: Main entry point for ccvault CLI application
// ABOUTME: Initializes and executes the root command via Cobra

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	// Register source adapters so their init() functions run
	_ "github.com/2389-research/ccvault/pkg/adapter/claudecode"
	_ "github.com/2389-research/ccvault/pkg/adapter/codex"
	_ "github.com/2389-research/ccvault/pkg/adapter/hex"
	_ "github.com/2389-research/ccvault/pkg/adapter/jeff"
	_ "github.com/2389-research/ccvault/pkg/adapter/nanoclaw"

	"golang.org/x/term"

	"github.com/2389-research/ccvault/internal/analytics"
	"github.com/2389-research/ccvault/internal/compact"
	"github.com/2389-research/ccvault/internal/config"
	"github.com/2389-research/ccvault/internal/db"
	"github.com/2389-research/ccvault/internal/export"
	"github.com/2389-research/ccvault/internal/mcp"
	"github.com/2389-research/ccvault/internal/projectref"
	"github.com/2389-research/ccvault/internal/search"
	"github.com/2389-research/ccvault/internal/sync"
	"github.com/2389-research/ccvault/internal/tui"
	"github.com/2389-research/ccvault/pkg/models"
	"github.com/spf13/cobra"
)

var version = "0.1.0"

var rootCmd = &cobra.Command{
	Use:   "ccvault",
	Short: "Archive and search Claude Code conversations",
	Long: `ccvault archives your Claude Code conversation history for offline
search, analytics, and AI integration.

Similar to msgvault for email, ccvault provides:
  - Full-text search across all conversations
  - Interactive TUI for drill-down analytics
  - MCP server for AI assistant integration

Every command reads its archive from one data directory. Resolved highest
precedence first:

  --data-dir <path>     this flag
  CCVAULT_DATA_DIR      environment
  data_dir = "..."      the config file
  ~/.ccvault            built-in default

--data-dir also REPLACES the config search path, so ~/.ccvault is not read
at all when it is given. --config names a single config file instead of
searching for one; a path that does not exist is an error. Together they
make the whole CLI runnable against a throwaway archive:

  ccvault --data-dir /tmp/fixture sync
  ccvault --data-dir /tmp/fixture stats --json

--data-dir redirects the archive, not the source it reads: claude_home
still defaults to ~/.claude. Set claude_home (or sources) in the fixture's
config.toml, or CCVAULT_CLAUDE_HOME, to redirect that as well.`,
}

// loadConfig reads configuration honouring the root command's persistent
// --config and --data-dir flags. Every subcommand goes through this rather
// than config.Load() so that pointing ccvault at a throwaway archive works
// uniformly — the recurring need that bare config.Load() could not meet.
func loadConfig(cmd *cobra.Command) (*config.Config, error) {
	// Persistent flags from the root are merged into every subcommand's
	// flag set, so these lookups resolve from any depth.
	configFile, _ := cmd.Flags().GetString("config")
	dataDir, _ := cmd.Flags().GetString("data-dir")

	cfg, err := config.LoadWith(config.Options{
		ConfigFile: configFile,
		DataDir:    dataDir,
	})
	if err != nil {
		// A bad config path or a malformed config file is a plain message,
		// not a reason to dump the flag table — that's for argument-shape
		// errors.
		cmd.SilenceUsage = true
		return nil, err
	}
	return cfg, nil
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the version number",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("ccvault %s\n", version)
	},
}

var quickstartCmd = &cobra.Command{
	Use:   "quickstart",
	Short: "Get started with ccvault",
	Long:  `Interactive quickstart guide for new users. Syncs data if needed and shows helpful next steps.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println("Welcome to ccvault!")
		fmt.Println("==================")
		fmt.Println()

		// Load config
		cfg, err := loadConfig(cmd)
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}

		// Ensure data directory exists
		if err := config.EnsureDataDir(cfg); err != nil {
			return fmt.Errorf("create data dir: %w", err)
		}

		// Open database
		database, err := db.Open(cfg.DataDir)
		if err != nil {
			return fmt.Errorf("open database: %w", err)
		}
		defer func() { _ = database.Close() }()

		// Check current state
		sessionCount, turnCount, totalTokens, err := database.GetSessionStats()
		if err != nil {
			return fmt.Errorf("get stats: %w", err)
		}

		// If no data, run sync
		if sessionCount == 0 {
			fmt.Println("No conversations indexed yet. Let's sync your Claude Code history...")
			fmt.Println()

			syncer := sync.New(database, cfg.Sources,
				sync.WithProgressCallback(func(msg string) {
					fmt.Println("  " + msg)
				}),
			)

			stats, err := syncer.Run(cmd.Context())
			if err != nil {
				return fmt.Errorf("sync: %w", err)
			}

			fmt.Println()
			fmt.Printf("Synced %d sessions with %d turns.\n", stats.SessionsIndexed, stats.TurnsIndexed)
			fmt.Println()
		} else {
			fmt.Printf("Found %d sessions with %d turns (%s tokens).\n",
				sessionCount, turnCount, formatTokens(totalTokens))
			fmt.Println()
		}

		// Show next steps
		fmt.Println("What would you like to do?")
		fmt.Println()
		fmt.Println("  ccvault tui              Launch interactive browser")
		fmt.Println("  ccvault search <query>   Search conversations")
		fmt.Println("  ccvault stats            View detailed statistics")
		fmt.Println("  ccvault sync             Update with latest conversations")
		fmt.Println("  ccvault mcp              Start MCP server for AI integration")
		fmt.Println()
		fmt.Println("Run 'ccvault --help' for all available commands.")

		return nil
	},
}

// orientation holds the database state gathered for the orient command.
type orientation struct {
	ProjectCount   int
	SessionCount   int
	TurnCount      int
	SessionTokens  int64
	FirstActivity  time.Time
	LastActivity   time.Time
	ToolStats      map[string]int
	TokensByModel  map[string]int64
	RecentProjects []models.Project // full rows so JSON emits {name, path} via projectref.Ref
	Storage        db.StorageStats
	Warnings       []string
}

// gatherOrientation collects database state for the orient command.
// Query failures land in Warnings instead of being silently dropped.
func gatherOrientation(database *db.DB) orientation {
	var o orientation
	warn := func(what string, err error) {
		if err != nil {
			o.Warnings = append(o.Warnings, fmt.Sprintf("%s unavailable: %v", what, err))
		}
	}

	var err error
	o.ProjectCount, _, err = database.GetProjectStats()
	warn("project stats", err)

	o.SessionCount, o.TurnCount, o.SessionTokens, err = database.GetSessionStats()
	warn("session stats", err)

	o.FirstActivity, o.LastActivity, err = database.GetFirstAndLastActivity()
	warn("activity range", err)

	o.ToolStats, err = database.GetToolUsageStats(5)
	warn("tool stats", err)

	o.TokensByModel, err = database.GetTokensByModel()
	warn("model stats", err)

	projects, err := database.GetProjects("activity", 5)
	warn("recent projects", err)
	o.RecentProjects = projects

	o.Storage, err = database.StorageStats()
	warn("storage stats", err)

	return o
}

// storageJSON renders page accounting for --json consumers. Dead space in
// the database file is invisible otherwise: the file grows every sync and
// nothing in the archive's row counts explains why.
func storageJSON(s db.StorageStats) map[string]interface{} {
	return map[string]interface{}{
		"file_bytes":        s.FileBytes,
		"page_size":         s.PageSize,
		"page_count":        s.PageCount,
		"freelist_count":    s.FreelistCount,
		"live_bytes":        s.LiveBytes(),
		"reclaimable_bytes": s.ReclaimableBytes(),
		"freelist_ratio":    s.FreelistRatio(),
		"worth_reclaiming":  s.WorthReclaiming(),
	}
}

// reclaimHint returns a one-line nudge when the file holds enough dead
// space to be worth acting on, and an empty string otherwise. Compaction is
// never automatic — it rewrites the whole file — so this hint is how the
// user finds out the condition exists.
func reclaimHint(s db.StorageStats) string {
	if !s.WorthReclaiming() {
		return ""
	}
	return fmt.Sprintf("%s of the %s database file is dead space (%.0f%%); reclaim it with 'ccvault vacuum'",
		formatBytes(s.ReclaimableBytes()), formatBytes(s.FileBytes), s.FreelistRatio()*100)
}

// printStorageSection writes the human-readable storage block shared by
// stats and orient.
func printStorageSection(s db.StorageStats) {
	if s.PageCount <= 0 {
		return
	}
	fmt.Println("Storage:")
	fmt.Printf("  Database file: %s\n", formatBytes(s.FileBytes))
	fmt.Printf("  Reclaimable:   %s (%.0f%% of the file)\n",
		formatBytes(s.ReclaimableBytes()), s.FreelistRatio()*100)
	if s.WorthReclaiming() {
		fmt.Println("  Reclaim it with 'ccvault vacuum'.")
	}
	fmt.Println()
}

// integrityJSON renders the search-index comparison for --json consumers,
// alongside storageJSON.
func integrityJSON(f db.FTSIntegrity) map[string]interface{} {
	return map[string]interface{}{
		"turns":      f.Turns,
		"indexed":    f.Indexed,
		"orphaned":   f.Orphaned,
		"unindexed":  f.Missing(),
		"consistent": f.Consistent(),
	}
}

// printIntegritySection writes the human-readable search-index block.
//
// Drift between turns and turns_fts is otherwise completely silent. An
// orphaned index entry matches a query and then resolves to no turn at all,
// so it costs recall without ever surfacing an error; an unindexed turn is
// held by the archive and invisible to search. Neither shows up in any row
// count, because turns_fts is an external-content table that answers
// COUNT(*) from the turns table itself.
func printIntegritySection(f db.FTSIntegrity) {
	if f.Turns == 0 && f.Indexed == 0 {
		return
	}
	fmt.Println("Search index:")
	fmt.Printf("  Indexed:       %d of %d turns\n", f.Indexed-f.Orphaned, f.Turns)
	if f.Consistent() {
		fmt.Println()
		return
	}
	if f.Orphaned > 0 {
		fmt.Printf("  Orphaned:      %d entries that match queries but resolve to no turn\n", f.Orphaned)
	}
	if f.Missing() > 0 {
		fmt.Printf("  Unindexed:     %d turns that search cannot reach\n", f.Missing())
	}
	fmt.Println("  Rebuild the index with 'ccvault sync --rebuild'.")
	fmt.Println()
}

var orientCmd = &cobra.Command{
	Use:   "orient",
	Short: "Output database state for AI agents",
	Long: `Output structured information about the ccvault database state.
Designed for AI agents to understand available data and commands.

Use --json for machine-readable output.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		jsonOutput, _ := cmd.Flags().GetBool("json")

		cfg, err := loadConfig(cmd)
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}

		database, err := db.Open(cfg.DataDir)
		if err != nil {
			return fmt.Errorf("open database: %w", err)
		}
		defer func() { _ = database.Close() }()

		o := gatherOrientation(database)

		orientationMap := map[string]interface{}{
			"version": version,
			"status":  "ready",
			"database": map[string]interface{}{
				"projects":     o.ProjectCount,
				"sessions":     o.SessionCount,
				"turns":        o.TurnCount,
				"total_tokens": o.SessionTokens,
			},
			"activity": map[string]interface{}{
				"first_session": o.FirstActivity.Format(time.RFC3339),
				"last_session":  o.LastActivity.Format(time.RFC3339),
				"days_span":     int(o.LastActivity.Sub(o.FirstActivity).Hours() / 24),
			},
			"recent_projects": projectref.RefsFromValues(o.RecentProjects),
			"top_tools":       o.ToolStats,
			"models":          o.TokensByModel,
			"storage":         storageJSON(o.Storage),
			"commands": map[string]string{
				"search <query>":      "Full-text search across conversations",
				"list-sessions":       "List recent sessions",
				"list-projects":       "List all projects",
				"show <session-id>":   "Display a specific session",
				"export <session-id>": "Export session to markdown",
				"stats":               "Show detailed statistics",
				"sync":                "Update with latest conversations",
				"vacuum":              "Reclaim dead space in the database file",
				"tui":                 "Launch interactive browser",
				"mcp":                 "Start MCP server",
			},
			"search_syntax": map[string]string{
				"project:<name>":   "Filter by project",
				"model:<name>":     "Filter by model",
				"tool:<name>":      "Filter by tool used",
				"source:<name>":    "Filter by source",
				"after:<date>":     "Sessions after date",
				"before:<date>":    "Sessions before date",
				"\"exact phrase\"": "Exact phrase match",
			},
		}

		if len(o.Warnings) > 0 {
			orientationMap["warnings"] = o.Warnings
		}

		// Handle empty database
		if o.SessionCount == 0 {
			orientationMap["status"] = "empty"
			orientationMap["hint"] = "Run 'ccvault sync' to index conversations from ~/.claude"
		}

		if jsonOutput {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(orientationMap)
		}

		// Human-readable output
		for _, w := range o.Warnings {
			fmt.Fprintf(os.Stderr, "warning: %s\n", w)
		}

		fmt.Println("=== ccvault Orientation ===")
		fmt.Println()
		fmt.Printf("Version: %s\n", version)
		fmt.Printf("Status:  %s\n", orientationMap["status"])
		fmt.Println()

		if o.SessionCount == 0 {
			fmt.Println("Database is empty. Run 'ccvault sync' to index conversations.")
			return nil
		}

		fmt.Println("Database:")
		fmt.Printf("  Projects: %d\n", o.ProjectCount)
		fmt.Printf("  Sessions: %d\n", o.SessionCount)
		fmt.Printf("  Turns:    %d\n", o.TurnCount)
		fmt.Printf("  Tokens:   %s\n", formatTokens(o.SessionTokens))
		fmt.Println()

		fmt.Println("Activity:")
		fmt.Printf("  First: %s\n", o.FirstActivity.Format("2006-01-02"))
		fmt.Printf("  Last:  %s\n", o.LastActivity.Format("2006-01-02"))
		fmt.Printf("  Span:  %d days\n", int(o.LastActivity.Sub(o.FirstActivity).Hours()/24))
		fmt.Println()

		printStorageSection(o.Storage)

		if len(o.RecentProjects) > 0 {
			fmt.Println("Recent Projects:")
			for i := range o.RecentProjects {
				// Class B — human-readable inline form, with disambiguating path
				fmt.Printf("  - %s\n", projectref.Inline(&o.RecentProjects[i]))
			}
			fmt.Println()
		}

		if len(o.TokensByModel) > 0 {
			fmt.Println("Models Used:")
			for model, tokens := range o.TokensByModel {
				shortModel := model
				if len(shortModel) > 30 {
					shortModel = shortModel[:27] + "..."
				}
				fmt.Printf("  %-30s %s tokens\n", shortModel, formatTokens(tokens))
			}
			fmt.Println()
		}

		fmt.Println("Available Commands:")
		fmt.Println("  search <query>        Full-text search")
		fmt.Println("  list-sessions         List recent sessions")
		fmt.Println("  show <session-id>     Display session")
		fmt.Println("  stats                 Detailed statistics")
		fmt.Println("  mcp                   Start MCP server")
		fmt.Println()
		fmt.Println("Use --json for machine-readable output.")

		return nil
	},
}

var syncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Sync conversations from Claude Code",
	// User-facing errors (refusal on "no", non-TTY without --yes, sync
	// failures) shouldn't dump the whole flag table — that's noise
	// reserved for argument-parse errors.
	SilenceUsage: true,
	Long: `Scan ~/.claude and index new or updated sessions into the ccvault database.

By default, sync is INCREMENTAL — only files whose mtimes have changed are
re-parsed. Sessions whose source files have been pruned upstream stay in the
archive; holding conversations after Claude Code deletes them is the point.

--full re-parses EVERY discovered session file, ignoring the mtime
skip-check. Use it after a parser change that extracts more from the same
JSONL. It is not destructive: each file replaces its own rows as the scan
reaches it, and rows whose source file is gone are left alone.

--rebuild WIPES all indexed data, then re-scans — so the archive ends up
holding exactly what is on disk right now. Everything the source has since
pruned is DESTROYED. Before wiping, it:
  1. Shows the current row counts, including how many sessions have no
     source file on disk and therefore cannot be re-created, and prompts
     for confirmation. Non-interactive callers (piped stdin, cron, CI) must
     pass --yes; they will be REFUSED without it — no silent wipes.
  2. Backs up the SQLite DB to <data_dir>/backups/ccvault-<timestamp>.db
     via VACUUM INTO (skipped with --no-backup).
  3. Rotates to the most recent 5 backups.

If a rebuild produces bad state, restore by copying the backup file back
over the live DB (the path is printed at run), or merge it back in with
"ccvault import <backup>".`,
	RunE: func(cmd *cobra.Command, args []string) error {
		full, _ := cmd.Flags().GetBool("full")
		rebuild, _ := cmd.Flags().GetBool("rebuild")
		verbose, _ := cmd.Flags().GetBool("verbose")
		sourceFilter, _ := cmd.Flags().GetString("source")
		assumeYes, _ := cmd.Flags().GetBool("yes")
		noBackup, _ := cmd.Flags().GetBool("no-backup")

		// Load config
		cfg, err := loadConfig(cmd)
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}

		// Filter sources if --source flag is provided
		sources := cfg.Sources
		if sourceFilter != "" {
			var filtered []config.SourceConfig
			for _, s := range cfg.Sources {
				if s.Name == sourceFilter {
					filtered = append(filtered, s)
				}
			}
			if len(filtered) == 0 {
				names := make([]string, len(cfg.Sources))
				for i, s := range cfg.Sources {
					names[i] = s.Name
				}
				return fmt.Errorf("source %q not found; available sources: %s", sourceFilter, strings.Join(names, ", "))
			}
			sources = filtered
		}

		// Ensure data directory exists
		if err := config.EnsureDataDir(cfg); err != nil {
			return fmt.Errorf("create data dir: %w", err)
		}

		// Open database
		database, err := db.Open(cfg.DataDir)
		if err != nil {
			return fmt.Errorf("open database: %w", err)
		}
		defer func() { _ = database.Close() }()

		// --rebuild safety: confirm + backup BEFORE handing off to the
		// syncer, so a user who Ctrl-Cs at the prompt hasn't lost anything,
		// and a user who confirms has a snapshot to restore from. --full
		// doesn't need either — it destroys nothing.
		var backupPath string
		if rebuild {
			backupPath, err = prepareRebuild(database, cfg.DataDir, assumeYes, noBackup)
			if err != nil {
				return err
			}
		} else if assumeYes || noBackup {
			// These two only gate the wipe. A cron job carried over from when
			// --full was destructive will still pass them; say they're inert
			// rather than letting the user assume they did something.
			fmt.Fprintln(os.Stderr,
				"note: --yes and --no-backup only apply to --rebuild; this sync destroys nothing and takes no backup")
		}

		// Create syncer. Cache dir is plumbed through so a re-parse can
		// invalidate the analytics parquet alongside SQLite.
		syncer := sync.New(database, sources,
			sync.WithFullSync(full),
			sync.WithRebuild(rebuild),
			sync.WithVerbose(verbose),
			sync.WithCacheDir(filepath.Join(cfg.DataDir, "analytics")),
			sync.WithProgressCallback(func(msg string) {
				fmt.Println(msg)
			}),
		)

		// Run sync
		stats, err := syncer.Run(cmd.Context())
		if err != nil {
			if rebuild && backupPath != "" {
				fmt.Fprintf(os.Stderr, "\nsync failed after wiping data. Restore the pre-sync state with:\n"+
					"  cp %q %q\n", backupPath, filepath.Join(cfg.DataDir, "ccvault.db"))
			}
			return fmt.Errorf("sync: %w", err)
		}

		// Print summary
		fmt.Println()
		fmt.Printf("Sync completed in %s\n", stats.Duration.Round(time.Millisecond))
		fmt.Printf("  Projects:  %d\n", stats.ProjectsFound)
		fmt.Printf("  Sessions:  %d indexed, %d skipped\n", stats.SessionsIndexed, stats.SessionsSkipped)
		fmt.Printf("  Turns:     %d\n", stats.TurnsIndexed)
		fmt.Printf("  Tool uses: %d\n", stats.ToolUsesIndexed)
		if stats.TotalSkippedLines > 0 {
			fmt.Printf("  Skipped lines: %d across %d session(s)\n",
				stats.TotalSkippedLines, stats.SessionsWithSkippedLines)
		}
		if stats.TurnsWithTruncatedRawJSON > 0 {
			fmt.Printf("  Truncated raw_json: %d turn(s) across %d session(s)\n",
				stats.TurnsWithTruncatedRawJSON, stats.SessionsWithTruncatedTurns)
		}
		if stats.SessionsRewrittenUpstream > 0 {
			fmt.Printf("  Rewritten upstream: %d session(s) — the transcript was replaced, "+
				"not appended to, so the archived turns were swapped rather than extended\n",
				stats.SessionsRewrittenUpstream)
		}

		if len(stats.Errors) > 0 {
			fmt.Printf("  Errors:    %d\n", len(stats.Errors))
			if verbose {
				for _, e := range stats.Errors {
					fmt.Printf("    - %v\n", e)
				}
			}
		}

		// Every sync leaves freed pages behind, so sync is where the user
		// finds out the file has drifted away from the data in it. Reporting
		// only: compaction rewrites the whole file and takes the database
		// exclusively, which is no business of a sync run.
		if storage, err := database.StorageStats(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: storage stats unavailable: %v\n", err)
		} else if hint := reclaimHint(storage); hint != "" {
			fmt.Fprintf(os.Stderr, "\nnote: %s\n", hint)
		}

		return nil
	},
}

var tuiCmd = &cobra.Command{
	Use:   "tui",
	Short: "Launch interactive TUI",
	Long:  `Open the interactive terminal UI for browsing and analyzing conversations.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig(cmd)
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}

		database, err := db.Open(cfg.DataDir)
		if err != nil {
			return fmt.Errorf("open database: %w", err)
		}
		defer func() { _ = database.Close() }()

		cacheDir := filepath.Join(cfg.DataDir, "analytics")
		return tui.Run(database, cacheDir, cfg.Sources)
	},
}

var searchCmd = &cobra.Command{
	Use:   "search [query]",
	Short: "Search conversations",
	Long: `Full-text search across all archived conversations.

Supports Gmail-like query syntax:
  project:name     Filter by project
  model:opus       Filter by model
  tool:Bash        Sessions using specific tool
  before:date      Date filters
  after:date
  "exact phrase"   Exact match`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		queryStr := strings.Join(args, " ")
		jsonOutput, _ := cmd.Flags().GetBool("json")
		limit, _ := cmd.Flags().GetInt("limit")

		cfg, err := loadConfig(cmd)
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}

		database, err := db.Open(cfg.DataDir)
		if err != nil {
			return fmt.Errorf("open database: %w", err)
		}
		defer func() { _ = database.Close() }()

		// Parse and execute search
		query := search.Parse(queryStr)
		searcher := search.New(database.DB)
		results, err := searcher.Search(query, limit)
		if err != nil {
			return fmt.Errorf("search: %w", err)
		}

		if jsonOutput {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(results)
		}

		// Pretty print results
		if len(results) == 0 {
			fmt.Println("No results found.")
			return nil
		}

		fmt.Printf("Found %d results:\n\n", len(results))
		// Load projects once so adapter DisplayNames surface in results.
		// Surface enrichment failures on stderr rather than silently
		// falling through to basename — agents watching this output need
		// to know why adapter branding is missing.
		allProjects, enrichErr := database.GetProjects("activity", 0)
		if enrichErr != nil {
			fmt.Fprintf(os.Stderr, "warning: project enrichment lookup failed: %v — Project labels fell back to basename\n", enrichErr)
		}
		byPath := projectref.ProjectsByPath(allProjects)
		for i, r := range results {
			fmt.Printf("%d. [%s] %s  Session: %s\n", i+1, r.Turn.Type, r.Turn.Timestamp.Format("2006-01-02 15:04"), r.Turn.SessionID)
			// Class B — combined form so same-basename projects don't look identical.
			// Look up the full project so adapter DisplayName is preserved.
			var p *models.Project
			if hit, ok := byPath[r.ProjectPath]; ok {
				p = hit
			} else {
				p = &models.Project{Path: r.ProjectPath}
			}
			fmt.Printf("   Project: %s\n", projectref.Inline(p))
			// Search is never filtered, so hits land inside subagent
			// transcripts that no default listing shows. Naming the parent
			// is how the reader places the hit in a conversation.
			if r.ParentSessionID != "" {
				fmt.Printf("   Subagent of: %s\n", r.ParentSessionID)
			}
			if r.Model != "" {
				fmt.Printf("   Model: %s\n", r.Model)
			}
			// A hit found through a stored tool payload rather than the turn's
			// own text needs saying, because the snippet below it is command
			// output or tool arguments, not something anyone wrote.
			if r.MatchedToolName != "" {
				fmt.Printf("   Matched in %s payload\n", r.MatchedToolName)
			}
			fmt.Printf("   %s\n\n", r.Snippet)
		}

		return nil
	},
}

var statsCmd = &cobra.Command{
	Use:   "stats",
	Short: "Show archive statistics",
	RunE: func(cmd *cobra.Command, args []string) error {
		jsonOutput, _ := cmd.Flags().GetBool("json")

		cfg, err := loadConfig(cmd)
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}

		database, err := db.Open(cfg.DataDir)
		if err != nil {
			return fmt.Errorf("open database: %w", err)
		}
		defer func() { _ = database.Close() }()

		// Get statistics
		projectCount, projectTokens, err := database.GetProjectStats()
		if err != nil {
			return fmt.Errorf("get project stats: %w", err)
		}

		sessionCount, turnCount, sessionTokens, err := database.GetSessionStats()
		if err != nil {
			return fmt.Errorf("get session stats: %w", err)
		}

		first, last, err := database.GetFirstAndLastActivity()
		if err != nil {
			return fmt.Errorf("get activity range: %w", err)
		}

		toolStats, err := database.GetToolUsageStats(10)
		if err != nil {
			return fmt.Errorf("get tool stats: %w", err)
		}

		tokensByModel, err := database.GetTokensByModel()
		if err != nil {
			return fmt.Errorf("get tokens by model: %w", err)
		}

		storage, err := database.StorageStats()
		if err != nil {
			return fmt.Errorf("get storage stats: %w", err)
		}

		integrity, err := database.CheckFTSIntegrity()
		if err != nil {
			return fmt.Errorf("check search index integrity: %w", err)
		}

		if jsonOutput {
			out := map[string]interface{}{
				"projects":        projectCount,
				"sessions":        sessionCount,
				"turns":           turnCount,
				"total_tokens":    sessionTokens,
				"project_tokens":  projectTokens,
				"tokens_by_model": tokensByModel,
				"top_tools":       toolStats,
				"storage":         storageJSON(storage),
				"search_index":    integrityJSON(integrity),
			}
			if !first.IsZero() && !last.IsZero() {
				out["activity"] = map[string]interface{}{
					"first_session": first.Format(time.RFC3339),
					"last_session":  last.Format(time.RFC3339),
					"days_span":     int(last.Sub(first).Hours() / 24),
				}
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(out)
		}

		// Print statistics
		fmt.Println("ccvault Archive Statistics")
		fmt.Println("==========================")
		fmt.Println()
		fmt.Printf("Projects:      %d\n", projectCount)
		fmt.Printf("Sessions:      %d\n", sessionCount)
		fmt.Printf("Turns:         %d\n", turnCount)
		fmt.Printf("Total Tokens:  %s\n", formatTokens(sessionTokens))
		fmt.Println()

		if !first.IsZero() && !last.IsZero() {
			fmt.Printf("Date Range:    %s to %s\n", first.Format("2006-01-02"), last.Format("2006-01-02"))
			fmt.Printf("Duration:      %d days\n", int(last.Sub(first).Hours()/24))
			fmt.Println()
		}

		printStorageSection(storage)
		printIntegritySection(integrity)

		if len(tokensByModel) > 0 {
			fmt.Println("Tokens by Model:")
			for model, tokens := range tokensByModel {
				shortModel := model
				if len(shortModel) > 30 {
					shortModel = shortModel[:30] + "..."
				}
				fmt.Printf("  %-35s %s\n", shortModel, formatTokens(tokens))
			}
			fmt.Println()
		}

		if len(toolStats) > 0 {
			fmt.Println("Top Tools:")
			for tool, count := range toolStats {
				fmt.Printf("  %-20s %d uses\n", tool, count)
			}
		}

		// Also print _ tokens from project stats if different (shouldn't be, but just in case)
		_ = projectTokens

		return nil
	},
}

var vacuumCmd = &cobra.Command{
	Use:   "vacuum",
	Short: "Reclaim dead space in the database file",
	// A refusal (busy database, not enough disk) is a plain message, not a
	// reason to dump the flag table.
	SilenceUsage: true,
	Long: `Shrink the ccvault database file by discarding its freelist — pages SQLite
freed internally but never returned to the filesystem.

Incremental sync replaces the rows of every changed session file, so the file
grows relative to the data it holds. A long-lived archive can be more dead
space than data.

How it works: a compacted copy is written beside the live database with
VACUUM INTO, checked with integrity_check and a row count per table, then
renamed over the original in one atomic step. The original is never modified,
so any failure before the swap leaves it exactly as it was.

Requirements and caveats:
  - Free disk space for the compacted copy (roughly the size of the live
    data, not the whole file). It refuses up front if the space isn't there.
  - Exclusive access. Close the TUI, MCP server, and any running sync first;
    it refuses rather than compacting a database someone else is writing to.
  - Whole-file rewrite. On a multi-gigabyte archive, expect tens of seconds.

Run 'ccvault stats' to see how much there is to reclaim before committing.
Use --json for machine-readable output.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		jsonOutput, _ := cmd.Flags().GetBool("json")

		cfg, err := loadConfig(cmd)
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}

		// Deliberately does not open the database: compaction takes the file
		// for itself, and a handle held here would be the thing blocking it.
		progress := func(msg string) { fmt.Println("  " + msg) }
		if jsonOutput {
			// Keep stdout parseable; progress still goes somewhere, because a
			// multi-gigabyte compaction is a long silence otherwise.
			progress = func(msg string) { fmt.Fprintln(os.Stderr, msg) }
		} else {
			fmt.Printf("Compacting %s\n", filepath.Join(cfg.DataDir, "ccvault.db"))
		}

		result, err := db.Compact(cfg.DataDir, db.WithCompactProgress(progress))
		if err != nil {
			return err
		}

		if jsonOutput {
			out := map[string]interface{}{
				"database":        result.Path,
				"before":          storageJSON(result.Before),
				"after":           storageJSON(result.After),
				"bytes_reclaimed": result.BytesReclaimed(),
				"duration_ms":     result.Duration.Milliseconds(),
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(out)
		}

		fmt.Println()
		fmt.Printf("Reclaimed %s in %s\n",
			formatBytes(result.BytesReclaimed()), result.Duration.Round(time.Millisecond))
		fmt.Printf("  Before: %s (%d pages, %d free)\n",
			formatBytes(result.Before.FileBytes), result.Before.PageCount, result.Before.FreelistCount)
		fmt.Printf("  After:  %s (%d pages, %d free)\n",
			formatBytes(result.After.FileBytes), result.After.PageCount, result.After.FreelistCount)
		return nil
	},
}

var listProjectsCmd = &cobra.Command{
	Use:   "list-projects",
	Short: "List all indexed projects",
	RunE: func(cmd *cobra.Command, args []string) error {
		jsonOutput, _ := cmd.Flags().GetBool("json")
		sortBy, _ := cmd.Flags().GetString("sort")
		limit, _ := cmd.Flags().GetInt("limit")

		cfg, err := loadConfig(cmd)
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}

		database, err := db.Open(cfg.DataDir)
		if err != nil {
			return fmt.Errorf("open database: %w", err)
		}
		defer func() { _ = database.Close() }()

		projects, err := database.GetProjects(sortBy, limit)
		if err != nil {
			return fmt.Errorf("get projects: %w", err)
		}

		if jsonOutput {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			// Class C — Ref shape via projectref so agents see {name, path}.
			return enc.Encode(projectref.EnrichedRefsFromValues(projects))
		}

		if len(projects) == 0 {
			fmt.Println("No projects found. Run 'ccvault sync' first.")
			return nil
		}

		// Route CLI cell rendering through compact so wide paths get
		// smart initialing (via segment initialing) rather than "..." +
		// tail truncation. Widths chosen to fit an 80-col terminal:
		// 20+28+8+10+10 = 76 + 4 seps = 80.
		fmt.Printf("%s %s %8s %10s %10s\n",
			padVisualCLI("PROJECT", 20),
			padVisualCLI("PATH", 28),
			"SESSIONS", "TOKENS", "ACTIVE")
		fmt.Println(strings.Repeat("-", 80))
		for i := range projects {
			p := &projects[i]
			// Class A — Label for the short column, compact.Path for the path.
			nameText := compact.Truncate(projectref.Label(p), 20).Text
			pathText := compact.Path(p.Path, 28).Text
			lastActive := compact.Date(p.LastActivityAt, 10).Text
			fmt.Printf("%s %s %8d %10s %10s\n",
				padVisualCLI(nameText, 20),
				padVisualCLI(pathText, 28),
				p.SessionCount, formatTokens(p.TotalTokens), lastActive)
		}

		return nil
	},
}

// subagentScopeFromFlags maps the list-sessions flags onto a listing scope.
// The default hides subagent sessions — on a real machine they outnumber
// top-level sessions roughly 2.6:1 — while every row still reports how many
// it stands for. --subagents-of beats --include-subagents because asking for
// one parent's children is the more specific request.
func subagentScopeFromFlags(includeSubagents bool, subagentsOf string) (db.SubagentScope, string) {
	switch {
	case subagentsOf != "":
		return db.SubagentsOf, subagentsOf
	case includeSubagents:
		return db.SubagentsIncluded, ""
	default:
		return db.SubagentsHidden, ""
	}
}

// Column widths for the list-sessions table. Named so the header, the rows,
// and the separator cannot drift apart — they used to be three magic numbers
// maintained by hand.
const (
	// sessionsIDMinWidth is the historical SESSION ID width: a 36-char uuid
	// plus two of gutter. It stays the floor so a listing of only top-level
	// sessions renders exactly as it always has.
	sessionsIDMinWidth   = 38
	sessionsIDGutter     = 2
	sessionsProjectWidth = 25
	sessionsStartedWidth = 16
	sessionsTurnsWidth   = 6
	sessionsTokensWidth  = 10
	sessionsSubsWidth    = 5
	sessionsModelWidth   = 25
)

// sessionIDColumnWidth sizes the SESSION ID column to the ids actually being
// rendered.
//
// A minted subagent id is 72 characters —
// claude-code:<36-char uuid>:agent-<17 hex> — against a top-level session's
// 36. A fixed-width %-38s pads but never truncates, so a single subagent row
// pushed every column after it 34 places right while the header stayed where
// it was, leaving a reader unable to tell which column was which.
//
// Widening rather than truncating is deliberate: the id is the string a user
// copies into `ccvault show` / `ccvault export`, and a shortened id is worse
// than a wide column.
func sessionIDColumnWidth(sessions []models.Session) int {
	width := sessionsIDMinWidth
	for _, s := range sessions {
		if w := visualWidthCLI(s.ID) + sessionsIDGutter; w > width {
			width = w
		}
	}
	return width
}

// sessionsTableWidth is the full visible width of the table, used for the
// separator rule so it always spans exactly the rendered columns.
func sessionsTableWidth(idWidth int, showProject bool) int {
	cols := idWidth + sessionsStartedWidth + sessionsTurnsWidth +
		sessionsTokensWidth + sessionsSubsWidth + sessionsModelWidth
	seps := 5
	if showProject {
		cols += sessionsProjectWidth
		seps++
	}
	return cols + seps
}

// renderSessionsTable formats the list-sessions table. Extracted from the
// command so the header/row/separator alignment is testable — the one
// property that makes the table readable at all.
func renderSessionsTable(sessions []models.Session, showProject bool, byPath map[string]*models.Project) string {
	idWidth := sessionIDColumnWidth(sessions)

	var b strings.Builder
	if showProject {
		fmt.Fprintf(&b, "%s %-*s %*s %*s %*s %*s %s\n",
			padVisualCLI("SESSION ID", idWidth),
			sessionsProjectWidth, "PROJECT",
			sessionsStartedWidth, "STARTED",
			sessionsTurnsWidth, "TURNS",
			sessionsTokensWidth, "TOKENS",
			sessionsSubsWidth, "SUBS",
			"MODEL")
	} else {
		fmt.Fprintf(&b, "%s %*s %*s %*s %*s %s\n",
			padVisualCLI("SESSION ID", idWidth),
			sessionsStartedWidth, "STARTED",
			sessionsTurnsWidth, "TURNS",
			sessionsTokensWidth, "TOKENS",
			sessionsSubsWidth, "SUBS",
			"MODEL")
	}
	b.WriteString(strings.Repeat("-", sessionsTableWidth(idWidth, showProject)))
	b.WriteString("\n")

	for _, s := range sessions {
		model := s.Model
		if len(model) > sessionsModelWidth {
			model = model[:sessionsModelWidth-3] + "..."
		}
		tokens := s.InputTokens + s.OutputTokens
		if showProject {
			// Class A — LabelFromPath surfaces adapter DisplayName
			// instead of falling through to basename. Route through
			// compact.Truncate so multibyte adapter labels don't get
			// byte-sliced (e.g. Cyrillic "Иванов-project").
			project := compact.Truncate(projectref.LabelFromPath(s.ProjectPath, byPath), sessionsProjectWidth-2).Text
			fmt.Fprintf(&b, "%s %s %*s %*d %*s %*s %s\n",
				padVisualCLI(s.ID, idWidth),
				padVisualCLI(project, sessionsProjectWidth),
				sessionsStartedWidth, s.StartedAt.Format("2006-01-02 15:04"),
				sessionsTurnsWidth, s.TurnCount,
				sessionsTokensWidth, formatTokens(tokens),
				sessionsSubsWidth, compact.SubagentCount(s.SubagentCount),
				model,
			)
		} else {
			fmt.Fprintf(&b, "%s %*s %*d %*s %*s %s\n",
				padVisualCLI(s.ID, idWidth),
				sessionsStartedWidth, s.StartedAt.Format("2006-01-02 15:04"),
				sessionsTurnsWidth, s.TurnCount,
				sessionsTokensWidth, formatTokens(tokens),
				sessionsSubsWidth, compact.SubagentCount(s.SubagentCount),
				model,
			)
		}
	}

	return b.String()
}

var listSessionsCmd = &cobra.Command{
	Use:   "list-sessions",
	Short: "List sessions",
	RunE: func(cmd *cobra.Command, args []string) error {
		jsonOutput, _ := cmd.Flags().GetBool("json")
		projectFilter, _ := cmd.Flags().GetString("project")
		limit, _ := cmd.Flags().GetInt("limit")
		includeSubagents, _ := cmd.Flags().GetBool("include-subagents")
		subagentsOf, _ := cmd.Flags().GetString("subagents-of")

		cfg, err := loadConfig(cmd)
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}

		database, err := db.Open(cfg.DataDir)
		if err != nil {
			return fmt.Errorf("open database: %w", err)
		}
		defer func() { _ = database.Close() }()

		// Get project ID if filter specified
		var projectID int64
		if projectFilter != "" {
			project, err := database.GetProjectByPath(projectFilter)
			if err != nil {
				return fmt.Errorf("get project: %w", err)
			}
			if project != nil {
				projectID = project.ID
			} else {
				// Fall back to partial match. Class D — return every match
				// via projectref.ResolveAll rather than silently picking one,
				// and refuse when ambiguous so the user picks a longer filter.
				projects, err := database.GetProjects("activity", 0)
				if err != nil {
					return fmt.Errorf("get projects: %w", err)
				}
				matches := projectref.ResolveAll(projects, projectFilter)
				switch len(matches) {
				case 0:
					return fmt.Errorf("project not found: %s", projectFilter)
				case 1:
					projectID = matches[0].ID
				default:
					lines := make([]string, len(matches))
					for i, m := range matches {
						lines[i] = "  - " + m.Path
					}
					return fmt.Errorf("multiple projects match %q; be more specific:\n%s",
						projectFilter, strings.Join(lines, "\n"))
				}
			}
		}

		scope, parentSessionID := subagentScopeFromFlags(includeSubagents, subagentsOf)
		sessions, err := database.QuerySessions(db.SessionQuery{
			ProjectID:       projectID,
			Limit:           limit,
			Scope:           scope,
			ParentSessionID: parentSessionID,
		})
		if err != nil {
			return fmt.Errorf("get sessions: %w", err)
		}

		if jsonOutput {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			// Class C — enrich with project_name so agents get the
			// doctrine {name, path} shape on session objects.
			allProjects, enrichErr := database.GetProjects("activity", 0)
			if enrichErr != nil {
				fmt.Fprintf(os.Stderr, "warning: project enrichment lookup failed: %v\n", enrichErr)
			}
			return enc.Encode(projectref.SessionRefsFromValues(sessions, projectref.ProjectsByID(allProjects)))
		}

		if len(sessions) == 0 {
			fmt.Println("No sessions found.")
			return nil
		}

		// PROJECT column is redundant when the list is already filtered to
		// one project — mirror the TUI's showProject := m.project == nil.
		showProject := projectFilter == ""
		// Load projects once so adapter DisplayNames (jeff/hex/nanoclaw)
		// surface in the PROJECT column.
		var byPath map[string]*models.Project
		if showProject {
			allProjects, enrichErr := database.GetProjects("activity", 0)
			if enrichErr != nil {
				fmt.Fprintf(os.Stderr, "warning: project enrichment lookup failed: %v — PROJECT column fell back to basename\n", enrichErr)
			}
			byPath = projectref.ProjectsByPath(allProjects)
		}
		fmt.Print(renderSessionsTable(sessions, showProject, byPath))

		return nil
	},
}

var showCmd = &cobra.Command{
	Use:   "show [session-id]",
	Short: "Show a specific session",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		sessionID := args[0]
		jsonOutput, _ := cmd.Flags().GetBool("json")

		cfg, err := loadConfig(cmd)
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}

		database, err := db.Open(cfg.DataDir)
		if err != nil {
			return fmt.Errorf("open database: %w", err)
		}
		defer func() { _ = database.Close() }()

		session, err := database.GetSession(sessionID)
		if err != nil {
			return fmt.Errorf("get session: %w", err)
		}
		if session == nil {
			return fmt.Errorf("session not found: %s", sessionID)
		}

		turns, err := database.GetTurns(sessionID)
		if err != nil {
			return fmt.Errorf("get turns: %w", err)
		}

		if jsonOutput {
			result := map[string]interface{}{
				"session": session,
				"turns":   turns,
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(result)
		}

		// Pretty print conversation
		fmt.Printf("Session: %s\n", session.ID)
		fmt.Printf("Model: %s\n", session.Model)
		fmt.Printf("Started: %s\n", session.StartedAt.Format("2006-01-02 15:04:05"))
		fmt.Printf("Turns: %d\n", len(turns))
		// The way a reader discovers that a session dispatched work into
		// transcripts the default listing doesn't show, and the way back up
		// from one of those transcripts to the session that started it.
		if session.ParentSessionID != "" {
			fmt.Printf("Subagent of: %s\n", session.ParentSessionID)
		}
		if session.SubagentCount > 0 {
			fmt.Printf("Subagents: %d (ccvault list-sessions --subagents-of %s)\n",
				session.SubagentCount, session.ID)
		}
		fmt.Println(strings.Repeat("=", 60))
		fmt.Println()

		for _, t := range turns {
			switch t.Type {
			case "user":
				fmt.Printf("[USER] %s\n", t.Timestamp.Format("15:04:05"))
				fmt.Println(t.Content)
				fmt.Println()
			case "assistant":
				fmt.Printf("[ASSISTANT] %s\n", t.Timestamp.Format("15:04:05"))
				content := t.Content
				if len(content) > 500 {
					content = content[:500] + "\n... (truncated)"
				}
				fmt.Println(content)
				fmt.Println()
			}
		}

		return nil
	},
}

var exportCmd = &cobra.Command{
	Use:   "export [session-id]",
	Short: "Export a session to markdown",
	Long: `Export a session to markdown format for easy reading or archival.

The exported markdown includes:
  - Session metadata (model, timestamps, tokens)
  - Full conversation with user and assistant messages
  - Tool usage details (commands, file paths, etc.)
  - Tool results (optional, can be disabled)
  - Thinking blocks (optional, collapsible)

Examples:
  ccvault export abc123-def456                    # Export to stdout
  ccvault export abc123-def456 -o session.md     # Export to file
  ccvault export abc123-def456 --no-thinking     # Exclude thinking blocks
  ccvault export abc123-def456 --no-tool-results # Exclude tool results`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		sessionID := args[0]
		outputPath, _ := cmd.Flags().GetString("output")
		includeThinking, _ := cmd.Flags().GetBool("thinking")
		includeToolResults, _ := cmd.Flags().GetBool("tool-results")

		cfg, err := loadConfig(cmd)
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}

		database, err := db.Open(cfg.DataDir)
		if err != nil {
			return fmt.Errorf("open database: %w", err)
		}
		defer func() { _ = database.Close() }()

		session, err := database.GetSession(sessionID)
		if err != nil {
			return fmt.Errorf("get session: %w", err)
		}
		if session == nil {
			return fmt.Errorf("session not found: %s", sessionID)
		}

		turns, err := database.GetTurns(sessionID)
		if err != nil {
			return fmt.Errorf("get turns: %w", err)
		}

		// Get project path for metadata
		var projectPath string
		if session.ProjectID > 0 {
			project, err := database.GetProject(session.ProjectID)
			if err == nil && project != nil {
				projectPath = project.Path
			}
		}

		// Create exporter
		exporter := export.NewMarkdownExporter(
			export.WithThinking(includeThinking),
			export.WithToolResults(includeToolResults),
		)

		// Determine output destination
		var writer *os.File
		if outputPath != "" {
			writer, err = os.Create(outputPath)
			if err != nil {
				return fmt.Errorf("create output file: %w", err)
			}
			defer func() { _ = writer.Close() }()
		} else {
			writer = os.Stdout
		}

		// Export
		if err := exporter.Export(writer, session, turns, projectPath); err != nil {
			return fmt.Errorf("export: %w", err)
		}

		if outputPath != "" {
			fmt.Fprintf(os.Stderr, "Exported session to %s\n", outputPath)
		}

		return nil
	},
}

var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Start MCP server",
	Long: `Start the MCP server for AI assistant integration.

The server uses JSON-RPC 2.0 over stdio (Model Context Protocol).

Tools:
  - search_conversations: Full-text search across conversations
  - get_session: Retrieve a specific session (JSON or markdown)
  - list_sessions: List recent sessions
  - list_projects: List all indexed projects
  - get_stats: Get archive statistics
  - get_analytics: Get detailed usage analytics

Prompts:
  - summarize_recent: Summarize recent activity
  - analyze_project: Analyze a specific project
  - find_solutions: Find past solutions for a topic
  - review_session: Review a specific session
  - compare_approaches: Compare approaches across sessions
  - tool_usage_report: Analyze tool usage patterns

Debug mode: Set CCVAULT_MCP_DEBUG=1 for verbose logging to stderr.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig(cmd)
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}

		database, err := db.Open(cfg.DataDir)
		if err != nil {
			return fmt.Errorf("open database: %w", err)
		}
		defer func() { _ = database.Close() }()

		server, err := mcp.NewServer(database, cfg)
		if err != nil {
			return fmt.Errorf("create server: %w", err)
		}
		defer func() { _ = server.Close() }()

		return server.Run()
	},
}

var buildCacheCmd = &cobra.Command{
	Use:   "build-cache",
	Short: "Rebuild analytics cache",
	Long:  `Rebuild the Parquet analytics cache for fast DuckDB queries.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig(cmd)
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}

		database, err := db.Open(cfg.DataDir)
		if err != nil {
			return fmt.Errorf("open database: %w", err)
		}
		defer func() { _ = database.Close() }()

		cacheDir := filepath.Join(cfg.DataDir, "analytics")
		exporter := analytics.NewExporter(database, cacheDir)
		exporter.SetProgressCallback(func(msg string) {
			fmt.Println(msg)
		})

		return exporter.Export()
	},
}

var importCmd = &cobra.Command{
	Use:     "import <database-path>",
	Aliases: []string{"merge"},
	Short:   "Merge another ccvault database into this archive",
	Long: `Merge the sessions, turns, tool uses, and projects of another ccvault
database into this one.

Import only ever ADDS. A session absent here is inserted along with its turns
and tool uses. A session present in both is left alone unless the incoming
copy ended later, in which case the incoming copy replaces it wholesale.
Nothing already in this archive is deleted, and the source database is only
read, never modified.

The whole merge runs in one transaction, so a failure leaves this archive
exactly as it was.

Use it to recover from a destructive rebuild:

  ccvault import ~/.ccvault/backups/ccvault-20260905-140418.db

or to consolidate the archives of two machines into one.`,
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		asJSON, _ := cmd.Flags().GetBool("json")

		cfg, err := loadConfig(cmd)
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}
		if err := config.EnsureDataDir(cfg); err != nil {
			return fmt.Errorf("create data dir: %w", err)
		}

		database, err := db.Open(cfg.DataDir)
		if err != nil {
			return fmt.Errorf("open database: %w", err)
		}
		defer func() { _ = database.Close() }()

		stats, err := database.MergeFrom(args[0])
		if err != nil {
			return fmt.Errorf("import: %w", err)
		}

		if asJSON {
			out, err := json.MarshalIndent(stats, "", "  ")
			if err != nil {
				return fmt.Errorf("marshal stats: %w", err)
			}
			fmt.Println(string(out))
			return nil
		}

		fmt.Printf("Imported from %s\n", args[0])
		fmt.Printf("  Projects:  %d added\n", stats.ProjectsInserted)
		fmt.Printf("  Sessions:  %d added, %d replaced with newer copies, %d already current\n",
			stats.SessionsInserted, stats.SessionsReplaced, stats.SessionsSkipped)
		fmt.Printf("  Turns:     %d\n", stats.TurnsInserted)
		fmt.Printf("  Tool uses: %d\n", stats.ToolUsesInserted)
		if stats.SessionsInserted+stats.SessionsReplaced > 0 {
			fmt.Println("\nRebuild the analytics cache to pick up the imported rows:")
			fmt.Println("  ccvault build-cache")
		}
		return nil
	},
}

func init() {
	// Persistent overrides, available to every subcommand. --data-dir says
	// where the archive lives; --config says which file to read settings
	// from. They are separate because a config file can itself set
	// data_dir, and because a fixture often needs one without the other.
	rootCmd.PersistentFlags().String("data-dir", "",
		"Data directory holding the archive. Overrides CCVAULT_DATA_DIR, the config file, and the ~/.ccvault default, and makes this directory the only place config.toml is looked for")
	rootCmd.PersistentFlags().String("config", "",
		"Config file to read (default: config.toml in the data directory, then ./config.toml). A path that does not exist is an error")

	// Add commands
	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(quickstartCmd)
	rootCmd.AddCommand(orientCmd)
	rootCmd.AddCommand(syncCmd)
	rootCmd.AddCommand(tuiCmd)
	rootCmd.AddCommand(searchCmd)
	rootCmd.AddCommand(statsCmd)
	rootCmd.AddCommand(vacuumCmd)
	rootCmd.AddCommand(listProjectsCmd)
	rootCmd.AddCommand(listSessionsCmd)
	rootCmd.AddCommand(showCmd)
	rootCmd.AddCommand(exportCmd)
	rootCmd.AddCommand(mcpCmd)
	rootCmd.AddCommand(buildCacheCmd)
	rootCmd.AddCommand(importCmd)

	// Orient flags
	orientCmd.Flags().Bool("json", false, "Output as JSON for machine parsing")

	// Stats flags
	statsCmd.Flags().Bool("json", false, "Output as JSON for machine parsing")

	// Vacuum flags
	vacuumCmd.Flags().Bool("json", false, "Output as JSON for machine parsing")

	// Sync flags
	syncCmd.Flags().Bool("full", false, "Re-parse every session file, ignoring mtimes. Not destructive — rows whose source file is gone are kept")
	syncCmd.Flags().Bool("rebuild", false, "WIPES all indexed data then re-scans, DESTROYING anything the source has pruned. Prompts for confirmation + takes a backup by default")
	syncCmd.Flags().Bool("yes", false, "Skip the --rebuild confirmation prompt (assumes 'yes')")
	syncCmd.Flags().Bool("no-backup", false, "Skip the pre-wipe SQLite backup when running --rebuild (dangerous)")
	syncCmd.Flags().BoolP("verbose", "v", false, "Show verbose output")
	syncCmd.Flags().String("source", "", "Sync only the configured source with this name (matches sources[].name from config)")

	// Import flags
	importCmd.Flags().Bool("json", false, "Output merge counts as JSON")

	// Search flags
	searchCmd.Flags().Bool("json", false, "Output results as JSON")
	searchCmd.Flags().Int("limit", 20, "Maximum number of results")

	// List flags
	listProjectsCmd.Flags().Bool("json", false, "Output as JSON")
	listProjectsCmd.Flags().String("sort", "activity", "Sort by: name, activity, tokens, sessions")
	listProjectsCmd.Flags().Int("limit", 50, "Maximum number of results")
	listSessionsCmd.Flags().Bool("json", false, "Output as JSON")
	listSessionsCmd.Flags().String("project", "", "Filter by project")
	listSessionsCmd.Flags().Int("limit", 50, "Maximum number of results")
	listSessionsCmd.Flags().Bool("include-subagents", false, "List subagent sessions alongside top-level ones")
	listSessionsCmd.Flags().String("subagents-of", "", "List only the subagent sessions dispatched by this session id")

	// Show flags
	showCmd.Flags().Bool("json", false, "Output as JSON")

	// Export flags
	exportCmd.Flags().StringP("output", "o", "", "Output file path (default: stdout)")
	exportCmd.Flags().Bool("thinking", true, "Include thinking blocks")
	exportCmd.Flags().Bool("tool-results", true, "Include tool results")
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// formatTokens formats a token count for display
func formatTokens(n int64) string {
	if n >= 1_000_000_000 {
		return fmt.Sprintf("%.1fB", float64(n)/1_000_000_000)
	}
	if n >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
	if n >= 1_000 {
		return fmt.Sprintf("%.1fK", float64(n)/1_000)
	}
	return fmt.Sprintf("%d", n)
}

// formatBytes formats a byte count for display. Binary units, because the
// numbers it describes are page counts times a power-of-two page size.
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	for _, suffix := range []string{"KB", "MB", "GB", "TB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1f PB", value/unit)
}

// visualWidthCLI returns the visible column count of s (runes, not bytes).
func visualWidthCLI(s string) int {
	visW := 0
	for range s {
		visW++
	}
	return visW
}

// padVisualCLI left-pads s to visual column width using rune count.
// Mirrors internal/tui.padVisual — sprintf's %-Ns pads by bytes and
// would misalign columns when compact.* returns strings containing "…"
// (3 bytes / 1 col) or non-ASCII path segments.
func padVisualCLI(s string, width int) string {
	visW := visualWidthCLI(s)
	if visW >= width {
		return s
	}
	return s + strings.Repeat(" ", width-visW)
}

// prepareRebuild runs the safety block for `sync --rebuild`: it shows
// current row counts, prompts for confirmation (unless assumeYes is set
// or stdin isn't a TTY), takes a VACUUM INTO backup to
// <dataDir>/backups/ccvault-<timestamp>.db, and rotates to the most
// recent 5 backups. Returns the backup path (empty when --no-backup)
// so the caller can print a restore hint if the subsequent re-scan
// fails. An error from any step aborts the sync before any wiping.
func prepareRebuild(database *db.DB, dataDir string, assumeYes, noBackup bool) (string, error) {
	// Gather counts for the user-facing confirmation.
	projects, sessions, turns, err := rebuildCounts(database)
	if err != nil {
		return "", fmt.Errorf("gather counts: %w", err)
	}

	fmt.Fprintf(os.Stderr, "\n"+
		"⚠ --rebuild WIPES the ccvault DB before re-scanning:\n"+
		"    Projects: %d\n"+
		"    Sessions: %d\n"+
		"    Turns:    %d\n",
		projects, sessions, turns)

	// The rows worth shouting about are the ones the re-scan cannot bring
	// back, because their source file no longer exists upstream.
	if orphans, err := sessionsWithoutSourceFiles(database); err != nil {
		fmt.Fprintf(os.Stderr, "  warning: could not check which sessions still have source files: %v\n", err)
	} else if orphans > 0 {
		fmt.Fprintf(os.Stderr,
			"\n  %d of those sessions have NO source file on disk any more.\n"+
				"  The re-scan CANNOT re-create them — they exist only here.\n"+
				"  If you want to re-parse everything without losing them, use --full instead.\n",
			orphans)
	}

	if assumeYes {
		fmt.Fprintln(os.Stderr, "  (--yes supplied — proceeding without prompt)")
	} else if !isStdinTTY() {
		// Piped stdin, cron, CI: no interactive prompt possible. Refuse
		// rather than silently wipe — the caller must opt in with --yes.
		return "", fmt.Errorf("aborted: stdin is not a TTY and --yes was not supplied; refusing to wipe data non-interactively")
	} else {
		fmt.Fprint(os.Stderr, "\nType 'yes' to confirm the wipe: ")
		var answer string
		_, _ = fmt.Fscanln(os.Stdin, &answer)
		if answer != "yes" {
			return "", fmt.Errorf("aborted: confirmation not received (got %q)", answer)
		}
	}

	if noBackup {
		fmt.Fprintln(os.Stderr, "  (--no-backup supplied — skipping backup step)")
		return "", nil
	}

	backupDir := filepath.Join(dataDir, "backups")
	if err := os.MkdirAll(backupDir, 0o750); err != nil {
		return "", fmt.Errorf("create backup dir: %w", err)
	}
	timestamp := time.Now().UTC().Format("20060102-150405")
	backupPath := filepath.Join(backupDir, fmt.Sprintf("ccvault-%s.db", timestamp))

	fmt.Fprintf(os.Stderr, "  Backing up to %s ... ", backupPath)
	if err := database.BackupTo(backupPath); err != nil {
		fmt.Fprintln(os.Stderr, "FAILED")
		return "", fmt.Errorf("backup: %w", err)
	}
	fmt.Fprintln(os.Stderr, "done")

	if pruned, err := pruneOldBackups(backupDir, 5); err != nil {
		fmt.Fprintf(os.Stderr, "  warning: could not rotate old backups: %v\n", err)
	} else if pruned > 0 {
		fmt.Fprintf(os.Stderr, "  Rotated %d older backup(s)\n", pruned)
	}
	fmt.Fprintln(os.Stderr)

	return backupPath, nil
}

// sessionsWithoutSourceFiles counts archived sessions whose source_file no
// longer exists on disk. Those are the rows a rebuild destroys for good: the
// re-scan can only re-create what it can still read.
func sessionsWithoutSourceFiles(database *db.DB) (int, error) {
	// Grouped so each path is stat'd once even when several sessions share it.
	rows, err := database.Query(
		"SELECT source_file, COUNT(*) FROM sessions WHERE source_file != '' GROUP BY source_file")
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()

	missing := 0
	for rows.Next() {
		var path string
		var sessions int
		if err := rows.Scan(&path, &sessions); err != nil {
			return 0, err
		}
		if _, err := os.Stat(path); os.IsNotExist(err) {
			missing += sessions
		}
	}
	return missing, rows.Err()
}

// rebuildCounts returns the row counts shown in the confirmation prompt.
// A stat failure doesn't abort — 0s are surfaced so the user sees they
// couldn't be gathered but can still proceed.
func rebuildCounts(database *db.DB) (int, int, int, error) {
	projects, _, err := database.GetProjectStats()
	if err != nil {
		return 0, 0, 0, err
	}
	sessions, turns, _, err := database.GetSessionStats()
	if err != nil {
		return projects, 0, 0, err
	}
	return projects, sessions, turns, nil
}

// isStdinTTY returns true when stdin is attached to an interactive
// terminal — used to decide whether the confirmation prompt makes
// sense. Piped input (`echo yes | ccvault sync --full`), redirected
// input (`< /dev/null`), and scripted invocation all return false;
// they must pass --yes to opt in.
//
// term.IsTerminal issues an ioctl and is stricter than checking
// os.ModeCharDevice — /dev/null is a character device but not a
// terminal, and misclassifying it opens a scripted-wipe hole.
func isStdinTTY() bool {
	// os.Stdin.Fd() returns uintptr; on Unix it's always a small fd
	// (0, 1, 2, low positive). The int conversion is standard for
	// x/term.IsTerminal's signature and cannot overflow in practice.
	return term.IsTerminal(int(os.Stdin.Fd())) //nolint:gosec // G115: fd is always a small non-negative int
}

// pruneOldBackups keeps the most recent `keep` files matching
// ccvault-*.db in dir and deletes the older ones. Returns how many were
// removed. Files with unparseable names are ignored (left in place).
func pruneOldBackups(dir string, keep int) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	// Collect ccvault-*.db backups.
	type entry struct {
		name string
		path string
	}
	var backups []entry
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "ccvault-") || !strings.HasSuffix(name, ".db") {
			continue
		}
		backups = append(backups, entry{name: name, path: filepath.Join(dir, name)})
	}
	// Timestamp is the filename between "ccvault-" and ".db", format
	// YYYYMMDD-HHMMSS — lexicographic sort matches chronological sort.
	// Newest first.
	sort.Slice(backups, func(i, j int) bool { return backups[i].name > backups[j].name })
	if len(backups) <= keep {
		return 0, nil
	}
	pruned := 0
	for _, b := range backups[keep:] {
		if err := os.Remove(b.path); err != nil {
			return pruned, fmt.Errorf("remove %s: %w", b.path, err)
		}
		pruned++
	}
	return pruned, nil
}
