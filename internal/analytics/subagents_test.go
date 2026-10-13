// ABOUTME: Tests that analytics counts subagent sessions rather than filtering them.
// ABOUTME: The hidden-by-default listing rule is a listing rule, not an accounting rule.

package analytics

import (
	"fmt"
	"path/filepath"
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

	// Two turns each, matching the turn_count the seeds record. The archive-wide
	// turn figure is counted off the turns table rather than summed from that
	// counter (#91), so a seed that sets the counter without writing the rows
	// would be asserting against a figure nothing reads.
	for _, sessionID := range []string{"parent-a", "claude-code:parent-a:agent-a1"} {
		for ordinal := 0; ordinal < 2; ordinal++ {
			if _, err := database.Exec(
				`INSERT INTO turns (id, session_id, type, timestamp, content, ordinal)
				 VALUES (?, ?, 'user', ?, 'c', ?)`,
				fmt.Sprintf("%s-turn-%d", sessionID, ordinal), sessionID, now, ordinal); err != nil {
				t.Fatalf("seed turn %d of %s: %v", ordinal, sessionID, err)
			}
		}
	}

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

// TestExportedSessionRecordCarriesParent is the quiet half: counting subagent
// sessions is not the same as being able to tell them apart afterwards. The
// parquet cache is the only ccvault artifact a third party queries directly
// (DuckDB, pandas, anything that reads a file), so a session row that drops
// its parent makes the split unrecoverable downstream — there is no join back
// to SQLite from a copied parquet file.
//
// Top-level sessions carry the empty string rather than NULL, matching
// models.Session, where "" already means "no parent" on every other surface.
func TestExportedSessionRecordCarriesParent(t *testing.T) {
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

	cacheDir := t.TempDir()
	if err := NewExporter(database, cacheDir).Export(); err != nil {
		t.Fatalf("export: %v", err)
	}
	analyzer, err := NewAnalyzer(cacheDir)
	if err != nil {
		t.Fatalf("new analyzer: %v", err)
	}
	t.Cleanup(func() { _ = analyzer.Close() })

	// Read the column back through DuckDB rather than the parquet-go reader:
	// DuckDB is how this file is actually consumed, so a column that only a
	// Go struct can see would pass a reader-side assertion and still be
	// useless to every query that matters.
	// The path is bound rather than interpolated: production queries in
	// queries.go build it with fmt.Sprintf and carry a gosec G201 waiver for
	// doing so, and a test has no reason to inherit the waiver.
	rows, err := analyzer.db.Query(
		`SELECT id, parent_session_id FROM read_parquet(?) ORDER BY id`,
		filepath.Join(cacheDir, "sessions.parquet"),
	)
	if err != nil {
		t.Fatalf("query parquet: %v", err)
	}
	defer func() { _ = rows.Close() }()

	parents := map[string]string{}
	for rows.Next() {
		var id, parent string
		if err := rows.Scan(&id, &parent); err != nil {
			t.Fatalf("scan: %v", err)
		}
		parents[id] = parent
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	if got := parents["claude-code:parent-a:agent-a1"]; got != "parent-a" {
		t.Errorf("subagent parent_session_id = %q, want %q", got, "parent-a")
	}
	if got := parents["parent-a"]; got != "" {
		t.Errorf("top-level parent_session_id = %q, want empty", got)
	}
}

// TestExportedSubagentSpendIsSeparable is the point of carrying the column:
// one DuckDB query over the cache alone must be able to split the spend.
//
// It matters less than #74's headline claims and more than the raw share
// suggests. Measured on the author's 47,409-session archive on 2026-10-13,
// subagents are 362 sessions (0.8%), 46,656 turns (4.4%) and 11.5M of 145.7M
// tokens (7.9%) — not the 49%/35% the issue asserted, which described disk
// rather than the archive (#111). But the two populations have very different
// shapes: a subagent session averages 31,807 tokens against a top-level
// session's 2,851, eleven times as much. A per-session mean over the undivided
// pool is a mean over neither population.
func TestExportedSubagentSpendIsSeparable(t *testing.T) {
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

	cacheDir := t.TempDir()
	if err := NewExporter(database, cacheDir).Export(); err != nil {
		t.Fatalf("export: %v", err)
	}
	analyzer, err := NewAnalyzer(cacheDir)
	if err != nil {
		t.Fatalf("new analyzer: %v", err)
	}
	t.Cleanup(func() { _ = analyzer.Close() })

	const query = `
		SELECT
			COALESCE(SUM(CASE WHEN parent_session_id <> '' THEN total_tokens END), 0),
			COALESCE(SUM(CASE WHEN parent_session_id = '' THEN total_tokens END), 0)
		FROM read_parquet(?)`
	var subagentTokens, topLevelTokens int64
	err = analyzer.db.QueryRow(query, filepath.Join(cacheDir, "sessions.parquet")).
		Scan(&subagentTokens, &topLevelTokens)
	if err != nil {
		t.Fatalf("query parquet: %v", err)
	}

	if subagentTokens != 1500 {
		t.Errorf("subagent tokens = %d, want 1500", subagentTokens)
	}
	if topLevelTokens != 150 {
		t.Errorf("top-level tokens = %d, want 150", topLevelTokens)
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
