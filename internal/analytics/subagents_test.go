// ABOUTME: Tests that analytics counts subagent sessions rather than filtering them.
// ABOUTME: The hidden-by-default listing rule is a listing rule, not an accounting rule.

package analytics

import (
	"testing"
	"time"

	"github.com/2389-research/ccvault/internal/db"
)

// TestAnalyticsCountsSubagentSessions is the loud half of this change: a
// subagent transcript's tokens are additive, not duplicated — parent
// transcripts contain no sidechain lines at all — so counting them raises the
// totals. On the author's archive the 63 parents with ingested subagents hold
// 2,905,646 tokens and their subagents another 832,723, about 29% more.
func TestAnalyticsCountsSubagentSessions(t *testing.T) {
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	now := time.Now().UTC()
	seedSession(t, database, now, "/tmp/proj-a", "proj-a", "parent-a",
		"claude-opus-4", "claude-code", 100, 50, 0)
	seedSubagentSession(t, database, now.Add(time.Minute), "/tmp/proj-a", "claude-code:parent-a:agent-a1",
		"parent-a", 1000, 500)

	// The summary the TUI and MCP read is driven by these totals.
	count, turns, tokens, err := database.GetSessionStats()
	if err != nil {
		t.Fatalf("session stats: %v", err)
	}
	if count != 2 {
		t.Errorf("session count = %d, want 2 — analytics must count subagent sessions", count)
	}
	if turns != 4 {
		t.Errorf("turn count = %d, want 4", turns)
	}
	if tokens != 1650 {
		t.Errorf("token total = %d, want 1650 (150 parent + 1500 subagent)", tokens)
	}

	// And the parquet export, which backs the DuckDB analytics views.
	cacheDir := t.TempDir()
	if err := NewExporter(database, cacheDir).Export(); err != nil {
		t.Fatalf("export: %v", err)
	}
	analyzer, err := NewAnalyzer(cacheDir)
	if err != nil {
		t.Fatalf("new analyzer: %v", err)
	}
	t.Cleanup(func() { _ = analyzer.Close() })

	summary, err := analyzer.GetSummary()
	if err != nil {
		t.Fatalf("get summary: %v", err)
	}
	if summary.TotalSessions != 2 {
		t.Errorf("parquet TotalSessions = %d, want 2", summary.TotalSessions)
	}
	if summary.TotalTokens != 1650 {
		t.Errorf("parquet TotalTokens = %d, want 1650", summary.TotalTokens)
	}
}

// seedSubagentSession inserts a session row linked to a parent, in the shape
// the claude-code adapter now mints.
func seedSubagentSession(t *testing.T, d *db.DB, when time.Time, projectPath, sessionID, parentID string, inputTok, outputTok int64) {
	t.Helper()

	proj, err := d.GetProjectByPath(projectPath)
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	if proj == nil {
		t.Fatalf("project %s must exist before its subagent session", projectPath)
	}

	_, err = d.Exec(`INSERT INTO sessions
		(id, project_id, model, git_branch, started_at, ended_at, turn_count, input_tokens, output_tokens,
		 cache_read_tokens, cache_write_tokens, source_file, source, parent_session_id)
		VALUES (?, ?, 'claude-opus-4', 'main', ?, ?, 2, ?, ?, 0, 0, ?, 'claude-code', ?)`,
		sessionID, proj.ID, when, when, inputTok, outputTok,
		"/tmp/parent-a/subagents/agent-a1.jsonl", parentID)
	if err != nil {
		t.Fatalf("insert subagent session: %v", err)
	}
}
