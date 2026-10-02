// ABOUTME: Tests for subagent session rows: the parent_session_id column, the
// ABOUTME: migration 007 backfill, listing scopes, and subagent_count.

package db

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/2389-research/ccvault/pkg/models"
)

// insertSessionRow inserts a project-less session row directly so these tests stay
// focused on the parent/child plumbing rather than project upserts.
func insertSessionRow(t *testing.T, db *DB, id, parentID, sourceFile string, started time.Time) {
	t.Helper()
	var parent interface{}
	if parentID != "" {
		parent = parentID
	}
	// model/git_branch are written as '' rather than left NULL because that is
	// what the sync write path does — see issue #64, where those two columns
	// are nullable in the schema but scanned into plain strings.
	_, err := db.Exec(`INSERT INTO sessions (id, started_at, source_file, source, model, git_branch, parent_session_id)
		VALUES (?, ?, ?, 'claude-code', '', '', ?)`, id, started, sourceFile, parent)
	if err != nil {
		t.Fatalf("seed session %s: %v", id, err)
	}
}

func TestParentSessionIDColumnExists(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	rows, err := db.Query("PRAGMA table_info(sessions)")
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	defer func() { _ = rows.Close() }()

	found := false
	for rows.Next() {
		var cid, notNull, pk int
		var name, colType string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if name == "parent_session_id" {
			found = true
			if notNull != 0 {
				t.Error("parent_session_id must be nullable — NULL means top-level")
			}
		}
	}
	if !found {
		t.Fatal("sessions.parent_session_id column missing")
	}
}

// TestMigration007BackfillsComposite proves the backfill on the exact row shape
// nanoclaw already wrote: a pre-007 database whose subagent rows carry the
// relationship only inside their composite id. Non-vacuous by construction —
// the rows are inserted through a schema that stops at migration 006, so before
// 007 exists there is no column to backfill and this test cannot pass.
func TestMigration007BackfillsComposite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ccvault.db")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}

	// Build the pre-007 schema by applying only migrations below 007.
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE IF NOT EXISTS schema_version (
		version INTEGER NOT NULL,
		applied_at TEXT NOT NULL DEFAULT (datetime('now')))`); err != nil {
		t.Fatalf("schema_version: %v", err)
	}
	for _, m := range migrations {
		if m.version >= 7 {
			continue
		}
		if err := applyMigration(raw, m); err != nil {
			t.Fatalf("apply %03d: %v", m.version, err)
		}
	}

	const parent = "nanoclaw:845f7a4e-2827-4f87-8c31-2e4d0b429405"
	const child = "nanoclaw:845f7a4e-2827-4f87-8c31-2e4d0b429405:agent-a07c3516373ab4719"
	const orphan = "nanoclaw:00000000-0000-0000-0000-000000000000:agent-adeadbeefdeadbeef"
	const topLevel = "04fb5717-c508-4503-ac85-dc11787cafaa"

	seed := func(id, file string) {
		if _, err := raw.Exec(
			"INSERT INTO sessions (id, started_at, source_file, source) VALUES (?, ?, ?, 'nanoclaw')",
			id, time.Now(), file); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	seed(parent, "/n/sessions/reed/.claude/projects/-x/845f7a4e-2827-4f87-8c31-2e4d0b429405.jsonl")
	seed(child, "/n/sessions/reed/.claude/projects/-x/845f7a4e-2827-4f87-8c31-2e4d0b429405/subagents/agent-a07c3516373ab4719.jsonl")
	seed(orphan, "/n/sessions/reed/.claude/projects/-x/00000000-0000-0000-0000-000000000000/subagents/agent-adeadbeefdeadbeef.jsonl")
	seed(topLevel, "/Users/x/.claude/projects/-x/04fb5717-c508-4503-ac85-dc11787cafaa.jsonl")
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	// Reopening through db.Open runs the remaining migrations, 007 included.
	database, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = database.Close() }()

	parentOf := func(id string) (string, bool) {
		var p sql.NullString
		if err := database.QueryRow("SELECT parent_session_id FROM sessions WHERE id = ?", id).Scan(&p); err != nil {
			t.Fatalf("read parent of %s: %v", id, err)
		}
		return p.String, p.Valid
	}

	if got, ok := parentOf(child); !ok || got != parent {
		t.Errorf("child backfill = (%q, %v), want (%q, true)", got, ok, parent)
	}
	// An orphan whose parent was never ingested still gets the truthful link.
	if got, ok := parentOf(orphan); !ok || got != "nanoclaw:00000000-0000-0000-0000-000000000000" {
		t.Errorf("orphan backfill = (%q, %v), want the parsed parent id", got, ok)
	}
	if _, ok := parentOf(parent); ok {
		t.Error("a parent session must keep parent_session_id NULL")
	}
	if _, ok := parentOf(topLevel); ok {
		t.Error("a top-level claude-code session must keep parent_session_id NULL")
	}
}

func TestQuerySessionsScopes(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	insertSessionRow(t, db, "parent-a", "", "/p/a.jsonl", base)
	insertSessionRow(t, db, "claude-code:parent-a:agent-a1", "parent-a", "/p/a/subagents/agent-a1.jsonl", base.Add(time.Minute))
	insertSessionRow(t, db, "claude-code:parent-a:agent-a2", "parent-a", "/p/a/subagents/agent-a2.jsonl", base.Add(2*time.Minute))
	insertSessionRow(t, db, "parent-b", "", "/p/b.jsonl", base.Add(3*time.Minute))
	// A subagent whose parent is not in this archive. Hidden-by-default must
	// not swallow it: hidden is not the same as unreachable.
	insertSessionRow(t, db, "claude-code:missing:agent-z", "missing", "/p/missing/subagents/agent-z.jsonl", base.Add(4*time.Minute))

	ids := func(q SessionQuery) []string {
		t.Helper()
		sessions, err := db.QuerySessions(q)
		if err != nil {
			t.Fatalf("query sessions: %v", err)
		}
		out := make([]string, len(sessions))
		for i, s := range sessions {
			out[i] = s.ID
		}
		return out
	}

	got := ids(SessionQuery{Scope: SubagentsHidden})
	want := []string{"claude-code:missing:agent-z", "parent-b", "parent-a"}
	assertIDs(t, "hidden", got, want)

	got = ids(SessionQuery{Scope: SubagentsIncluded})
	want = []string{
		"claude-code:missing:agent-z", "parent-b",
		"claude-code:parent-a:agent-a2", "claude-code:parent-a:agent-a1", "parent-a",
	}
	assertIDs(t, "included", got, want)

	got = ids(SessionQuery{Scope: SubagentsOf, ParentSessionID: "parent-a"})
	want = []string{"claude-code:parent-a:agent-a2", "claude-code:parent-a:agent-a1"}
	assertIDs(t, "subagents-of", got, want)

	// SubagentsOf with no parent id would otherwise degenerate into "every
	// session", which is the opposite of what the caller asked for.
	if _, err := db.QuerySessions(SessionQuery{Scope: SubagentsOf}); err == nil {
		t.Error("SubagentsOf without ParentSessionID must error")
	}
}

func TestQuerySessionsCarriesSubagentCount(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	insertSessionRow(t, db, "parent-a", "", "/p/a.jsonl", base)
	insertSessionRow(t, db, "claude-code:parent-a:agent-a1", "parent-a", "/p/a/subagents/agent-a1.jsonl", base.Add(time.Minute))
	insertSessionRow(t, db, "claude-code:parent-a:agent-a2", "parent-a", "/p/a/subagents/agent-a2.jsonl", base.Add(2*time.Minute))
	insertSessionRow(t, db, "parent-b", "", "/p/b.jsonl", base.Add(3*time.Minute))

	sessions, err := db.QuerySessions(SessionQuery{Scope: SubagentsHidden})
	if err != nil {
		t.Fatalf("query sessions: %v", err)
	}
	counts := map[string]int{}
	for _, s := range sessions {
		counts[s.ID] = s.SubagentCount
	}
	if counts["parent-a"] != 2 {
		t.Errorf("parent-a subagent_count = %d, want 2", counts["parent-a"])
	}
	if counts["parent-b"] != 0 {
		t.Errorf("parent-b subagent_count = %d, want 0", counts["parent-b"])
	}

	// GetSession must carry both fields too — `show`/`get_session` are the
	// surfaces that reach a subagent with no flag.
	child, err := db.GetSession("claude-code:parent-a:agent-a1")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if child == nil {
		t.Fatal("subagent session not retrievable by id")
	}
	if child.ParentSessionID != "parent-a" {
		t.Errorf("child ParentSessionID = %q, want parent-a", child.ParentSessionID)
	}
	parent, err := db.GetSession("parent-a")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if parent.SubagentCount != 2 {
		t.Errorf("parent SubagentCount = %d, want 2", parent.SubagentCount)
	}
	if parent.ParentSessionID != "" {
		t.Errorf("parent ParentSessionID = %q, want empty", parent.ParentSessionID)
	}
}

// TestUpsertSessionRoundTripsParent proves the write path persists the link
// rather than dropping it the way sync did before this change (nanoclaw's
// adapter has always produced a parent_session_id in Metadata; nothing stored
// it, which is why all 133 ingested subagent rows were orphaned).
func TestUpsertSessionRoundTripsParent(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	parent := newTestSession("parent-a", "")
	if err := db.UpsertSession(parent); err != nil {
		t.Fatalf("upsert parent: %v", err)
	}
	child := newTestSession("claude-code:parent-a:agent-a1", "parent-a")
	if err := db.UpsertSession(child); err != nil {
		t.Fatalf("upsert child: %v", err)
	}

	got, err := db.GetSession(child.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.ParentSessionID != "parent-a" {
		t.Errorf("ParentSessionID = %q, want parent-a", got.ParentSessionID)
	}

	// Re-upserting must not clear the link.
	if err := db.UpsertSession(child); err != nil {
		t.Fatalf("re-upsert child: %v", err)
	}
	got, err = db.GetSession(child.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.ParentSessionID != "parent-a" {
		t.Errorf("after re-upsert ParentSessionID = %q, want parent-a", got.ParentSessionID)
	}
}

// TestSubagentIDDoesNotCollideWithParent is the regression guard for the reason
// scanner.go skipped subagents/ in the first place: a subagent transcript's
// in-band sessionId is its PARENT's uuid, so storing it under that id would
// overwrite the parent row on the sessions.id primary key.
func TestSubagentIDDoesNotCollideWithParent(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	const parentUUID = "04fb5717-c508-4503-ac85-dc11787cafaa"
	parent := newTestSession(parentUUID, "")
	parent.TurnCount = 305
	if err := db.UpsertSession(parent); err != nil {
		t.Fatalf("upsert parent: %v", err)
	}

	child := newTestSession("claude-code:"+parentUUID+":agent-a01b71e80ea28b3ad", parentUUID)
	child.TurnCount = 150
	if err := db.UpsertSession(child); err != nil {
		t.Fatalf("upsert child: %v", err)
	}

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Fatalf("session count = %d, want 2 (subagent must not overwrite its parent)", count)
	}
	kept, err := db.GetSession(parentUUID)
	if err != nil {
		t.Fatalf("get parent: %v", err)
	}
	if kept.TurnCount != 305 {
		t.Errorf("parent turn_count = %d, want 305 — the subagent clobbered it", kept.TurnCount)
	}
}

func newTestSession(id, parentID string) *models.Session {
	return &models.Session{
		ID:              id,
		StartedAt:       time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
		EndedAt:         time.Date(2026, 10, 1, 13, 0, 0, 0, time.UTC),
		Model:           "claude-opus-4",
		SourceFile:      "/p/" + id + ".jsonl",
		Source:          "claude-code",
		ParentSessionID: parentID,
	}
}

func assertIDs(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %v, want %v", label, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: got %v, want %v", label, got, want)
		}
	}
}
