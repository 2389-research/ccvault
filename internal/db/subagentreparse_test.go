// ABOUTME: Tests migration 015's predicate — which transcripts it schedules for a re-parse, and which it must not.
// ABOUTME: The author's hand-made .worktrees/evals sessions are the rows a looser pattern match would have destroyed.

package db

import (
	"strings"
	"testing"
)

// migration015SQL returns the embedded text of migration 015, so the test runs
// the statement that ships rather than a copy of it.
func migration015SQL(t *testing.T) string {
	t.Helper()
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	for _, m := range migrations {
		if m.version == 15 {
			return m.sql
		}
	}
	t.Fatal("migration 015 not found")
	return ""
}

// seedSubagentReparseFixture lays down one row of each shape the predicate has
// to decide, and returns nothing: the test reads source_files back by path.
func seedSubagentReparseFixture(t *testing.T, db *DB) {
	t.Helper()

	stmts := []string{
		// Three projects: a repo, one agent worktree under it, and a
		// hand-made worktree directory the author uses for eval runs.
		`INSERT INTO projects (id, path, display_name, first_seen_at, last_activity_at)
		 VALUES (1, '/repo', 'repo', '2026-10-01T00:00:00.000000000Z', '2026-10-01T00:00:00.000000000Z')`,
		`INSERT INTO projects (id, path, display_name, first_seen_at, last_activity_at)
		 VALUES (2, '/repo/.claude/worktrees/agent-a1', 'agent-a1', '2026-10-01T00:00:00.000000000Z', '2026-10-01T00:00:00.000000000Z')`,
		`INSERT INTO projects (id, path, display_name, first_seen_at, last_activity_at)
		 VALUES (3, '/repo/.worktrees/evals', 'evals', '2026-10-01T00:00:00.000000000Z', '2026-10-01T00:00:00.000000000Z')`,

		// The dispatching session, correctly filed under the repo.
		`INSERT INTO sessions (id, project_id, source, source_file, started_at, ended_at)
		 VALUES ('parent', 1, 'claude-code', '/h/parent.jsonl', '2026-10-01T00:00:00.000000000Z', '2026-10-01T00:00:00.000000000Z')`,
		// Misfiled: a subagent of that session, sitting in the worktree it
		// ran in. This is the only row the migration should act on.
		`INSERT INTO sessions (id, project_id, parent_session_id, source, source_file, started_at, ended_at)
		 VALUES ('claude-code:parent:agent-a1', 2, 'parent', 'claude-code', '/h/agent-a1.jsonl', '2026-10-01T00:00:00.000000000Z', '2026-10-01T00:00:00.000000000Z')`,
		// Already correct: a subagent filed under its parent's project.
		`INSERT INTO sessions (id, project_id, parent_session_id, source, source_file, started_at, ended_at)
		 VALUES ('claude-code:parent:agent-a2', 1, 'parent', 'claude-code', '/h/agent-a2.jsonl', '2026-10-01T00:00:00.000000000Z', '2026-10-01T00:00:00.000000000Z')`,
		// A subagent whose dispatcher is not in the archive. There is
		// nothing to inherit from, so leave it where it is.
		`INSERT INTO sessions (id, project_id, parent_session_id, source, source_file, started_at, ended_at)
		 VALUES ('claude-code:gone:agent-a3', 2, 'gone', 'claude-code', '/h/agent-a3.jsonl', '2026-10-01T00:00:00.000000000Z', '2026-10-01T00:00:00.000000000Z')`,
		// A TOP-LEVEL session someone ran inside a hand-made worktree
		// himself. Correctly filed, no parent, and the row any pattern match
		// on "/.worktrees/" would have wrongly swept up.
		`INSERT INTO sessions (id, project_id, source, source_file, started_at, ended_at)
		 VALUES ('human-in-worktree', 3, 'claude-code', '/h/human.jsonl', '2026-10-01T00:00:00.000000000Z', '2026-10-01T00:00:00.000000000Z')`,
	}

	// One source_files row per session, plus a second source holding the
	// misfiled subagent's path so the (path, source) match can be checked.
	for _, path := range []string{"/h/parent.jsonl", "/h/agent-a1.jsonl", "/h/agent-a2.jsonl", "/h/agent-a3.jsonl", "/h/human.jsonl"} {
		stmts = append(stmts, `INSERT INTO source_files (path, source, mtime, synced_at)
			VALUES ('`+path+`', 'claude-code', '2026-10-01T00:00:00.000000000Z', '2026-10-01T00:00:00.000000000Z')`)
	}
	stmts = append(stmts, `INSERT INTO source_files (path, source, mtime, synced_at)
		VALUES ('/h/agent-a1.jsonl', 'nanoclaw', '2026-10-01T00:00:00.000000000Z', '2026-10-01T00:00:00.000000000Z')`)

	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed %q: %v", strings.Split(s, "\n")[0], err)
		}
	}
}

func TestMigration015SchedulesOnlyMisfiledSubagents(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	seedSubagentReparseFixture(t, database)

	// Open() has already applied 015 to an empty archive, so re-run it over
	// the seeded rows. Every statement in it is idempotent by construction.
	if _, err := database.Exec(migration015SQL(t)); err != nil {
		t.Fatalf("exec migration 015: %v", err)
	}

	rows, err := database.Query("SELECT path, source FROM source_files ORDER BY path, source")
	if err != nil {
		t.Fatalf("query source_files: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var got []string
	for rows.Next() {
		var path, source string
		if err := rows.Scan(&path, &source); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, path+" "+source)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}

	want := []string{
		// The misfiled subagent's claude-code row is gone; its nanoclaw
		// namesake is not, because the match is on (path, source).
		"/h/agent-a1.jsonl nanoclaw",
		"/h/agent-a2.jsonl claude-code",
		"/h/agent-a3.jsonl claude-code",
		"/h/human.jsonl claude-code",
		"/h/parent.jsonl claude-code",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("source_files after migration:\n got %v\nwant %v", got, want)
	}
}

// The counters and rows the migration must leave exactly as it found them. A
// re-parse is what moves the sessions; a migration that moved them itself
// would be a second spelling of the attribution rule, in SQL, over data it
// cannot read the parent transcript for.
func TestMigration015TouchesNothingButSourceFiles(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	seedSubagentReparseFixture(t, database)

	if _, err := database.Exec(migration015SQL(t)); err != nil {
		t.Fatalf("exec migration 015: %v", err)
	}

	var projectID int64
	if err := database.QueryRow(
		`SELECT project_id FROM sessions WHERE id = 'claude-code:parent:agent-a1'`).Scan(&projectID); err != nil {
		t.Fatalf("scan session project_id: %v", err)
	}
	if projectID != 2 {
		t.Errorf("session project_id = %d, want it still on the worktree row (2) until the re-parse", projectID)
	}

	var projects int
	if err := database.QueryRow(`SELECT COUNT(*) FROM projects`).Scan(&projects); err != nil {
		t.Fatalf("count projects: %v", err)
	}
	if projects != 3 {
		t.Errorf("projects = %d, want 3 — the migration drops no project row", projects)
	}
}
