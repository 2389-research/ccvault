// ABOUTME: Tests for the versioned database migration system
// ABOUTME: Validates fresh installs, idempotency, and bootstrap from pre-migrator databases

package db

import (
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func openMemoryDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open memory db: %v", err)
	}
	return db
}

func TestMigrator_FreshDatabase(t *testing.T) {
	db := openMemoryDB(t)
	defer func() { _ = db.Close() }()

	if err := RunMigrations(db); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	// Verify schema_version has correct version
	var maxVersion int
	err := db.QueryRow("SELECT MAX(version) FROM schema_version").Scan(&maxVersion)
	if err != nil {
		t.Fatalf("query max version: %v", err)
	}
	if maxVersion != 5 {
		t.Errorf("max version = %d, want 5", maxVersion)
	}

	// Count migration records
	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM schema_version").Scan(&count)
	if err != nil {
		t.Fatalf("count schema_version: %v", err)
	}
	if count != 5 {
		t.Errorf("schema_version count = %d, want 5", count)
	}

	// Verify all core tables exist
	tables := []string{"projects", "sessions", "turns", "tool_uses", "turns_fts", "sync_state", "source_files"}
	for _, table := range tables {
		var n int
		err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type IN ('table', 'view') AND name=?", table).Scan(&n)
		if err != nil {
			t.Fatalf("check table %s: %v", table, err)
		}
		if n != 1 {
			t.Errorf("table %s not found", table)
		}
	}

	// Verify has_error and has_subagent columns exist on sessions
	rows, err := db.Query("PRAGMA table_info(sessions)")
	if err != nil {
		t.Fatalf("pragma table_info: %v", err)
	}
	defer func() { _ = rows.Close() }()

	columnFound := map[string]bool{"has_error": false, "has_subagent": false}
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull int
		var dfltValue sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err != nil {
			t.Fatalf("scan column info: %v", err)
		}
		if _, ok := columnFound[name]; ok {
			columnFound[name] = true
		}
	}

	for col, found := range columnFound {
		if !found {
			t.Errorf("column %s not found on sessions table", col)
		}
	}

	// Verify triggers exist
	triggers := []string{"turns_ai", "turns_ad", "turns_au"}
	for _, trigger := range triggers {
		var n int
		err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name=?", trigger).Scan(&n)
		if err != nil {
			t.Fatalf("check trigger %s: %v", trigger, err)
		}
		if n != 1 {
			t.Errorf("trigger %s not found", trigger)
		}
	}
}

func TestMigrator_ExistingDatabase(t *testing.T) {
	db := openMemoryDB(t)
	defer func() { _ = db.Close() }()

	// Run migrations twice
	if err := RunMigrations(db); err != nil {
		t.Fatalf("first RunMigrations: %v", err)
	}
	if err := RunMigrations(db); err != nil {
		t.Fatalf("second RunMigrations: %v", err)
	}

	// Verify exactly 5 migration records, not 10
	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM schema_version").Scan(&count)
	if err != nil {
		t.Fatalf("count schema_version: %v", err)
	}
	if count != 5 {
		t.Errorf("schema_version count = %d, want 5 (idempotent)", count)
	}
}

func TestMigrator_BootstrapExisting(t *testing.T) {
	db := openMemoryDB(t)
	defer func() { _ = db.Close() }()

	// Simulate a pre-migrator database by creating the tables manually
	// (as the old schema.sql + ad-hoc ALTERs would have done)
	stmts := []string{
		`CREATE TABLE projects (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			path TEXT UNIQUE NOT NULL,
			display_name TEXT NOT NULL,
			first_seen_at DATETIME,
			last_activity_at DATETIME,
			session_count INTEGER DEFAULT 0,
			total_tokens INTEGER DEFAULT 0
		)`,
		`CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			project_id INTEGER REFERENCES projects(id),
			started_at DATETIME NOT NULL,
			ended_at DATETIME,
			model TEXT,
			git_branch TEXT,
			turn_count INTEGER DEFAULT 0,
			input_tokens INTEGER DEFAULT 0,
			output_tokens INTEGER DEFAULT 0,
			cache_read_tokens INTEGER DEFAULT 0,
			cache_write_tokens INTEGER DEFAULT 0,
			source_file TEXT NOT NULL,
			source_mtime DATETIME,
			has_error BOOLEAN DEFAULT 0,
			has_subagent BOOLEAN DEFAULT 0
		)`,
		`CREATE TABLE turns (
			id TEXT PRIMARY KEY,
			session_id TEXT REFERENCES sessions(id) ON DELETE CASCADE,
			parent_id TEXT,
			type TEXT NOT NULL,
			timestamp DATETIME NOT NULL,
			content TEXT,
			raw_json TEXT,
			input_tokens INTEGER DEFAULT 0,
			output_tokens INTEGER DEFAULT 0
		)`,
		`CREATE TABLE tool_uses (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			turn_id TEXT REFERENCES turns(id) ON DELETE CASCADE,
			session_id TEXT REFERENCES sessions(id) ON DELETE CASCADE,
			tool_name TEXT NOT NULL,
			file_path TEXT,
			timestamp DATETIME NOT NULL
		)`,
		`CREATE TABLE sync_state (key TEXT PRIMARY KEY, value TEXT)`,
		`CREATE TABLE source_files (path TEXT PRIMARY KEY, mtime DATETIME NOT NULL, synced_at DATETIME NOT NULL)`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("setup pre-migrator table: %v", err)
		}
	}

	// Run the migrator on this pre-existing database
	if err := RunMigrations(db); err != nil {
		t.Fatalf("RunMigrations on pre-existing db: %v", err)
	}

	// Verify it bootstrapped to version 2 then applied migrations 003, 004, and 005
	var maxVersion int
	err := db.QueryRow("SELECT MAX(version) FROM schema_version").Scan(&maxVersion)
	if err != nil {
		t.Fatalf("query max version: %v", err)
	}
	if maxVersion != 5 {
		t.Errorf("max version = %d, want 5", maxVersion)
	}

	// Verify exactly 5 records (2 bootstrapped + 3 applied)
	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM schema_version").Scan(&count)
	if err != nil {
		t.Fatalf("count schema_version: %v", err)
	}
	if count != 5 {
		t.Errorf("schema_version count = %d, want 5", count)
	}
}

func TestMigrator_BootstrapPartial(t *testing.T) {
	db := openMemoryDB(t)
	defer func() { _ = db.Close() }()

	// Simulate a database with only the initial schema (no has_error/has_subagent)
	stmts := []string{
		`CREATE TABLE projects (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			path TEXT UNIQUE NOT NULL,
			display_name TEXT NOT NULL
		)`,
		`CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			project_id INTEGER,
			started_at DATETIME NOT NULL,
			source_file TEXT NOT NULL
		)`,
		`CREATE TABLE turns (
			id TEXT PRIMARY KEY,
			session_id TEXT,
			type TEXT NOT NULL,
			timestamp DATETIME NOT NULL,
			content TEXT
		)`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("setup table: %v", err)
		}
	}

	// Run migrator — should bootstrap to version 1 and apply migrations 002-005
	if err := RunMigrations(db); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	// Verify version is now 5 (bootstrapped to 1, applied 002, 003, 004, and 005)
	var maxVersion int
	err := db.QueryRow("SELECT MAX(version) FROM schema_version").Scan(&maxVersion)
	if err != nil {
		t.Fatalf("query max version: %v", err)
	}
	if maxVersion != 5 {
		t.Errorf("max version = %d, want 5", maxVersion)
	}

	// Verify has_error column was added by migration 002
	rows, err := db.Query("PRAGMA table_info(sessions)")
	if err != nil {
		t.Fatalf("pragma: %v", err)
	}
	defer func() { _ = rows.Close() }()

	found := false
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull int
		var dfltValue sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err != nil {
			continue
		}
		if name == "has_error" {
			found = true
		}
	}
	if !found {
		t.Error("has_error column not added by migration 002")
	}
}

func TestMigrator_SourceColumns(t *testing.T) {
	db := openMemoryDB(t)
	defer func() { _ = db.Close() }()

	if err := RunMigrations(db); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	// Verify version is 5
	var maxVersion int
	err := db.QueryRow("SELECT MAX(version) FROM schema_version").Scan(&maxVersion)
	if err != nil {
		t.Fatalf("query max version: %v", err)
	}
	if maxVersion != 5 {
		t.Errorf("max version = %d, want 5", maxVersion)
	}

	// Insert a project row and verify the source column defaults to "claude-code"
	_, err = db.Exec(`INSERT INTO projects (path, display_name, first_seen_at, last_activity_at)
		VALUES ('/tmp/test', 'test-project', datetime('now'), datetime('now'))`)
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}

	var source string
	err = db.QueryRow("SELECT source FROM projects WHERE path = '/tmp/test'").Scan(&source)
	if err != nil {
		t.Fatalf("query project source: %v", err)
	}
	if source != "claude-code" {
		t.Errorf("project source = %q, want %q", source, "claude-code")
	}

	// Insert a session and verify source defaults to "claude-code"
	_, err = db.Exec(`INSERT INTO sessions (id, project_id, started_at, source_file)
		VALUES ('test-session-1', 1, datetime('now'), '/tmp/test.jsonl')`)
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}

	err = db.QueryRow("SELECT source FROM sessions WHERE id = 'test-session-1'").Scan(&source)
	if err != nil {
		t.Fatalf("query session source: %v", err)
	}
	if source != "claude-code" {
		t.Errorf("session source = %q, want %q", source, "claude-code")
	}

	// Verify source_files table also has the source column
	_, err = db.Exec(`INSERT INTO source_files (path, mtime, synced_at)
		VALUES ('/tmp/test.jsonl', datetime('now'), datetime('now'))`)
	if err != nil {
		t.Fatalf("insert source_file: %v", err)
	}

	err = db.QueryRow("SELECT source FROM source_files WHERE path = '/tmp/test.jsonl'").Scan(&source)
	if err != nil {
		t.Fatalf("query source_files source: %v", err)
	}
	if source != "claude-code" {
		t.Errorf("source_files source = %q, want %q", source, "claude-code")
	}
}

func TestMigration005CreatesRemotePushState(t *testing.T) {
	db := openMemoryDB(t)
	defer func() { _ = db.Close() }()

	if err := RunMigrations(db); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	// migrator ran — the table should exist and be empty
	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM remote_push_state").Scan(&count)
	if err != nil {
		t.Fatalf("query remote_push_state: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected empty table, got %d rows", count)
	}

	// Insert a row and verify the composite PK is enforced
	_, err = db.Exec(`
		INSERT INTO remote_push_state (remote_name, session_id, session_ended_at, pushed_at)
		VALUES ('origin', 'sess-1', '2026-07-21T10:00:00Z', '2026-07-21T10:00:01Z')
	`)
	if err != nil {
		t.Fatalf("insert row: %v", err)
	}

	_, err = db.Exec(`
		INSERT INTO remote_push_state (remote_name, session_id, session_ended_at, pushed_at)
		VALUES ('origin', 'sess-1', '2026-07-21T11:00:00Z', '2026-07-21T11:00:01Z')
	`)
	if err == nil {
		t.Fatal("expected PK conflict on duplicate (remote_name, session_id)")
	}
}

func TestSplitStatements(t *testing.T) {
	// Test that trigger blocks with internal semicolons are handled correctly
	input := `CREATE TABLE foo (id INTEGER);

CREATE TRIGGER bar AFTER INSERT ON foo BEGIN
    INSERT INTO baz(rowid, val) VALUES (new.rowid, new.val);
END;

CREATE INDEX idx_foo ON foo(id);`

	stmts := splitStatements(input)
	if len(stmts) != 3 {
		t.Fatalf("got %d statements, want 4", len(stmts))
	}

	if stmts[0] != "CREATE TABLE foo (id INTEGER);" {
		t.Errorf("stmt[0] = %q", stmts[0])
	}

	// The trigger should be one complete statement
	if !containsAll(stmts[1], "CREATE TRIGGER", "BEGIN", "END;") {
		t.Errorf("stmt[1] should be complete trigger, got: %q", stmts[1])
	}

	if stmts[2] != "CREATE INDEX idx_foo ON foo(id);" {
		t.Errorf("stmt[2] = %q", stmts[2])
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
