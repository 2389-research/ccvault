// ABOUTME: Tests that every session read path survives a NULL in any nullable column
// ABOUTME: Covers model, git_branch, turn_count and the four token counters (issue #64)

package db

import (
	"testing"
	"time"
)

// insertSessionWithNullMetadata writes a session row with every nullable
// metadata column explicitly NULL.
//
// Raw SQL because UpsertSession cannot produce these NULLs — models.Session
// holds model and git_branch as plain strings and the counters as plain
// integers, so the write path always has a value to put in the column. The
// schema allows NULL in all seven (migration 001; the counters carry
// DEFAULT 0, which applies to an omitted column and not to an explicit NULL),
// and an archive produced by anything other than this binary can carry them.
func insertSessionWithNullMetadata(t *testing.T, db *DB, id, sourceFile string, projectID int64) {
	t.Helper()

	var project interface{}
	if projectID > 0 {
		project = projectID
	}

	_, err := db.Exec(
		`INSERT INTO sessions (id, project_id, started_at, ended_at, model, git_branch,
			turn_count, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
			source_file, source)
		 VALUES (?, ?, ?, ?, NULL, NULL, NULL, NULL, NULL, NULL, NULL, ?, 'claude-code')`,
		id, project, time.Now().Add(-time.Hour), time.Now(), sourceFile)
	if err != nil {
		t.Fatalf("insert session with NULL metadata: %v", err)
	}
}

// assertNullMetadataReadsAsZero checks the whole row, not just the column the
// scan happened to fail on first: scans stop at the first NULL, so a test that
// asserted one column at a time would pass as soon as that one was fixed.
func assertNullMetadataReadsAsZero(t *testing.T, where string, got sessionMetadata) {
	t.Helper()

	if got.model != "" {
		t.Errorf("%s: Model = %q, want empty for a NULL model", where, got.model)
	}
	if got.gitBranch != "" {
		t.Errorf("%s: GitBranch = %q, want empty for a NULL git_branch", where, got.gitBranch)
	}
	if got.turnCount != 0 {
		t.Errorf("%s: TurnCount = %d, want 0 for a NULL turn_count", where, got.turnCount)
	}
	if got.inputTokens != 0 || got.outputTokens != 0 || got.cacheReadTokens != 0 || got.cacheWriteTokens != 0 {
		t.Errorf("%s: token counters = (%d, %d, %d, %d), want all 0 for NULL columns",
			where, got.inputTokens, got.outputTokens, got.cacheReadTokens, got.cacheWriteTokens)
	}
}

type sessionMetadata struct {
	model            string
	gitBranch        string
	turnCount        int
	inputTokens      int64
	outputTokens     int64
	cacheReadTokens  int64
	cacheWriteTokens int64
}

// TestSessionReadPathsTolerateNullMetadata is #64: model and git_branch are
// nullable with no default, the counters are nullable with one, and all seven
// were scanned into non-nullable Go types. A NULL in any of them failed the
// scan outright, so the session was not merely incomplete — it was unreadable.
func TestSessionReadPathsTolerateNullMetadata(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	const sourceFile = "/proj/null-metadata/session.jsonl"
	insertSessionWithNullMetadata(t, db, "null-metadata-session", sourceFile, 0)

	s, err := db.GetSession("null-metadata-session")
	if err != nil {
		t.Fatalf("GetSession on NULL metadata: %v", err)
	}
	if s == nil {
		t.Fatal("GetSession returned no session for an existing row")
	}
	assertNullMetadataReadsAsZero(t, "GetSession", sessionMetadata{
		model: s.Model, gitBranch: s.GitBranch, turnCount: s.TurnCount,
		inputTokens: s.InputTokens, outputTokens: s.OutputTokens,
		cacheReadTokens: s.CacheReadTokens, cacheWriteTokens: s.CacheWriteTokens,
	})

	byFile, err := db.GetSessionBySourceFile(sourceFile)
	if err != nil {
		t.Fatalf("GetSessionBySourceFile on NULL metadata: %v", err)
	}
	if byFile == nil {
		t.Fatal("GetSessionBySourceFile returned no session for an existing row")
	}
	assertNullMetadataReadsAsZero(t, "GetSessionBySourceFile", sessionMetadata{
		model: byFile.Model, gitBranch: byFile.GitBranch, turnCount: byFile.TurnCount,
		inputTokens: byFile.InputTokens, outputTokens: byFile.OutputTokens,
		cacheReadTokens: byFile.CacheReadTokens, cacheWriteTokens: byFile.CacheWriteTokens,
	})

	page, err := db.GetSessionsPage(0, 0, 0)
	if err != nil {
		t.Fatalf("GetSessionsPage with a NULL metadata row present: %v", err)
	}
	if len(page) != 1 {
		t.Fatalf("GetSessionsPage returned %d sessions, want 1", len(page))
	}
	assertNullMetadataReadsAsZero(t, "GetSessionsPage", sessionMetadata{
		model: page[0].Model, gitBranch: page[0].GitBranch, turnCount: page[0].TurnCount,
		inputTokens: page[0].InputTokens, outputTokens: page[0].OutputTokens,
		cacheReadTokens: page[0].CacheReadTokens, cacheWriteTokens: page[0].CacheWriteTokens,
	})
}

// A NULL turn_count must not be counted as anything by the aggregate either —
// GetSessionStats already COALESCEs its SUMs, and this pins that down so a
// NULL row cannot make the archive-wide totals NULL.
func TestGetSessionStatsTolerateNullCounters(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	insertSessionWithNullMetadata(t, db, "null-counters-session", "/proj/null/a.jsonl", 0)

	// A row with only *some* counters NULL is the case a COALESCE around the
	// SUM does not cover: `a + b + c + d` is NULL as soon as one term is, so
	// SUM skips the row entirely and the 500 output tokens it really holds are
	// counted as none. Undercounting is the quieter failure of the two.
	_, err := db.Exec(
		`INSERT INTO sessions (id, project_id, started_at, ended_at, model, git_branch,
			turn_count, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
			source_file, source)
		 VALUES ('partial-counters-session', NULL, ?, ?, 'm', 'main', 3, NULL, 500, NULL, 0, ?, 'claude-code')`,
		time.Now().Add(-time.Hour), time.Now(), "/proj/null/b.jsonl")
	if err != nil {
		t.Fatalf("insert session with partially NULL counters: %v", err)
	}

	count, totalTurns, totalTokens, err := db.GetSessionStats()
	if err != nil {
		t.Fatalf("GetSessionStats with a NULL counter row present: %v", err)
	}
	if count != 2 {
		t.Errorf("session count = %d, want 2", count)
	}
	if totalTurns != 3 {
		t.Errorf("total turns = %d, want 3", totalTurns)
	}
	if totalTokens != 500 {
		t.Errorf("total tokens = %d, want 500", totalTokens)
	}
}
