// ABOUTME: Tests for turns.ordinal — the gapless per-session position — and
// ABOUTME: sessions.last_entry_uuid: the migration 008 backfill, the uniqueness
// ABOUTME: constraint, ordinal-ordered reads, the cursor, and merging a pre-008 archive.

package db

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/2389-research/ccvault/pkg/models"
)

// turnOrdinalFixture is one row of the deliberately adversarial seed the
// ordinal tests share: a session whose insertion order (and therefore file
// order) disagrees with its timestamps, both by going backwards and by tying.
//
// Those two shapes are not hypothetical. On the author's 965,061-turn archive,
// 6,597 adjacent turn pairs have a timestamp earlier than the turn inserted
// before them, and 19,160 share a timestamp exactly.
type turnOrdinalFixture struct {
	id          string
	sessionID   string
	turnType    string
	timestamp   time.Time
	wantOrdinal int
}

// adversarialTurns returns the seed in insertion order. Ordering these by
// (timestamp, rowid) — what issue #29 originally proposed — produces a
// different sequence than insertion order, so a test that asserts the wanted
// ordinals distinguishes the two rules rather than agreeing with both.
func adversarialTurns(base time.Time) []turnOrdinalFixture {
	return []turnOrdinalFixture{
		// Inserted first but timestamped last: a skewed clock.
		{"a-late", "session-a", "user", base.Add(3 * time.Second), 0},
		// Two turns sharing a timestamp to the millisecond.
		{"a-tie-1", "session-a", "assistant", base.Add(time.Second), 1},
		{"a-tie-2", "session-a", "assistant", base.Add(time.Second), 2},
		// Non-conversational types take part in the same sequence; a consumer
		// must not have to know which types participate.
		{"a-progress", "session-a", "progress", base.Add(2 * time.Second), 3},
		{"a-attachment", "session-a", "attachment", base.Add(2 * time.Second), 4},
		{"a-system", "session-a", "system", base.Add(4 * time.Second), 5},
		// A second session restarts at 0. Subagent sessions are their own
		// session row (migration 007), so this is also what makes a subagent's
		// sequence independent of its parent's without a special case.
		{"b-first", "session-b", "user", base.Add(-time.Hour), 0},
		{"b-second", "session-b", "assistant", base, 1},
	}
}

// seedPre008 builds a database holding the adversarial turns under the schema
// as it stood *below* migration 008, then closes it and returns its directory.
//
// Applying only the migrations below 008 is what makes the backfill tests
// non-vacuous: at seed time there is no ordinal column and no
// last_entry_uuid column, so nothing can write a correct value early and the
// assertions can only be satisfied by migration 008 running afterwards.
func seedPre008(t *testing.T, fixtures []turnOrdinalFixture) string {
	t.Helper()

	dir := t.TempDir()
	raw, err := sql.Open("sqlite", filepath.Join(dir, "ccvault.db"))
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}

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
		if m.version >= 8 {
			continue
		}
		if err := applyMigration(raw, m); err != nil {
			t.Fatalf("apply %03d: %v", m.version, err)
		}
	}

	// Guard the guard: if a future edit lets the ordinal column exist before
	// 008 runs, these tests would pass without proving anything.
	for _, probe := range []struct{ table, column string }{
		{"turns", "ordinal"},
		{"sessions", "last_entry_uuid"},
	} {
		if columnExists(t, raw, probe.table, probe.column) {
			t.Fatalf("%s.%s exists below migration 008, so the backfill tests prove nothing",
				probe.table, probe.column)
		}
	}

	for _, id := range []string{"session-a", "session-b", "session-empty"} {
		if _, err := raw.Exec(`INSERT INTO sessions (id, started_at, source_file, source, model, git_branch)
			VALUES (?, ?, ?, 'claude-code', '', '')`, id, fixtures[0].timestamp, "/fake/"+id+".jsonl"); err != nil {
			t.Fatalf("seed session %s: %v", id, err)
		}
	}

	// Insert in slice order so rowid reflects it. rowid is insertion order,
	// insertion order is file order, and file order is the ground truth.
	for _, f := range fixtures {
		if _, err := raw.Exec(
			`INSERT INTO turns (id, session_id, type, timestamp, content) VALUES (?, ?, ?, ?, ?)`,
			f.id, f.sessionID, f.turnType, f.timestamp, "content of "+f.id); err != nil {
			t.Fatalf("seed turn %s: %v", f.id, err)
		}
	}

	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}
	return dir
}

func columnExists(t *testing.T, raw *sql.DB, table, column string) bool {
	t.Helper()
	rows, err := raw.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		t.Fatalf("table_info(%s): %v", table, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var cid, notNull, pk int
		var name, colType string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		if name == column {
			return true
		}
	}
	return false
}

// TestMigration008BackfillsOrdinalsFromRowidNotTimestamp is the core backfill
// proof. The seed's timestamps contradict its insertion order, so the wanted
// ordinals are reachable only by ordering on rowid.
//
// Checked against the real archive before choosing the rule: of 1,413 sessions
// whose source .jsonl still exists on disk, ORDER BY rowid reproduced the
// file's line order for 1,412 and ORDER BY timestamp, rowid for only 1,336.
func TestMigration008BackfillsOrdinalsFromRowidNotTimestamp(t *testing.T) {
	base := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	fixtures := adversarialTurns(base)
	dir := seedPre008(t, fixtures)

	database, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = database.Close() }()

	for _, f := range fixtures {
		var got int
		if err := database.QueryRow("SELECT ordinal FROM turns WHERE id = ?", f.id).Scan(&got); err != nil {
			t.Fatalf("read ordinal of %s: %v", f.id, err)
		}
		if got != f.wantOrdinal {
			t.Errorf("turn %s: ordinal = %d, want %d", f.id, got, f.wantOrdinal)
		}
	}
}

// TestMigration008BackfillIsGaplessFromZero states the property the per-turn
// assertions above imply but do not name: every session's ordinals are exactly
// 0..n-1 with nothing missing and nothing repeated, over all turn types.
func TestMigration008BackfillIsGaplessFromZero(t *testing.T) {
	fixtures := adversarialTurns(time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC))
	dir := seedPre008(t, fixtures)

	database, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = database.Close() }()

	rows, err := database.Query(`
		SELECT session_id, COUNT(*), MIN(ordinal), MAX(ordinal), COUNT(DISTINCT ordinal)
		FROM turns GROUP BY session_id ORDER BY session_id`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer func() { _ = rows.Close() }()

	seen := 0
	for rows.Next() {
		var id string
		var n, min, max, distinct int
		if err := rows.Scan(&id, &n, &min, &max, &distinct); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen++
		if min != 0 || max != n-1 || distinct != n {
			t.Errorf("session %s: %d turns with ordinals min=%d max=%d distinct=%d, want 0..%d gapless",
				id, n, min, max, distinct, n-1)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if seen != 2 {
		t.Errorf("sessions with turns = %d, want 2", seen)
	}
}

// TestMigration008BackfillsLastEntryUUID covers the half of agentsview's
// pattern this work keeps. It names the turn at the end of the sequence, which
// is what lets a re-parse tell "resuming where I left off" from "this
// transcript was rewritten under me".
func TestMigration008BackfillsLastEntryUUID(t *testing.T) {
	fixtures := adversarialTurns(time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC))
	dir := seedPre008(t, fixtures)

	database, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = database.Close() }()

	lastOf := func(sessionID string) (string, bool) {
		var u sql.NullString
		if err := database.QueryRow(
			"SELECT last_entry_uuid FROM sessions WHERE id = ?", sessionID).Scan(&u); err != nil {
			t.Fatalf("read last_entry_uuid of %s: %v", sessionID, err)
		}
		return u.String, u.Valid
	}

	// The highest ordinal, not the latest timestamp: "a-system" is both here,
	// but "a-late" is the one a timestamp-descending rule would have picked
	// before the clock skew in the fixture.
	if got, ok := lastOf("session-a"); !ok || got != "a-system" {
		t.Errorf("session-a last_entry_uuid = (%q, %v), want (%q, true)", got, ok, "a-system")
	}
	if got, ok := lastOf("session-b"); !ok || got != "b-second" {
		t.Errorf("session-b last_entry_uuid = (%q, %v), want (%q, true)", got, ok, "b-second")
	}
	// A session with no turns has no last entry. NULL, not the empty string,
	// so no read path has to treat "" as a third state.
	if got, ok := lastOf("session-empty"); ok {
		t.Errorf("session-empty last_entry_uuid = %q, want NULL", got)
	}
}

// TestOrdinalIsUniqueWithinSessionAndFreeAcross pins the constraint that makes
// "the turns after position N" a sound cursor: a position identifies at most
// one turn in a session, and sessions do not share a numbering space.
func TestOrdinalIsUniqueWithinSessionAndFreeAcross(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	insert := func(id, sessionID string, ordinal int) error {
		_, err := database.Exec(
			`INSERT INTO turns (id, session_id, type, timestamp, ordinal) VALUES (?, ?, 'user', ?, ?)`,
			id, sessionID, time.Now(), ordinal)
		return err
	}

	if err := insert("t1", "s1", 0); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if err := insert("t2", "s2", 0); err != nil {
		t.Fatalf("ordinal 0 in a second session must be allowed: %v", err)
	}
	if err := insert("t3", "s1", 0); err == nil {
		t.Error("a repeated ordinal within one session must be rejected")
	}
}

// TestInsertTurnsRejectsRepeatedOrdinal covers the sharp edge the unique
// index alone leaves: the insert is INSERT OR REPLACE, and SQLite resolves a
// REPLACE against a unique index by deleting the row it conflicts with. A
// batch that forgot to assign ordinals therefore used to land as one surviving
// turn, silently, rather than as an error.
func TestInsertTurnsRejectsRepeatedOrdinal(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	now := time.Now()
	batch := []models.Turn{
		{ID: "t1", SessionID: "s1", Type: "user", Timestamp: now, Content: "first"},
		{ID: "t2", SessionID: "s1", Type: "assistant", Timestamp: now, Content: "second"},
	}

	err := database.InsertTurns(batch)
	if err == nil {
		t.Fatal("inserting two turns at the same ordinal must fail, not drop one")
	}
	for _, want := range []string{"t1", "t2", "ordinal 0"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}

	// Nothing landed: the whole batch is rejected before any of it executes.
	if n := countRows(t, database, "SELECT COUNT(*) FROM turns"); n != 0 {
		t.Errorf("turns after a rejected batch = %d, want 0", n)
	}
}

// TestGetTurnsOrdersByOrdinalNotTimestamp is the read-side half: ordering is
// deterministic even when the timestamps are not.
func TestGetTurnsOrdersByOrdinalNotTimestamp(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	base := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	fixtures := adversarialTurns(base)

	// Write through the production insert path so the column it writes is
	// covered too, not just the schema.
	turns := make([]models.Turn, 0, len(fixtures))
	for _, f := range fixtures {
		turns = append(turns, models.Turn{
			ID: f.id, SessionID: f.sessionID, Type: f.turnType,
			Timestamp: f.timestamp, Ordinal: f.wantOrdinal, Content: "c " + f.id,
		})
	}
	if err := database.InsertTurns(turns); err != nil {
		t.Fatalf("insert turns: %v", err)
	}

	got, err := database.GetTurns("session-a")
	if err != nil {
		t.Fatalf("GetTurns: %v", err)
	}

	want := []string{"a-late", "a-tie-1", "a-tie-2", "a-progress", "a-attachment", "a-system"}
	if len(got) != len(want) {
		t.Fatalf("GetTurns returned %d turns, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i] {
			t.Errorf("turn %d = %q, want %q", i, got[i].ID, want[i])
		}
		if got[i].Ordinal != i {
			t.Errorf("turn %d (%s): Ordinal = %d, want %d", i, got[i].ID, got[i].Ordinal, i)
		}
	}
}

// TestSessionTurnCursor covers the read API a consumer needs to ask for "the
// turns after position N": the last position in the session and the uuid
// sitting at it.
func TestSessionTurnCursor(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	base := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	var turns []models.Turn
	for _, f := range adversarialTurns(base) {
		turns = append(turns, models.Turn{
			ID: f.id, SessionID: f.sessionID, Type: f.turnType,
			Timestamp: f.timestamp, Ordinal: f.wantOrdinal,
		})
	}
	if err := database.InsertTurns(turns); err != nil {
		t.Fatalf("insert turns: %v", err)
	}

	cursor, err := database.SessionTurnCursor("session-a")
	if err != nil {
		t.Fatalf("SessionTurnCursor: %v", err)
	}
	if !cursor.Found {
		t.Fatal("cursor.Found = false for a session with turns")
	}
	if cursor.LastOrdinal != 5 {
		t.Errorf("LastOrdinal = %d, want 5", cursor.LastOrdinal)
	}
	if cursor.LastEntryUUID != "a-system" {
		t.Errorf("LastEntryUUID = %q, want %q", cursor.LastEntryUUID, "a-system")
	}

	empty, err := database.SessionTurnCursor("session-nothing")
	if err != nil {
		t.Fatalf("SessionTurnCursor on an unknown session: %v", err)
	}
	if empty.Found {
		t.Errorf("cursor for a session with no turns = %+v, want Found false", empty)
	}
}

// TestMergeFromAssignsOrdinalsForPre008Source covers the archive `ccvault
// import` is actually pointed at: a copy taken off another machine, or a
// backup, written before this column existed.
//
// sharedColumns drops a column the incoming database lacks, so without
// special handling every imported turn would land on the ordinal column's
// DEFAULT 0 and the second one in a session would violate the unique index —
// turning a recovery import into a hard failure.
func TestMergeFromAssignsOrdinalsForPre008Source(t *testing.T) {
	dest, cleanup := setupTestDB(t)
	defer cleanup()

	base := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	fixtures := adversarialTurns(base)
	srcDir := seedPre008(t, fixtures)

	stats, err := dest.MergeFrom(filepath.Join(srcDir, "ccvault.db"))
	if err != nil {
		t.Fatalf("MergeFrom a pre-008 archive: %v", err)
	}
	if stats.TurnsInserted != int64(len(fixtures)) {
		t.Errorf("TurnsInserted = %d, want %d", stats.TurnsInserted, len(fixtures))
	}

	for _, f := range fixtures {
		var got int
		if err := dest.QueryRow("SELECT ordinal FROM turns WHERE id = ?", f.id).Scan(&got); err != nil {
			t.Fatalf("read ordinal of imported %s: %v", f.id, err)
		}
		if got != f.wantOrdinal {
			t.Errorf("imported turn %s: ordinal = %d, want %d", f.id, got, f.wantOrdinal)
		}
	}
}
