// ABOUTME: Main entry point for ccvault CLI application
// ABOUTME: Initializes and executes the root command via Cobra

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	// Register source adapters so their init() functions run
	_ "github.com/2389-research/ccvault/pkg/adapter/claudecode"
	_ "github.com/2389-research/ccvault/pkg/adapter/codex"
	_ "github.com/2389-research/ccvault/pkg/adapter/hex"
	_ "github.com/2389-research/ccvault/pkg/adapter/jeff"
	_ "github.com/2389-research/ccvault/pkg/adapter/nanoclaw"

	"github.com/2389-research/ccvault/internal/analytics"
	"github.com/2389-research/ccvault/internal/config"
	"github.com/2389-research/ccvault/internal/db"
	"github.com/2389-research/ccvault/internal/export"
	"github.com/2389-research/ccvault/internal/mcp"
	"github.com/2389-research/ccvault/internal/search"
	"github.com/2389-research/ccvault/internal/sync"
	"github.com/2389-research/ccvault/internal/tui"
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
  - MCP server for AI assistant integration`,
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
		cfg, err := config.Load()
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

			stats, err := syncer.Run()
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
	ProjectCount  int
	SessionCount  int
	TurnCount     int
	SessionTokens int64
	FirstActivity time.Time
	LastActivity  time.Time
	ToolStats     map[string]int
	TokensByModel map[string]int64
	ProjectNames  []string
	Warnings      []string
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
	for _, p := range projects {
		o.ProjectNames = append(o.ProjectNames, p.DisplayName)
	}

	return o
}

var orientCmd = &cobra.Command{
	Use:   "orient",
	Short: "Output database state for AI agents",
	Long: `Output structured information about the ccvault database state.
Designed for AI agents to understand available data and commands.

Use --json for machine-readable output.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		jsonOutput, _ := cmd.Flags().GetBool("json")

		cfg, err := config.Load()
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
			"recent_projects": o.ProjectNames,
			"top_tools":       o.ToolStats,
			"models":          o.TokensByModel,
			"commands": map[string]string{
				"search <query>":      "Full-text search across conversations",
				"list-sessions":       "List recent sessions",
				"list-projects":       "List all projects",
				"show <session-id>":   "Display a specific session",
				"export <session-id>": "Export session to markdown",
				"stats":               "Show detailed statistics",
				"sync":                "Update with latest conversations",
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

		if len(o.ProjectNames) > 0 {
			fmt.Println("Recent Projects:")
			for _, name := range o.ProjectNames {
				fmt.Printf("  - %s\n", name)
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
	Long:  `Scan ~/.claude and index new or updated sessions into the ccvault database.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		full, _ := cmd.Flags().GetBool("full")
		verbose, _ := cmd.Flags().GetBool("verbose")
		sourceFilter, _ := cmd.Flags().GetString("source")

		// Load config
		cfg, err := config.Load()
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

		// Create syncer
		syncer := sync.New(database, sources,
			sync.WithFullSync(full),
			sync.WithVerbose(verbose),
			sync.WithProgressCallback(func(msg string) {
				fmt.Println(msg)
			}),
		)

		// Run sync
		stats, err := syncer.Run()
		if err != nil {
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

		if len(stats.Errors) > 0 {
			fmt.Printf("  Errors:    %d\n", len(stats.Errors))
			if verbose {
				for _, e := range stats.Errors {
					fmt.Printf("    - %v\n", e)
				}
			}
		}

		return nil
	},
}

var tuiCmd = &cobra.Command{
	Use:   "tui",
	Short: "Launch interactive TUI",
	Long:  `Open the interactive terminal UI for browsing and analyzing conversations.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
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

		cfg, err := config.Load()
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
		for i, r := range results {
			fmt.Printf("%d. [%s] %s\n", i+1, r.Turn.Type, r.Turn.Timestamp.Format("2006-01-02 15:04"))
			fmt.Printf("   Project: %s\n", r.ProjectPath)
			if r.Model != "" {
				fmt.Printf("   Model: %s\n", r.Model)
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
		cfg, err := config.Load()
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

var listProjectsCmd = &cobra.Command{
	Use:   "list-projects",
	Short: "List all indexed projects",
	RunE: func(cmd *cobra.Command, args []string) error {
		jsonOutput, _ := cmd.Flags().GetBool("json")
		sortBy, _ := cmd.Flags().GetString("sort")
		limit, _ := cmd.Flags().GetInt("limit")

		cfg, err := config.Load()
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
			return enc.Encode(projects)
		}

		if len(projects) == 0 {
			fmt.Println("No projects found. Run 'ccvault sync' first.")
			return nil
		}

		fmt.Printf("%-50s %8s %10s %12s\n", "PROJECT", "SESSIONS", "TOKENS", "LAST ACTIVE")
		fmt.Println(strings.Repeat("-", 85))
		for _, p := range projects {
			name := p.DisplayName
			if len(name) > 48 {
				name = "..." + name[len(name)-45:]
			}
			lastActive := p.LastActivityAt.Format("2006-01-02")
			fmt.Printf("%-50s %8d %10s %12s\n", name, p.SessionCount, formatTokens(p.TotalTokens), lastActive)
		}

		return nil
	},
}

var listSessionsCmd = &cobra.Command{
	Use:   "list-sessions",
	Short: "List sessions",
	RunE: func(cmd *cobra.Command, args []string) error {
		jsonOutput, _ := cmd.Flags().GetBool("json")
		projectFilter, _ := cmd.Flags().GetString("project")
		limit, _ := cmd.Flags().GetInt("limit")

		cfg, err := config.Load()
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
			if project == nil {
				// Try partial match
				projects, err := database.GetProjects("activity", 0)
				if err != nil {
					return fmt.Errorf("get projects: %w", err)
				}
				for _, p := range projects {
					if strings.Contains(strings.ToLower(p.Path), strings.ToLower(projectFilter)) ||
						strings.Contains(strings.ToLower(p.DisplayName), strings.ToLower(projectFilter)) {
						projectID = p.ID
						break
					}
				}
				if projectID == 0 {
					return fmt.Errorf("project not found: %s", projectFilter)
				}
			} else {
				projectID = project.ID
			}
		}

		sessions, err := database.GetSessions(projectID, limit)
		if err != nil {
			return fmt.Errorf("get sessions: %w", err)
		}

		if jsonOutput {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(sessions)
		}

		if len(sessions) == 0 {
			fmt.Println("No sessions found.")
			return nil
		}

		fmt.Printf("%-38s %16s %6s %10s %s\n", "SESSION ID", "STARTED", "TURNS", "TOKENS", "MODEL")
		fmt.Println(strings.Repeat("-", 100))
		for _, s := range sessions {
			model := s.Model
			if len(model) > 25 {
				model = model[:22] + "..."
			}
			tokens := s.InputTokens + s.OutputTokens
			fmt.Printf("%-38s %16s %6d %10s %s\n",
				s.ID,
				s.StartedAt.Format("2006-01-02 15:04"),
				s.TurnCount,
				formatTokens(tokens),
				model,
			)
		}

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

		cfg, err := config.Load()
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

		cfg, err := config.Load()
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
		cfg, err := config.Load()
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
		cfg, err := config.Load()
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

func init() {
	// Add commands
	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(quickstartCmd)
	rootCmd.AddCommand(orientCmd)
	rootCmd.AddCommand(syncCmd)
	rootCmd.AddCommand(tuiCmd)
	rootCmd.AddCommand(searchCmd)
	rootCmd.AddCommand(statsCmd)
	rootCmd.AddCommand(listProjectsCmd)
	rootCmd.AddCommand(listSessionsCmd)
	rootCmd.AddCommand(showCmd)
	rootCmd.AddCommand(exportCmd)
	rootCmd.AddCommand(mcpCmd)
	rootCmd.AddCommand(buildCacheCmd)

	// Orient flags
	orientCmd.Flags().Bool("json", false, "Output as JSON for machine parsing")

	// Sync flags
	syncCmd.Flags().Bool("full", false, "Force full rescan instead of incremental")
	syncCmd.Flags().BoolP("verbose", "v", false, "Show verbose output")
	syncCmd.Flags().String("source", "", "Sync only the configured source with this name (matches sources[].name from config)")

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
