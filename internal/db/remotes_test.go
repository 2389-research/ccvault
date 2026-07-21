// ABOUTME: Tests for remote_push_state CRUD helpers
// ABOUTME: Exercises incremental push watermark logic

package db

import (
	"testing"
	"time"
)

func TestSessionsPendingPushIncludesUntrackedSessions(t *testing.T) {
	d := openMemoryVaultDB(t)
	defer func() { _ = d.Close() }()
	if err := RunMigrations(d.DB); err != nil {
		t.Fatal(err)
	}

	seedSession(t, d, "sess-a", "2026-07-21T10:00:00Z")

	ids, err := d.SessionsPendingPush("origin")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "sess-a" {
		t.Fatalf("want [sess-a], got %v", ids)
	}
}

func TestSessionsPendingPushExcludesAlreadyPushed(t *testing.T) {
	d := openMemoryVaultDB(t)
	defer func() { _ = d.Close() }()
	if err := RunMigrations(d.DB); err != nil {
		t.Fatal(err)
	}

	seedSession(t, d, "sess-a", "2026-07-21T10:00:00Z")

	ended, _ := time.Parse(time.RFC3339, "2026-07-21T10:00:00Z")
	if err := d.RecordPush("origin", "sess-a", ended); err != nil {
		t.Fatal(err)
	}

	ids, _ := d.SessionsPendingPush("origin")
	if len(ids) != 0 {
		t.Fatalf("want empty, got %v", ids)
	}
}

func TestSessionsPendingPushIncludesUpdatedSessions(t *testing.T) {
	d := openMemoryVaultDB(t)
	defer func() { _ = d.Close() }()
	if err := RunMigrations(d.DB); err != nil {
		t.Fatal(err)
	}

	seedSession(t, d, "sess-a", "2026-07-21T10:00:00Z")

	ended, _ := time.Parse(time.RFC3339, "2026-07-21T10:00:00Z")
	if err := d.RecordPush("origin", "sess-a", ended); err != nil {
		t.Fatal(err)
	}

	updated, _ := time.Parse(time.RFC3339, "2026-07-21T11:00:00Z")
	_, err := d.Exec("UPDATE sessions SET ended_at = ? WHERE id = ?",
		updated, "sess-a")
	if err != nil {
		t.Fatal(err)
	}

	ids, _ := d.SessionsPendingPush("origin")
	if len(ids) != 1 {
		t.Fatalf("want 1 pending session after update, got %v", ids)
	}
}

// openMemoryVaultDB returns a *DB backed by an in-memory SQLite connection.
// Wraps openMemoryDB so tests can call methods defined on *DB.
func openMemoryVaultDB(t *testing.T) *DB {
	t.Helper()
	return &DB{DB: openMemoryDB(t)}
}

// seedSession inserts a minimal project + session pair for testing.
// endedAt is an RFC3339 timestamp; it's parsed to time.Time so it lands in the
// DB in the same format that RecordPush (and production code) uses.
func seedSession(t *testing.T, d *DB, id, endedAt string) {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, endedAt)
	if err != nil {
		t.Fatalf("parse endedAt: %v", err)
	}
	_, err = d.Exec(`INSERT INTO projects (path, display_name, first_seen_at, last_activity_at, session_count, total_tokens, source)
        VALUES ('/tmp/p', 'p', ?, ?, 1, 0, 'claude-code')`, ts, ts)
	if err != nil {
		t.Fatal(err)
	}
	var pid int64
	if err := d.QueryRow("SELECT last_insert_rowid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	_, err = d.Exec(`INSERT INTO sessions (id, project_id, started_at, ended_at, turn_count, input_tokens, output_tokens, source_file, source, pushed_by)
        VALUES (?, ?, ?, ?, 0, 0, 0, '/tmp/p/x.jsonl', 'claude-code', '')`,
		id, pid, ts, ts)
	if err != nil {
		t.Fatal(err)
	}
}
