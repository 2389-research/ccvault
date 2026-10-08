// ABOUTME: Tests that a turn is identified by (session_id, id) rather than by its uuid alone (#92)
// ABOUTME: Covers migration 012's rebuild, the write path, the recovery it schedules, and row-counted stats (#91)

package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/2389-research/ccvault/pkg/models"
)

// sharedUUID is the turn uuid two sessions hold a copy of. A resumed Claude
// Code transcript repeats the earlier session's lines verbatim, uuids
// included, so this is the ordinary case and not a corruption.
const sharedUUID = "11111111-2222-3333-4444-555555555555"

// seedBelow012 builds an archive at migration 011 and hands back its
// directory, so a test can watch 012 act on it.
//
// Raw *sql.DB rather than Open: Open runs every migration, which would apply
// the thing under test before the seed exists. Everything below 012 is applied
// so the seed is the shape a real archive is in on the eve of the rebuild.
func seedBelow012(t *testing.T, seed func(*sql.DB)) string {
	t.Helper()

	dir := t.TempDir()
	raw, err := sql.Open("sqlite", filepath.Join(dir, dbFileName))
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}

	if _, err := raw.Exec(`CREATE TABLE IF NOT EXISTS schema_version (
		version INTEGER NOT NULL,
		applied_at TEXT NOT NULL DEFAULT (datetime('now')))`); err != nil {
		t.Fatalf("schema_version: %v", err)
	}
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	applied := 0
	for _, m := range migrations {
		if m.version >= 12 {
			continue
		}
		if err := applyMigration(raw, m); err != nil {
			t.Fatalf("apply %03d: %v", m.version, err)
		}
		applied++
	}
	if applied == 0 {
		t.Fatal("no migrations below 012 were applied, so the fixture proves nothing")
	}

	// Guard the guard: if turns already carried the composite identity below
	// 012, every assertion below would pass without 012 doing anything.
	if turnsIdentityColumns(t, raw) != "id" {
		t.Fatalf("turns is already keyed on %q below migration 012, so these tests prove nothing",
			turnsIdentityColumns(t, raw))
	}
	if _, err := raw.Exec(`INSERT INTO sessions (id, started_at, source_file, source)
		VALUES ('vacuity-a', ?, '/fake/vacuity-a.jsonl', 'claude-code'),
		       ('vacuity-b', ?, '/fake/vacuity-b.jsonl', 'claude-code')`,
		time.Now(), time.Now()); err != nil {
		t.Fatalf("seed vacuity sessions: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO turns (id, session_id, type, timestamp, content, ordinal)
		VALUES ('vacuity-turn', 'vacuity-a', 'user', ?, 'x', 0)`, time.Now()); err != nil {
		t.Fatalf("seed vacuity turn: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO turns (id, session_id, type, timestamp, content, ordinal)
		VALUES ('vacuity-turn', 'vacuity-b', 'user', ?, 'x', 0)`, time.Now()); err == nil {
		t.Fatal("two sessions can already hold one uuid below migration 012, so these tests prove nothing")
	}
	if _, err := raw.Exec(`DELETE FROM turns WHERE id = 'vacuity-turn'`); err != nil {
		t.Fatalf("clear vacuity turn: %v", err)
	}
	if _, err := raw.Exec(`DELETE FROM sessions WHERE id IN ('vacuity-a', 'vacuity-b')`); err != nil {
		t.Fatalf("clear vacuity sessions: %v", err)
	}

	seed(raw)

	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}
	return dir
}

// turnsIdentityColumns renders the columns of turns' primary key, in key
// order, as a comma-separated list — "id" before migration 012,
// "session_id,id" after it.
func turnsIdentityColumns(t *testing.T, raw *sql.DB) string {
	t.Helper()
	rows, err := raw.Query("PRAGMA table_info(turns)")
	if err != nil {
		t.Fatalf("table_info(turns): %v", err)
	}
	defer func() { _ = rows.Close() }()

	byPosition := map[int]string{}
	highest := 0
	for rows.Next() {
		var cid, notNull, pk int
		var name, colType string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		if pk > 0 {
			byPosition[pk] = name
			if pk > highest {
				highest = pk
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("table_info(turns): %v", err)
	}

	out := ""
	for i := 1; i <= highest; i++ {
		if i > 1 {
			out += ","
		}
		out += byPosition[i]
	}
	return out
}

// openSeeded runs the migrations over a seeded directory the way the
// application does — through Open — and returns the archive.
func openSeeded(t *testing.T, dir string) *DB {
	t.Helper()
	db, err := Open(dir)
	if err != nil {
		t.Fatalf("open seeded archive: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func countQuery(t *testing.T, db *DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// seedResumedPair writes the state a resumed transcript leaves behind at
// migration 011: an earlier session whose turns the later session's write
// deleted, and the counter it kept from its own parse.
//
// session-earlier reported 3 turns and holds one, at ordinal 0 — #92's
// signature, which 197 of the author's 201 drifted sessions match exactly.
// session-later holds the three turns it copied, the shared uuid among them.
func seedResumedPair(t *testing.T) func(*sql.DB) {
	t.Helper()
	base := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	return func(raw *sql.DB) {
		for _, s := range []string{"session-earlier", "session-later", "session-clean"} {
			if _, err := raw.Exec(`INSERT INTO sessions (id, started_at, source_file, source, turn_count)
				VALUES (?, ?, ?, 'claude-code', 0)`, s, base, "/fake/"+s+".jsonl"); err != nil {
				t.Fatalf("seed session %s: %v", s, err)
			}
			if _, err := raw.Exec(`INSERT INTO source_files (path, source, mtime, synced_at)
				VALUES (?, 'claude-code', ?, ?)`, "/fake/"+s+".jsonl", base, base); err != nil {
				t.Fatalf("seed source_files for %s: %v", s, err)
			}
		}

		// The earlier session's survivor: whatever the later transcript did
		// not copy. Here that is its opening turn.
		if _, err := raw.Exec(`INSERT INTO turns (id, session_id, type, timestamp, content, ordinal)
			VALUES ('earlier-own-turn', 'session-earlier', 'user', ?, 'survivorcontent', 0)`, base); err != nil {
			t.Fatalf("seed earlier survivor: %v", err)
		}
		if _, err := raw.Exec(`UPDATE sessions SET turn_count = 3 WHERE id = 'session-earlier'`); err != nil {
			t.Fatalf("set earlier turn_count: %v", err)
		}

		// The later session holds the copies, including the shared uuid.
		for i, id := range []string{sharedUUID, "later-turn-1", "later-turn-2"} {
			if _, err := raw.Exec(`INSERT INTO turns (id, session_id, type, timestamp, content, ordinal)
				VALUES (?, 'session-later', 'assistant', ?, ?, ?)`,
				id, base.Add(time.Duration(i)*time.Second), fmt.Sprintf("latercontent%d", i), i); err != nil {
				t.Fatalf("seed later turn %s: %v", id, err)
			}
			if _, err := raw.Exec(`INSERT INTO tool_uses (turn_id, session_id, tool_name, timestamp, turn_ordinal, input_json)
				VALUES (?, 'session-later', 'Read', ?, 0, '{"later":true}')`,
				id, base.Add(time.Duration(i)*time.Second)); err != nil {
				t.Fatalf("seed later tool use for %s: %v", id, err)
			}
		}
		if _, err := raw.Exec(`UPDATE sessions SET turn_count = 3 WHERE id = 'session-later'`); err != nil {
			t.Fatalf("set later turn_count: %v", err)
		}

		// A session whose counter already agrees with its rows, so the tests
		// can tell a targeted repair from a blanket one.
		if _, err := raw.Exec(`INSERT INTO turns (id, session_id, type, timestamp, content, ordinal)
			VALUES ('clean-turn', 'session-clean', 'user', ?, 'cleancontent', 0)`, base); err != nil {
			t.Fatalf("seed clean turn: %v", err)
		}
		if _, err := raw.Exec(`UPDATE sessions SET turn_count = 1 WHERE id = 'session-clean'`); err != nil {
			t.Fatalf("set clean turn_count: %v", err)
		}
	}
}

// TestMigration012MakesATurnsIdentityItsSessionAndUUID is the core of #92. A
// turn uuid is not unique across sessions, so keying turns on it alone makes
// the later-parsed session's write delete the earlier session's rows.
func TestMigration012MakesATurnsIdentityItsSessionAndUUID(t *testing.T) {
	dir := seedBelow012(t, seedResumedPair(t))
	database := openSeeded(t, dir)

	if got := turnsIdentityColumns(t, database.DB); got != "session_id,id" {
		t.Fatalf("turns is keyed on %q, want \"session_id,id\"", got)
	}

	// What the old key forbade: the earlier session's own copy of the turn
	// the later one took over.
	if _, err := database.Exec(`INSERT INTO turns (id, session_id, type, timestamp, content, ordinal)
		VALUES (?, 'session-earlier', 'assistant', ?, 'recoveredcontent', 1)`,
		sharedUUID, time.Date(2026, 10, 1, 9, 0, 1, 0, time.UTC)); err != nil {
		t.Fatalf("two sessions must be able to hold one uuid: %v", err)
	}
	if n := countQuery(t, database, "SELECT COUNT(*) FROM turns WHERE id = ?", sharedUUID); n != 2 {
		t.Errorf("%d rows hold the shared uuid, want 2 — one per session", n)
	}

	// And what it must still forbid: one session holding the same uuid twice.
	_, err := database.Exec(`INSERT INTO turns (id, session_id, type, timestamp, content, ordinal)
		VALUES (?, 'session-earlier', 'assistant', ?, 'dup', 9)`,
		sharedUUID, time.Date(2026, 10, 1, 9, 0, 2, 0, time.UTC))
	if err == nil {
		t.Error("one session held the same uuid twice; (session_id, id) is not being enforced")
	}

	// The rebuild has to carry turns_fts with it. The cheap orphan count
	// cannot see a stranded entry whose rowid was reused, so the verdict is
	// FTS5's strict check — see assertFTSSound.
	assertFTSSound(t, database, "after migration 012 rebuilt turns")

	for _, want := range []struct{ term, turnID string }{
		{"survivorcontent", "earlier-own-turn"},
		{"latercontent0", sharedUUID},
		{"cleancontent", "clean-turn"},
	} {
		var id string
		err := database.QueryRow(`SELECT t.id FROM turns t JOIN turns_fts f ON t.rowid = f.rowid
			WHERE turns_fts MATCH ?`, contentScoped(want.term)).Scan(&id)
		if err != nil {
			t.Errorf("searching %q after the rebuild: %v", want.term, err)
			continue
		}
		if id != want.turnID {
			t.Errorf("%q matched turn %q, want %q", want.term, id, want.turnID)
		}
	}
}

// TestMigration012PreservesEveryTurnAndItsPosition guards the rebuild itself:
// a CREATE TABLE … AS SELECT that drops a column, a row, or an ordinal would
// be a far worse bug than the one it fixes.
func TestMigration012PreservesEveryTurnAndItsPosition(t *testing.T) {
	dir := seedBelow012(t, seedResumedPair(t))
	database := openSeeded(t, dir)

	if n := countQuery(t, database, "SELECT COUNT(*) FROM turns"); n != 5 {
		t.Errorf("turns holds %d rows after the rebuild, want the 5 seeded", n)
	}

	turns, err := database.GetTurns("session-later")
	if err != nil {
		t.Fatalf("GetTurns: %v", err)
	}
	if len(turns) != 3 {
		t.Fatalf("session-later holds %d turns, want 3", len(turns))
	}
	for i, want := range []string{sharedUUID, "later-turn-1", "later-turn-2"} {
		if turns[i].ID != want {
			t.Errorf("turn at position %d is %q, want %q", i, turns[i].ID, want)
		}
		if turns[i].Ordinal != i {
			t.Errorf("turn %q sits at ordinal %d, want %d", turns[i].ID, turns[i].Ordinal, i)
		}
		if turns[i].Content != fmt.Sprintf("latercontent%d", i) {
			t.Errorf("turn %q carries content %q", turns[i].ID, turns[i].Content)
		}
	}

	// UNIQUE(session_id, ordinal) from #29 has to survive the swap, along with
	// the index MAX(ordinal) seeks.
	if _, err := database.Exec(`INSERT INTO turns (id, session_id, type, timestamp, ordinal)
		VALUES ('position-thief', 'session-later', 'user', ?, 0)`, time.Now()); err == nil {
		t.Error("two turns took ordinal 0 of one session; UNIQUE(session_id, ordinal) did not survive the rebuild")
	}
	cursor, err := database.SessionTurnCursor("session-later")
	if err != nil {
		t.Fatalf("SessionTurnCursor: %v", err)
	}
	if !cursor.Found || cursor.LastOrdinal != 2 || cursor.LastEntryUUID != "later-turn-2" {
		t.Errorf("cursor after the rebuild is %+v, want ordinal 2 at later-turn-2", cursor)
	}
}

// TestMigration012SchedulesAReparseOfTheSessionsThatLostTurns covers the
// recovery half. A schema change cannot bring back a deleted row; only a
// re-parse of the transcript can. Forgetting the stored mtime of exactly the
// sessions whose counter disagrees with their rows is what makes the next
// ordinary sync do it, rather than leaving the archive short until someone
// thinks to run --full.
func TestMigration012SchedulesAReparseOfTheSessionsThatLostTurns(t *testing.T) {
	dir := seedBelow012(t, seedResumedPair(t))
	database := openSeeded(t, dir)

	for _, path := range []string{"/fake/session-earlier.jsonl"} {
		if n := countQuery(t, database, "SELECT COUNT(*) FROM source_files WHERE path = ?", path); n != 0 {
			t.Errorf("%s is still marked synced, so the next sync will skip the session that lost turns", path)
		}
	}
	for _, path := range []string{"/fake/session-later.jsonl", "/fake/session-clean.jsonl"} {
		if n := countQuery(t, database, "SELECT COUNT(*) FROM source_files WHERE path = ?", path); n != 1 {
			t.Errorf("%s was unmarked, but its session's counter agreed with its rows", path)
		}
	}
}

// TestMigration012LeavesTheCounterAloneSoTheDamageStaysVisible pins the
// ordering #92 insists on. Reconciling turn_count in the migration would make
// the counter agree with a database still missing the turns, and the counter is
// the only visible evidence anything is wrong.
func TestMigration012LeavesTheCounterAloneSoTheDamageStaysVisible(t *testing.T) {
	dir := seedBelow012(t, seedResumedPair(t))
	database := openSeeded(t, dir)

	var stored int
	if err := database.QueryRow(
		"SELECT turn_count FROM sessions WHERE id = 'session-earlier'").Scan(&stored); err != nil {
		t.Fatalf("read turn_count: %v", err)
	}
	if stored != 3 {
		t.Errorf("session-earlier reports %d turns, want the 3 its own parse counted — "+
			"the migration must not reconcile the counter before the re-parse", stored)
	}
}

// TestMigration012IsReplaySafe covers the case the migrator's own tests
// establish is real: anything that rewinds schema_version re-runs every
// migration above the rewind point.
func TestMigration012IsReplaySafe(t *testing.T) {
	dir := seedBelow012(t, seedResumedPair(t))
	database := openSeeded(t, dir)

	before := countQuery(t, database, "SELECT COUNT(*) FROM turns")
	if _, err := database.Exec("DELETE FROM schema_version WHERE version = 12"); err != nil {
		t.Fatalf("rewind schema_version: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	replayed := openSeeded(t, dir)
	if got := turnsIdentityColumns(t, replayed.DB); got != "session_id,id" {
		t.Errorf("after a replay turns is keyed on %q, want \"session_id,id\"", got)
	}
	if after := countQuery(t, replayed, "SELECT COUNT(*) FROM turns"); after != before {
		t.Errorf("a replay changed the turn count from %d to %d", before, after)
	}
	assertFTSSound(t, replayed, "after replaying migration 012")
}

// TestInsertTurnsKeepsAnotherSessionsCopyOfTheSameUUID is the write path half
// of #92, and the thing the schema change exists to let it do. Before the fix
// the later session's write deleted the earlier session's row outright — on
// the author's archive that cost 31,864 turns across 201 sessions.
func TestInsertTurnsKeepsAnotherSessionsCopyOfTheSameUUID(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	base := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	for _, id := range []string{"session-earlier", "session-later"} {
		if _, err := database.Exec(`INSERT INTO sessions (id, started_at, source_file, source)
			VALUES (?, ?, ?, 'claude-code')`, id, base, "/fake/"+id+".jsonl"); err != nil {
			t.Fatalf("seed session %s: %v", id, err)
		}
	}

	shared := models.Turn{
		ID: sharedUUID, Type: "user", Timestamp: base, Content: "sharedcontent",
	}
	earlier := shared
	earlier.SessionID = "session-earlier"
	earlier.Ordinal = 7
	later := shared
	later.SessionID = "session-later"
	later.Ordinal = 0

	if err := database.InsertTurns([]models.Turn{earlier}); err != nil {
		t.Fatalf("insert the earlier session's turn: %v", err)
	}
	if err := database.InsertTurns([]models.Turn{later}); err != nil {
		t.Fatalf("insert the later session's copy: %v", err)
	}

	if n := countQuery(t, database, "SELECT COUNT(*) FROM turns WHERE session_id = 'session-earlier'"); n != 1 {
		t.Errorf("the earlier session holds %d turns, want 1 — the later session's write deleted it", n)
	}
	if n := countQuery(t, database, "SELECT COUNT(*) FROM turns WHERE session_id = 'session-later'"); n != 1 {
		t.Errorf("the later session holds %d turns, want 1", n)
	}

	// Re-parsing the later session must still replace its own copy, both by
	// uuid and by position.
	later.Content = "reparsedcontent"
	if err := database.InsertTurns([]models.Turn{later}); err != nil {
		t.Fatalf("re-parse the later session: %v", err)
	}
	if n := countQuery(t, database, "SELECT COUNT(*) FROM turns"); n != 2 {
		t.Errorf("the archive holds %d turns after a re-parse, want 2", n)
	}
	var content string
	if err := database.QueryRow(
		"SELECT content FROM turns WHERE session_id = 'session-later'").Scan(&content); err != nil {
		t.Fatalf("read back the re-parsed turn: %v", err)
	}
	if content != "reparsedcontent" {
		t.Errorf("the later session's turn carries %q, want the re-parsed text", content)
	}
	assertFTSSound(t, database, "after a cross-session uuid collision and a re-parse")
}

// TestInsertTurnsStillReplacesWhateverHoldsThePosition keeps the other half of
// the write path honest: a position collision inside one session is still
// resolved by deleting whatever holds the position, which is how a rewritten
// transcript replaces a session's history.
func TestInsertTurnsStillReplacesWhateverHoldsThePosition(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	base := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	if _, err := database.Exec(`INSERT INTO sessions (id, started_at, source_file, source)
		VALUES ('sess', ?, '/fake/sess.jsonl', 'claude-code')`, base); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	first := models.Turn{ID: "turn-first", SessionID: "sess", Type: "user", Timestamp: base, Ordinal: 0, Content: "firstcontent"}
	if err := database.InsertTurns([]models.Turn{first}); err != nil {
		t.Fatalf("insert first: %v", err)
	}
	second := models.Turn{ID: "turn-second", SessionID: "sess", Type: "user", Timestamp: base, Ordinal: 0, Content: "secondcontent"}
	if err := database.InsertTurns([]models.Turn{second}); err != nil {
		t.Fatalf("insert a turn at the same position: %v", err)
	}

	if n := countQuery(t, database, "SELECT COUNT(*) FROM turns WHERE session_id = 'sess'"); n != 1 {
		t.Errorf("the session holds %d turns, want 1 — the position is unique", n)
	}
	var id string
	if err := database.QueryRow("SELECT id FROM turns WHERE session_id = 'sess'").Scan(&id); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if id != "turn-second" {
		t.Errorf("ordinal 0 is held by %q, want turn-second", id)
	}
	assertFTSSound(t, database, "after a position collision")
}

// TestToolUsesOfSharedTurnsStayWithTheirSession covers the consequence the
// identity change has for tool_uses. tool_uses.turn_id names a turn uuid, and
// once two sessions can hold one uuid, UNIQUE(turn_id, turn_ordinal) says a
// shared turn may have only one call at each position across the whole
// archive — so writing one session's calls would delete the other's. #113
// established that a unique index plus a delete-then-insert converts a
// legitimate second row into a silent deletion; the key has to be scoped the
// same way the turn is.
func TestToolUsesOfSharedTurnsStayWithTheirSession(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	base := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	for _, id := range []string{"session-earlier", "session-later"} {
		if _, err := database.Exec(`INSERT INTO sessions (id, started_at, source_file, source)
			VALUES (?, ?, ?, 'claude-code')`, id, base, "/fake/"+id+".jsonl"); err != nil {
			t.Fatalf("seed session %s: %v", id, err)
		}
		turn := models.Turn{ID: sharedUUID, SessionID: id, Type: "assistant", Timestamp: base, Content: "sharedcontent"}
		if err := database.InsertTurns([]models.Turn{turn}); err != nil {
			t.Fatalf("insert %s's copy of the shared turn: %v", id, err)
		}
		if err := database.InsertToolUses([]models.ToolUse{{
			TurnID:    sharedUUID,
			SessionID: id,
			ToolName:  "Read",
			Timestamp: base,
			InputJSON: `{"session":"` + id + `"}`,
		}}); err != nil {
			t.Fatalf("insert %s's tool use: %v", id, err)
		}
	}

	if n := countQuery(t, database, "SELECT COUNT(*) FROM tool_uses WHERE turn_id = ?", sharedUUID); n != 2 {
		t.Errorf("%d tool_uses rows for the shared turn, want 2 — one per session", n)
	}
	for _, id := range []string{"session-earlier", "session-later"} {
		if n := countQuery(t,
			database, "SELECT COUNT(*) FROM tool_uses WHERE session_id = ?", id); n != 1 {
			t.Errorf("%s holds %d tool uses, want 1", id, n)
		}
	}

	// Deleting one session's turns must take only that session's calls.
	if err := database.DeleteTurnsForSession("session-later"); err != nil {
		t.Fatalf("delete the later session's turns: %v", err)
	}
	if n := countQuery(t, database, "SELECT COUNT(*) FROM tool_uses WHERE session_id = 'session-earlier'"); n != 1 {
		t.Error("deleting the later session's turns took the earlier session's tool use with them")
	}
	if n := countQuery(t, database, "SELECT COUNT(*) FROM tool_uses WHERE session_id = 'session-later'"); n != 0 {
		t.Errorf("%d of the later session's tool uses outlived its turns", n)
	}
	assertFTSSound(t, database, "after a shared turn's sessions parted ways")
}

// TestGetSessionStatsCountsTurnRows is #91. Summing sessions.turn_count
// reported 46,000 turns that are not in the database, and agents read
// `orient --json` to decide whether the archive is worth querying.
func TestGetSessionStatsCountsTurnRows(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	base := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	if _, err := database.Exec(`INSERT INTO sessions (id, started_at, source_file, source, turn_count)
		VALUES ('sess-drifted', ?, '/fake/a.jsonl', 'claude-code', 1462),
		       ('sess-clean', ?, '/fake/b.jsonl', 'claude-code', 1)`, base, base); err != nil {
		t.Fatalf("seed sessions: %v", err)
	}
	for _, s := range []string{"sess-drifted", "sess-clean"} {
		turn := models.Turn{ID: s + "-turn", SessionID: s, Type: "user", Timestamp: base, Content: "c"}
		if err := database.InsertTurns([]models.Turn{turn}); err != nil {
			t.Fatalf("insert %s's turn: %v", s, err)
		}
	}

	count, totalTurns, _, err := database.GetSessionStats()
	if err != nil {
		t.Fatalf("GetSessionStats: %v", err)
	}
	if count != 2 {
		t.Errorf("session count = %d, want 2", count)
	}
	if totalTurns != 2 {
		t.Errorf("turn total = %d, want the 2 rows turns actually holds", totalTurns)
	}
}

// TestTurnCountDriftReportsBothFigures backs the warning `stats` prints. #91
// asks for the discrepancy to be visible rather than silently corrected: a
// counter that disagrees with the rows means turns are missing, and that is
// worth saying out loud.
func TestTurnCountDriftReportsBothFigures(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	base := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	if _, err := database.Exec(`INSERT INTO sessions (id, started_at, source_file, source, turn_count)
		VALUES ('sess-drifted', ?, '/fake/a.jsonl', 'claude-code', 5),
		       ('sess-clean', ?, '/fake/b.jsonl', 'claude-code', 1)`, base, base); err != nil {
		t.Fatalf("seed sessions: %v", err)
	}
	for _, s := range []string{"sess-drifted", "sess-clean"} {
		turn := models.Turn{ID: s + "-turn", SessionID: s, Type: "user", Timestamp: base, Content: "c"}
		if err := database.InsertTurns([]models.Turn{turn}); err != nil {
			t.Fatalf("insert %s's turn: %v", s, err)
		}
	}

	drift, err := database.TurnCountDrift()
	if err != nil {
		t.Fatalf("TurnCountDrift: %v", err)
	}
	if drift.Counted != 6 {
		t.Errorf("summed counter = %d, want 6", drift.Counted)
	}
	if drift.Rows != 2 {
		t.Errorf("row count = %d, want 2", drift.Rows)
	}
	if drift.Sessions != 1 {
		t.Errorf("%d sessions disagree with their rows, want 1", drift.Sessions)
	}
	if !drift.Drifted() {
		t.Error("Drifted() is false while the counter is 4 turns above the rows")
	}
}

// TestReconcileSessionTurnCountsRunsAfterTheReparse is step three of #92's
// order. It only makes sense once every transcript still on disk has been
// re-read: before that, the counter is evidence.
func TestReconcileSessionTurnCountsRunsAfterTheReparse(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	base := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	if _, err := database.Exec(`INSERT INTO sessions (id, started_at, source_file, source, turn_count)
		VALUES ('sess-drifted', ?, '/fake/a.jsonl', 'claude-code', 5),
		       ('sess-clean', ?, '/fake/b.jsonl', 'claude-code', 1),
		       ('sess-empty', ?, '/fake/c.jsonl', 'claude-code', 9)`, base, base, base); err != nil {
		t.Fatalf("seed sessions: %v", err)
	}
	for _, s := range []string{"sess-drifted", "sess-clean"} {
		turn := models.Turn{ID: s + "-turn", SessionID: s, Type: "user", Timestamp: base, Content: "c"}
		if err := database.InsertTurns([]models.Turn{turn}); err != nil {
			t.Fatalf("insert %s's turn: %v", s, err)
		}
	}

	corrected, err := database.ReconcileSessionTurnCounts()
	if err != nil {
		t.Fatalf("ReconcileSessionTurnCounts: %v", err)
	}
	if corrected != 2 {
		t.Errorf("%d session counters corrected, want 2", corrected)
	}

	drift, err := database.TurnCountDrift()
	if err != nil {
		t.Fatalf("TurnCountDrift: %v", err)
	}
	if drift.Drifted() {
		t.Errorf("counters still disagree with the rows: %+v", drift)
	}

	// Idempotent, and a second pass must not write: a `<>` guard is what keeps
	// a clean archive's reconcile free.
	again, err := database.ReconcileSessionTurnCounts()
	if err != nil {
		t.Fatalf("second ReconcileSessionTurnCounts: %v", err)
	}
	if again != 0 {
		t.Errorf("a second pass corrected %d counters, want 0", again)
	}
}

// TestMigration012OnAFreshArchive covers the path most installs take: a
// database created from scratch runs every migration in order, and 012's
// rebuild has to cope with a turns table that has no rows in it.
func TestMigration012OnAFreshArchive(t *testing.T) {
	dir, err := os.MkdirTemp("", "ccvault-fresh-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	database := openSeeded(t, dir)
	if got := turnsIdentityColumns(t, database.DB); got != "session_id,id" {
		t.Errorf("a fresh archive keys turns on %q, want \"session_id,id\"", got)
	}
	assertFTSSound(t, database, "on a fresh archive")
}
