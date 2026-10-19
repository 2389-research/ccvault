// ABOUTME: Tests DeleteEmptyProjects — the prune that disposes of a project row no session is filed under.
// ABOUTME: Keyed on the absence of a session, not on session_count, which UpsertProject maintains additively.

package db

import "testing"

func TestDeleteEmptyProjects(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	stmts := []string{
		`INSERT INTO projects (id, path, display_name, first_seen_at, last_activity_at, session_count)
		 VALUES (1, '/repo', 'repo', '2026-10-01T00:00:00.000000000Z', '2026-10-01T00:00:00.000000000Z', 1)`,
		// Abandoned: the stale session_count of 1 is what a re-parse that
		// moved the session elsewhere leaves behind, and is exactly why the
		// counter cannot be the predicate.
		`INSERT INTO projects (id, path, display_name, first_seen_at, last_activity_at, session_count)
		 VALUES (2, '/repo/.claude/worktrees/agent-a1', 'agent-a1', '2026-10-01T00:00:00.000000000Z', '2026-10-01T00:00:00.000000000Z', 1)`,
		// Never held a session and claims as much. Still garbage.
		`INSERT INTO projects (id, path, display_name, first_seen_at, last_activity_at, session_count)
		 VALUES (3, '/repo/.claude/worktrees/agent-a2', 'agent-a2', '2026-10-01T00:00:00.000000000Z', '2026-10-01T00:00:00.000000000Z', 0)`,
		`INSERT INTO sessions (id, project_id, source, source_file, started_at, ended_at)
		 VALUES ('s1', 1, 'claude-code', '/h/s1.jsonl', '2026-10-01T00:00:00.000000000Z', '2026-10-01T00:00:00.000000000Z')`,
	}
	for _, s := range stmts {
		if _, err := database.Exec(s); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	removed, err := database.DeleteEmptyProjects()
	if err != nil {
		t.Fatalf("DeleteEmptyProjects: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed = %d, want 2", removed)
	}

	var paths []string
	rows, err := database.Query("SELECT path FROM projects ORDER BY path")
	if err != nil {
		t.Fatalf("query projects: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatalf("scan: %v", err)
		}
		paths = append(paths, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	if len(paths) != 1 || paths[0] != "/repo" {
		t.Errorf("projects = %v, want only /repo", paths)
	}

	// Idempotent: nothing left to remove, and no error for saying so.
	if removed, err := database.DeleteEmptyProjects(); err != nil || removed != 0 {
		t.Errorf("second call = (%d, %v), want (0, nil)", removed, err)
	}
}
