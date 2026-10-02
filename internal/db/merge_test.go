// ABOUTME: Tests for merging another ccvault database into the current one
// ABOUTME: Covers insert-absent semantics, newer-row protection, project remapping, FTS

package db

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/2389-research/ccvault/pkg/models"
)

// seedSession writes one project + session + turn + tool_use into db.
// endedAt doubles as the session's recency marker, which is what the merge
// uses to decide whether an incoming row is newer than the one it would
// replace.
func seedSession(t *testing.T, database *DB, projectPath, sessionID, content string, endedAt time.Time) {
	t.Helper()

	project := &models.Project{
		Path:           projectPath,
		DisplayName:    filepath.Base(projectPath),
		FirstSeenAt:    endedAt.Add(-time.Hour),
		LastActivityAt: endedAt,
		SessionCount:   1,
		TotalTokens:    15,
		Source:         "claude-code",
	}
	if err := database.UpsertProject(project); err != nil {
		t.Fatalf("seed project %s: %v", projectPath, err)
	}

	session := &models.Session{
		ID:           sessionID,
		ProjectID:    project.ID,
		ProjectPath:  projectPath,
		StartedAt:    endedAt.Add(-time.Hour),
		EndedAt:      endedAt,
		Model:        "claude-opus-5",
		TurnCount:    1,
		InputTokens:  10,
		OutputTokens: 5,
		SourceFile:   "/fake/" + sessionID + ".jsonl",
		Source:       "claude-code",
	}
	if err := database.UpsertSession(session); err != nil {
		t.Fatalf("seed session %s: %v", sessionID, err)
	}

	turn := models.Turn{
		ID:        sessionID + "-turn-1",
		SessionID: sessionID,
		Type:      "user",
		Timestamp: endedAt,
		Content:   content,
	}
	if err := database.InsertTurns([]models.Turn{turn}); err != nil {
		t.Fatalf("seed turn for %s: %v", sessionID, err)
	}

	if err := database.InsertToolUses([]models.ToolUse{{
		TurnID:    turn.ID,
		SessionID: sessionID,
		ToolName:  "Bash",
		FilePath:  "/fake/script.sh",
		Timestamp: endedAt,
	}}); err != nil {
		t.Fatalf("seed tool_use for %s: %v", sessionID, err)
	}
}

// newSourceDB builds a standalone ccvault database in its own temp dir and
// returns it alongside its file path, closed and ready to be merged from.
func newSourceDB(t *testing.T, seed func(*DB)) string {
	t.Helper()

	dir := t.TempDir()
	database, err := Open(dir)
	if err != nil {
		t.Fatalf("open source db: %v", err)
	}
	seed(database)
	if err := database.Close(); err != nil {
		t.Fatalf("close source db: %v", err)
	}
	return filepath.Join(dir, "ccvault.db")
}

func countRows(t *testing.T, database *DB, query string, args ...interface{}) int {
	t.Helper()
	var n int
	if err := database.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return n
}

// TestMergeFrom_ReadsAnIncomingDatabaseWithAnUncheckpointedWAL covers the
// case WAL mode adds to `ccvault import`. Every other merge test closes the
// source first, and a clean close folds the log away — so none of them
// exercises a source whose newest sessions exist only in its -wal sidecar.
//
// That is the realistic shape of the feature's main use: an archive copied
// off another machine, or left behind by a process that died mid-sync. If
// ATTACH read the main file alone, the merge would silently import an
// archive missing its most recent sessions and report success.
func TestMergeFrom_ReadsAnIncomingDatabaseWithAnUncheckpointedWAL(t *testing.T) {
	// Build a source and copy main + sidecars away while it is still open,
	// so the copy keeps a log that was never folded in.
	srcDir := t.TempDir()
	source, err := Open(srcDir)
	if err != nil {
		t.Fatalf("open source db: %v", err)
	}
	seedSession(t, source, "/proj/incoming", "session-in-wal", "walmergecanary", time.Now())

	srcPath := filepath.Join(srcDir, "ccvault.db")
	if info, err := os.Stat(srcPath + "-wal"); err != nil {
		t.Fatalf("expected the source's writes to still be in its -wal: %v", err)
	} else if info.Size() == 0 {
		t.Fatal("source -wal is empty; the writes were already checkpointed")
	}

	copyDir := t.TempDir()
	copiedPath := filepath.Join(copyDir, "ccvault.db")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		data, err := os.ReadFile(srcPath + suffix)
		if err != nil {
			if os.IsNotExist(err) && suffix != "" {
				continue
			}
			t.Fatalf("read source%s: %v", suffix, err)
		}
		if err := os.WriteFile(copiedPath+suffix, data, 0o600); err != nil {
			t.Fatalf("write copy%s: %v", suffix, err)
		}
	}
	if err := source.Close(); err != nil {
		t.Fatalf("close source db: %v", err)
	}

	// The copy's main file alone must not contain the session, or this test
	// would pass without ATTACH ever consulting the log.
	if _, err := os.Stat(copiedPath + "-wal"); err != nil {
		t.Fatalf("copied -wal missing: %v", err)
	}

	dest, cleanup := setupTestDB(t)
	defer cleanup()

	stats, err := dest.MergeFrom(copiedPath)
	if err != nil {
		t.Fatalf("MergeFrom a WAL source: %v", err)
	}
	if stats.SessionsInserted != 1 {
		t.Errorf("sessions inserted = %d, want 1; the incoming -wal was not read", stats.SessionsInserted)
	}
	if n := countRows(t, dest, "SELECT COUNT(*) FROM turns WHERE session_id = ?", "session-in-wal"); n != 1 {
		t.Errorf("turns for session-in-wal = %d, want 1", n)
	}
	if n := countRows(t, dest, "SELECT COUNT(*) FROM turns_fts WHERE turns_fts MATCH ?", "walmergecanary"); n != 1 {
		t.Errorf("FTS matches = %d, want 1", n)
	}

	// Merging must not have left the destination's own journal mode behind.
	var mode string
	if err := dest.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("destination journal_mode: %v", err)
	}
	if !strings.EqualFold(mode, "wal") {
		t.Errorf("destination journal_mode = %q after merge, want wal", mode)
	}
}

func TestMergeFrom_InsertsSessionsAbsentFromDestination(t *testing.T) {
	dest, cleanup := setupTestDB(t)
	defer cleanup()

	now := time.Now().UTC().Truncate(time.Second)
	seedSession(t, dest, "/work/kept", "session-kept", "already here", now)

	src := newSourceDB(t, func(s *DB) {
		seedSession(t, s, "/work/kept", "session-kept", "already here", now)
		seedSession(t, s, "/work/archived", "session-archived", "only in the backup", now.Add(-48*time.Hour))
	})

	stats, err := dest.MergeFrom(src)
	if err != nil {
		t.Fatalf("MergeFrom: %v", err)
	}

	if stats.SessionsInserted != 1 {
		t.Errorf("SessionsInserted = %d, want 1", stats.SessionsInserted)
	}
	if stats.SessionsSkipped != 1 {
		t.Errorf("SessionsSkipped = %d, want 1 (the row already present)", stats.SessionsSkipped)
	}
	if stats.ProjectsInserted != 1 {
		t.Errorf("ProjectsInserted = %d, want 1", stats.ProjectsInserted)
	}

	if n := countRows(t, dest, "SELECT COUNT(*) FROM sessions"); n != 2 {
		t.Errorf("sessions = %d, want 2", n)
	}
	if n := countRows(t, dest, "SELECT COUNT(*) FROM turns WHERE session_id = 'session-archived'"); n != 1 {
		t.Errorf("imported turns = %d, want 1", n)
	}
	if n := countRows(t, dest, "SELECT COUNT(*) FROM tool_uses WHERE session_id = 'session-archived'"); n != 1 {
		t.Errorf("imported tool_uses = %d, want 1", n)
	}
	if n := countRows(t, dest, "SELECT COUNT(*) FROM projects WHERE path = '/work/archived'"); n != 1 {
		t.Errorf("imported project rows = %d, want 1", n)
	}
}

func TestMergeFrom_KeepsNewerDestinationRow(t *testing.T) {
	dest, cleanup := setupTestDB(t)
	defer cleanup()

	now := time.Now().UTC().Truncate(time.Second)
	seedSession(t, dest, "/work/app", "session-both", "the current content", now)

	// Source holds an older copy of the same session.
	src := newSourceDB(t, func(s *DB) {
		seedSession(t, s, "/work/app", "session-both", "the stale content", now.Add(-72*time.Hour))
	})

	stats, err := dest.MergeFrom(src)
	if err != nil {
		t.Fatalf("MergeFrom: %v", err)
	}
	if stats.SessionsReplaced != 0 {
		t.Errorf("SessionsReplaced = %d, want 0 (must never overwrite a newer row)", stats.SessionsReplaced)
	}

	var content string
	if err := dest.QueryRow("SELECT content FROM turns WHERE session_id = 'session-both'").Scan(&content); err != nil {
		t.Fatalf("read turn content: %v", err)
	}
	if content != "the current content" {
		t.Errorf("turn content = %q, want the destination's own (newer) copy", content)
	}
}

func TestMergeFrom_ReplacesOlderDestinationRow(t *testing.T) {
	dest, cleanup := setupTestDB(t)
	defer cleanup()

	now := time.Now().UTC().Truncate(time.Second)
	seedSession(t, dest, "/work/app", "session-both", "the truncated copy", now.Add(-72*time.Hour))

	src := newSourceDB(t, func(s *DB) {
		seedSession(t, s, "/work/app", "session-both", "the complete copy", now)
	})

	stats, err := dest.MergeFrom(src)
	if err != nil {
		t.Fatalf("MergeFrom: %v", err)
	}
	if stats.SessionsReplaced != 1 {
		t.Errorf("SessionsReplaced = %d, want 1", stats.SessionsReplaced)
	}

	var content string
	if err := dest.QueryRow("SELECT content FROM turns WHERE session_id = 'session-both'").Scan(&content); err != nil {
		t.Fatalf("read turn content: %v", err)
	}
	if content != "the complete copy" {
		t.Errorf("turn content = %q, want the incoming (newer) copy", content)
	}
	// Replacing must not leave both copies of the turn behind.
	if n := countRows(t, dest, "SELECT COUNT(*) FROM turns WHERE session_id = 'session-both'"); n != 1 {
		t.Errorf("turns for replaced session = %d, want 1", n)
	}
	if n := countRows(t, dest, "SELECT COUNT(*) FROM tool_uses WHERE session_id = 'session-both'"); n != 1 {
		t.Errorf("tool_uses for replaced session = %d, want 1 (no duplicates)", n)
	}
}

// TestMergeFrom_RemapsProjectIDs is the test that catches the obvious way to
// get this wrong: projects.id is an autoincrement local to each database, so
// copying sessions.project_id verbatim files imported sessions under whatever
// project happens to hold that id in the destination.
func TestMergeFrom_RemapsProjectIDs(t *testing.T) {
	dest, cleanup := setupTestDB(t)
	defer cleanup()

	now := time.Now().UTC().Truncate(time.Second)
	// Destination's project id 1 is "/work/alpha".
	seedSession(t, dest, "/work/alpha", "session-alpha", "alpha work", now)

	// Source's project id 1 is "/work/beta" — a verbatim copy would attach
	// session-beta to /work/alpha.
	src := newSourceDB(t, func(s *DB) {
		seedSession(t, s, "/work/beta", "session-beta", "beta work", now)
	})

	if _, err := dest.MergeFrom(src); err != nil {
		t.Fatalf("MergeFrom: %v", err)
	}

	var path string
	err := dest.QueryRow(`
		SELECT p.path FROM sessions s JOIN projects p ON s.project_id = p.id
		WHERE s.id = 'session-beta'`).Scan(&path)
	if err != nil {
		t.Fatalf("join imported session to project: %v", err)
	}
	if path != "/work/beta" {
		t.Errorf("imported session's project = %q, want /work/beta", path)
	}
}

func TestMergeFrom_ReconcilesProjectAggregates(t *testing.T) {
	dest, cleanup := setupTestDB(t)
	defer cleanup()

	now := time.Now().UTC().Truncate(time.Second)
	seedSession(t, dest, "/work/app", "session-1", "first", now)

	src := newSourceDB(t, func(s *DB) {
		seedSession(t, s, "/work/app", "session-2", "second", now.Add(-time.Hour))
		seedSession(t, s, "/work/app", "session-3", "third", now.Add(-2*time.Hour))
	})

	if _, err := dest.MergeFrom(src); err != nil {
		t.Fatalf("MergeFrom: %v", err)
	}

	var sessionCount int
	var totalTokens int64
	err := dest.QueryRow("SELECT session_count, total_tokens FROM projects WHERE path = '/work/app'").
		Scan(&sessionCount, &totalTokens)
	if err != nil {
		t.Fatalf("read aggregates: %v", err)
	}
	if sessionCount != 3 {
		t.Errorf("session_count = %d, want 3", sessionCount)
	}
	if totalTokens != 45 {
		t.Errorf("total_tokens = %d, want 45", totalTokens)
	}
}

// TestMergeFrom_ImportedTurnsAreSearchable guards the FTS index: imported
// turns that search can't reach are not recovered in any useful sense.
func TestMergeFrom_ImportedTurnsAreSearchable(t *testing.T) {
	dest, cleanup := setupTestDB(t)
	defer cleanup()

	now := time.Now().UTC().Truncate(time.Second)
	src := newSourceDB(t, func(s *DB) {
		seedSession(t, s, "/work/archived", "session-fts", "quuxamole deployment notes", now)
	})

	if _, err := dest.MergeFrom(src); err != nil {
		t.Fatalf("MergeFrom: %v", err)
	}

	results, err := dest.SearchTurns("quuxamole", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("search hits = %d, want 1 (imported turns must be in the FTS index)", len(results))
	}
	if results[0].SessionID != "session-fts" {
		t.Errorf("hit session = %q, want session-fts", results[0].SessionID)
	}
}

// TestMergeFrom_ReplacedTurnsLeaveNoFTSGhost: turns_fts is an external-content
// FTS5 index kept in step by triggers. If a replaced session's old turns stay
// in the index, search keeps returning content that no longer exists.
func TestMergeFrom_ReplacedTurnsLeaveNoFTSGhost(t *testing.T) {
	dest, cleanup := setupTestDB(t)
	defer cleanup()

	now := time.Now().UTC().Truncate(time.Second)
	seedSession(t, dest, "/work/app", "session-both", "zorblatt the stale copy", now.Add(-72*time.Hour))

	src := newSourceDB(t, func(s *DB) {
		seedSession(t, s, "/work/app", "session-both", "frobnax the complete copy", now)
	})

	if _, err := dest.MergeFrom(src); err != nil {
		t.Fatalf("MergeFrom: %v", err)
	}

	stale, err := dest.SearchTurns("zorblatt", 10)
	if err != nil {
		t.Fatalf("search stale: %v", err)
	}
	if len(stale) != 0 {
		t.Errorf("stale content still searchable (%d hits); the FTS index wasn't updated on replace", len(stale))
	}

	fresh, err := dest.SearchTurns("frobnax", 10)
	if err != nil {
		t.Fatalf("search fresh: %v", err)
	}
	if len(fresh) != 1 {
		t.Errorf("replacement content hits = %d, want 1", len(fresh))
	}
}

// TestMergeFrom_ToleratesSchemaDifference: an archive written by a different
// ccvault build can carry columns this one doesn't know about. The merge uses
// the columns the two databases share rather than refusing outright.
func TestMergeFrom_ToleratesSchemaDifference(t *testing.T) {
	dest, cleanup := setupTestDB(t)
	defer cleanup()

	now := time.Now().UTC().Truncate(time.Second)
	src := newSourceDB(t, func(s *DB) {
		seedSession(t, s, "/work/future", "session-future", "written by a later build", now)
		if _, err := s.Exec("ALTER TABLE sessions ADD COLUMN flavor TEXT DEFAULT 'vanilla'"); err != nil {
			t.Fatalf("add unknown column: %v", err)
		}
	})

	stats, err := dest.MergeFrom(src)
	if err != nil {
		t.Fatalf("MergeFrom with an unknown incoming column: %v", err)
	}
	if stats.SessionsInserted != 1 {
		t.Errorf("SessionsInserted = %d, want 1", stats.SessionsInserted)
	}
	if n := countRows(t, dest, "SELECT COUNT(*) FROM sessions WHERE id = 'session-future'"); n != 1 {
		t.Errorf("imported session rows = %d, want 1", n)
	}
}

func TestMergeFrom_RefusesMissingFile(t *testing.T) {
	dest, cleanup := setupTestDB(t)
	defer cleanup()

	if _, err := dest.MergeFrom(filepath.Join(t.TempDir(), "nope.db")); err == nil {
		t.Error("expected an error merging from a path that does not exist")
	}
}

func TestMergeFrom_RefusesSelfMerge(t *testing.T) {
	dest, cleanup := setupTestDB(t)
	defer cleanup()

	if _, err := dest.MergeFrom(dest.Path()); err == nil {
		t.Error("expected an error merging a database into itself")
	}
}

func TestMergeFrom_RefusesNonCcvaultDatabase(t *testing.T) {
	dest, cleanup := setupTestDB(t)
	defer cleanup()

	// A valid SQLite file with none of ccvault's tables.
	dir := t.TempDir()
	other, err := Open(dir)
	if err != nil {
		t.Fatalf("open other db: %v", err)
	}
	for _, table := range []string{"tool_uses", "turns", "sessions", "projects"} {
		if _, err := other.Exec("DROP TABLE IF EXISTS " + table); err != nil {
			t.Fatalf("drop %s: %v", table, err)
		}
	}
	if err := other.Close(); err != nil {
		t.Fatalf("close other db: %v", err)
	}

	if _, err := dest.MergeFrom(filepath.Join(dir, "ccvault.db")); err == nil {
		t.Error("expected an error merging a database with no ccvault tables")
	}
}

// TestMergeFrom_LeavesDestinationUsableAfterFailure checks the merge is
// transactional: a failure partway must not leave half the backup imported.
func TestMergeFrom_LeavesDestinationUsableAfterFailure(t *testing.T) {
	dest, cleanup := setupTestDB(t)
	defer cleanup()

	now := time.Now().UTC().Truncate(time.Second)
	seedSession(t, dest, "/work/app", "session-1", "first", now)

	// A file that is not a database at all: ATTACH fails, nothing is written.
	bogus := filepath.Join(t.TempDir(), "garbage.db")
	if err := os.WriteFile(bogus, []byte("this is not a sqlite file"), 0o600); err != nil {
		t.Fatalf("write bogus file: %v", err)
	}

	if _, err := dest.MergeFrom(bogus); err == nil {
		t.Fatal("expected an error merging a non-database file")
	}

	if n := countRows(t, dest, "SELECT COUNT(*) FROM sessions"); n != 1 {
		t.Errorf("sessions after failed merge = %d, want 1", n)
	}
	// And the connection is still usable for ordinary work afterwards.
	if _, err := dest.GetProjects("activity", 10); err != nil {
		t.Errorf("destination unusable after failed merge: %v", err)
	}
}
