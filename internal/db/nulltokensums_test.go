// ABOUTME: Tests that project token reconciliation survives a NULL token column
// ABOUTME: Covers both reconcile paths — ReconcileProjectAggregates and MergeFrom (issue #110)

package db

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/2389-research/ccvault/pkg/models"
)

// insertSessionWithOneNullToken writes a session whose cache_write_tokens is
// explicitly NULL and whose other three counters hold real values.
//
// Raw SQL because UpsertSession cannot produce this row: models.Session holds
// the counters as plain integers, so the write path always has a value for the
// column. The schema permits NULL in all four (migration 001 — the DEFAULT 0
// applies to an omitted column, not to an explicit NULL), and an archive
// written by anything other than this binary can carry one.
func insertSessionWithOneNullToken(t *testing.T, db *DB, id string, projectID int64) {
	t.Helper()

	_, err := db.Exec(
		`INSERT INTO sessions (id, project_id, started_at, ended_at,
			turn_count, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
			source_file, source)
		 VALUES (?, ?, ?, ?, 1, 100, 20, 3, NULL, ?, 'claude-code')`,
		id, projectID, time.Now().Add(-time.Hour), time.Now(),
		filepath.Join(t.TempDir(), id+".jsonl"))
	if err != nil {
		t.Fatalf("insert session with NULL cache_write_tokens: %v", err)
	}
}

// TestReconcileProjectAggregatesCoalescesNullTokens is the aggregate form of
// the #64 defect, reported as #110.
//
// `a + b + c + d` is NULL in SQL if any operand is NULL, and SUM skips a NULL
// input rather than failing on it. So one session holding a single NULL counter
// used to contribute nothing at all to its project's total — not its three real
// counters, and not an error either. Two sessions here, one clean and one with
// a NULL, so the assertion distinguishes "the NULL row contributed zero" from
// "the whole total came out NULL".
func TestReconcileProjectAggregatesCoalescesNullTokens(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	project := &models.Project{
		Path:           "/Users/test/null-tokens",
		DisplayName:    "null-tokens",
		FirstSeenAt:    time.Now(),
		LastActivityAt: time.Now(),
	}
	if err := db.UpsertProject(project); err != nil {
		t.Fatalf("upsert project: %v", err)
	}

	clean := &models.Session{
		ID:               "null-tokens-clean",
		ProjectID:        project.ID,
		StartedAt:        time.Now(),
		EndedAt:          time.Now(),
		InputTokens:      1000,
		OutputTokens:     200,
		CacheReadTokens:  30,
		CacheWriteTokens: 4,
		SourceFile:       filepath.Join(t.TempDir(), "clean.jsonl"),
	}
	if err := db.UpsertSession(clean); err != nil {
		t.Fatalf("upsert clean session: %v", err)
	}

	insertSessionWithOneNullToken(t, db, "null-tokens-nulled", project.ID)

	if err := db.ReconcileProjectAggregates([]string{project.Path}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got, err := db.GetProjectByPath(project.Path)
	if err != nil {
		t.Fatalf("read project: %v", err)
	}

	// 1234 from the clean session, 123 from the three non-NULL counters of the
	// other one.
	const want = 1234 + 123
	if got.TotalTokens != want {
		t.Errorf("total_tokens = %d, want %d (the NULL counter must read as 0, "+
			"not zero out its whole session)", got.TotalTokens, want)
	}
	if got.SessionCount != 2 {
		t.Errorf("session_count = %d, want 2", got.SessionCount)
	}
}

// TestMergeFromCoalescesNullTokens covers the second copy of the same SQL.
//
// MergeFrom recomputes project aggregates from the sessions that exist after
// the merge, using its own statement against the `main.` schema, so fixing
// ReconcileProjectAggregates alone leaves the undercount reachable through
// `ccvault merge`.
func TestMergeFromCoalescesNullTokens(t *testing.T) {
	source, sourceCleanup := setupTestDB(t)
	defer sourceCleanup()

	project := &models.Project{
		Path:           "/Users/test/merge-null-tokens",
		DisplayName:    "merge-null-tokens",
		FirstSeenAt:    time.Now(),
		LastActivityAt: time.Now(),
	}
	if err := source.UpsertProject(project); err != nil {
		t.Fatalf("upsert project: %v", err)
	}
	insertSessionWithOneNullToken(t, source, "merge-null-tokens-session", project.ID)
	sourcePath := source.Path()
	if err := source.Close(); err != nil {
		t.Fatalf("close source: %v", err)
	}

	dest, destCleanup := setupTestDB(t)
	defer destCleanup()

	if _, err := dest.MergeFrom(sourcePath); err != nil {
		t.Fatalf("merge: %v", err)
	}

	got, err := dest.GetProjectByPath(project.Path)
	if err != nil {
		t.Fatalf("read merged project: %v", err)
	}
	if got.TotalTokens != 123 {
		t.Errorf("merged total_tokens = %d, want 123 (the NULL counter must read as 0)",
			got.TotalTokens)
	}
}
