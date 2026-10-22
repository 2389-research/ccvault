// ABOUTME: Tests migration 016 — tool_uses.turn_id's reference names a key that exists again, and the rebuild keeps every row, every id and tool_uses_fts (#122).
// ABOUTME: Red first: the declaration said REFERENCES turns(id), which migration 012 stopped being a unique key.

package db

import (
	"database/sql"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// seedPre016 builds an archive at migration 015 holding sessions, turns and
// tool uses, and returns its data directory.
//
// Rows rather than an empty schema, because the rebuild's whole risk is in
// what it carries across: tool_uses_fts is an external-content index keyed on
// tool_uses.id, so an id the rebuild reassigned would leave every document in
// it describing a different call.
func seedPre016(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	raw, err := sql.Open("sqlite", filepath.Join(dir, "ccvault.db"))
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer func() { _ = raw.Close() }()

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
		if m.version >= 16 {
			continue
		}
		if err := applyMigration(raw, m); err != nil {
			t.Fatalf("apply %03d: %v", m.version, err)
		}
	}

	// Guard the guard: if tool_uses already carried the composite reference
	// below 016, every assertion here would pass without proving anything.
	if strings.Contains(tableSQL(t, raw, "tool_uses"), "turns(id, session_id)") {
		t.Fatal("tool_uses already references turns(id, session_id) below migration 016")
	}

	ts := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)

	// Two sessions sharing a turn uuid, which is the state migration 012 made
	// legal and the reason the reference needs both columns. A rebuild that
	// kept the single-column reference, or that deduplicated on turn_id,
	// would lose one session's calls.
	for _, sessionID := range []string{"sess-a", "sess-b"} {
		if _, err := raw.Exec(`INSERT INTO sessions (id, started_at, source_file, source, model, git_branch)
			VALUES (?, ?, ?, 'claude-code', '', '')`,
			sessionID, FormatTimestamp(ts), "/fake/"+sessionID+".jsonl"); err != nil {
			t.Fatalf("seed session %s: %v", sessionID, err)
		}
		if _, err := raw.Exec(`INSERT INTO turns (id, session_id, type, timestamp, content, ordinal)
			VALUES ('shared-turn', ?, 'assistant', ?, 'hello', 0)`,
			sessionID, FormatTimestamp(ts)); err != nil {
			t.Fatalf("seed turn of %s: %v", sessionID, err)
		}
		for ordinal, name := range []string{"Bash", "Edit"} {
			if _, err := raw.Exec(`
				INSERT INTO tool_uses
					(turn_id, session_id, tool_name, timestamp, tool_use_id,
					 input_json, input_length, result_content, result_length, is_error, turn_ordinal)
				VALUES ('shared-turn', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				sessionID, name, FormatTimestamp(ts), sessionID+"-"+name,
				`{"command":"`+name+` in `+sessionID+`"}`, 30,
				"result of "+name+" in "+sessionID, 20, ordinal, ordinal); err != nil {
				t.Fatalf("seed tool use %s/%s: %v", sessionID, name, err)
			}
		}
	}

	// A call whose turn is gone, so the test can establish that correcting the
	// reference does not silently delete anything. #87's orphans are real rows
	// on the author's archive, and a migration that quietly dropped them would
	// destroy the evidence of a defect that is still open.
	if _, err := raw.Exec(`
		INSERT INTO tool_uses (turn_id, session_id, tool_name, timestamp, turn_ordinal)
		VALUES ('turn-that-is-gone', 'sess-a', 'Read', ?, 0)`,
		FormatTimestamp(ts)); err != nil {
		t.Fatalf("seed orphaned tool use: %v", err)
	}

	return dir
}

// tableSQL reads a table's CREATE statement back from sqlite_master.
func tableSQL(t *testing.T, raw *sql.DB, name string) string {
	t.Helper()

	var stmt string
	if err := raw.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&stmt); err != nil {
		t.Fatalf("read %s schema: %v", name, err)
	}
	return stmt
}

// TestMigration016PointsTheReferenceAtAKeyThatExists is #122. Migration 012
// re-keyed turns on (session_id, id), which left `turn_id TEXT REFERENCES
// turns(id)` naming something that is not a unique key — a declaration that
// cannot be a foreign key target and has never enforced anything, because
// ccvault opens no connection with foreign_keys on.
//
// So this is a trap rather than a live bug, and the fix is for whoever next
// turns enforcement on: with the composite reference in place, the pragma
// describes the archive instead of failing on it.
func TestMigration016PointsTheReferenceAtAKeyThatExists(t *testing.T) {
	dir := seedPre016(t)

	database, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = database.Close() }()

	stmt := tableSQL(t, database.DB, "tool_uses")

	if !strings.Contains(stmt, "FOREIGN KEY (turn_id, session_id) REFERENCES turns(id, session_id)") {
		t.Errorf("tool_uses does not name turns' real key:\n%s", stmt)
	}
	if strings.Contains(stmt, "turn_id TEXT REFERENCES turns(id)") {
		t.Errorf("tool_uses still references turns(id), which is not a unique key:\n%s", stmt)
	}
}

// TestMigration016LeavesNoRowOrphanedAgainstTheCompositeKey is the assertion
// the new declaration earns: every row whose turn exists must satisfy the
// reference as written. A composite reference that matched on turn_id alone
// would pass vacuously, and one that matched the wrong pair would report the
// shared turn's calls as orphans.
func TestMigration016LeavesNoRowOrphanedAgainstTheCompositeKey(t *testing.T) {
	dir := seedPre016(t)

	database, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = database.Close() }()

	// Against the composite definition: a call belongs to the turn with its
	// uuid *in its own session*. The four seeded calls on 'shared-turn' all
	// satisfy that; under the old single-column reference they would too, which
	// is why the orphan below is in the fixture.
	var orphaned int
	if err := database.QueryRow(`
		SELECT COUNT(*) FROM tool_uses tu
		WHERE tu.turn_id IS NOT NULL
		  AND tu.turn_id <> 'turn-that-is-gone'
		  AND NOT EXISTS (
			SELECT 1 FROM turns t
			WHERE t.id = tu.turn_id AND t.session_id = tu.session_id)`).Scan(&orphaned); err != nil {
		t.Fatalf("count orphans: %v", err)
	}
	if orphaned != 0 {
		t.Errorf("%d call(s) do not satisfy the composite reference the rebuild declared", orphaned)
	}

	// Both sessions' copies of the shared turn kept their own calls. This is
	// what the second column of the key is for, and a rebuild that
	// deduplicated on turn_id would have halved it.
	for _, sessionID := range []string{"sess-a", "sess-b"} {
		var n int
		if err := database.QueryRow(
			`SELECT COUNT(*) FROM tool_uses WHERE turn_id = 'shared-turn' AND session_id = ?`,
			sessionID).Scan(&n); err != nil {
			t.Fatalf("count calls of %s: %v", sessionID, err)
		}
		if n != 2 {
			t.Errorf("session %s holds %d calls on the shared turn, want 2", sessionID, n)
		}
	}

	// The orphan survives. Correcting a declaration that enforces nothing must
	// not delete the rows that prove #87 happened.
	var orphanSurvived int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM tool_uses WHERE turn_id = 'turn-that-is-gone'`).Scan(&orphanSurvived); err != nil {
		t.Fatalf("count the orphan: %v", err)
	}
	if orphanSurvived != 1 {
		t.Errorf("the rebuild deleted the orphaned call (%d left), destroying #87's evidence", orphanSurvived)
	}
}

// TestMigration016KeepsEveryRowAndItsID is the rebuild's load-bearing
// property. tool_uses_fts is an FTS5 external-content index with
// content_rowid='id', so a row that came back with a different id leaves the
// index describing a different call — and the cheap orphan check could not see
// it, because it compares counts and the counts would still agree.
func TestMigration016KeepsEveryRowAndItsID(t *testing.T) {
	dir := seedPre016(t)

	// What the table holds at 015, keyed by the id the index is pinned to.
	before := map[int64]string{}
	func() {
		raw, err := sql.Open("sqlite", filepath.Join(dir, "ccvault.db"))
		if err != nil {
			t.Fatalf("open raw: %v", err)
		}
		defer func() { _ = raw.Close() }()
		before = toolUseFingerprints(t, raw)
	}()
	if len(before) != 5 {
		t.Fatalf("the fixture holds %d calls, want 5", len(before))
	}

	database, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = database.Close() }()

	after := toolUseFingerprints(t, database.DB)

	if len(after) != len(before) {
		t.Fatalf("the rebuild changed the row count: %d -> %d", len(before), len(after))
	}
	for id, want := range before {
		got, ok := after[id]
		if !ok {
			t.Errorf("id %d is gone, so tool_uses_fts' document for it now describes nothing", id)
			continue
		}
		if got != want {
			t.Errorf("id %d changed:\n before %s\n after  %s", id, want, got)
		}
	}
}

// toolUseFingerprints reads every tool_uses row as id -> a rendering of its
// columns, so a rebuild can be compared against what it replaced.
func toolUseFingerprints(t *testing.T, raw *sql.DB) map[int64]string {
	t.Helper()

	rows, err := raw.Query(`
		SELECT id, COALESCE(turn_id, ''), COALESCE(session_id, ''), tool_name,
		       COALESCE(file_path, ''), CAST(timestamp AS TEXT), COALESCE(tool_use_id, ''),
		       COALESCE(input_json, ''), COALESCE(input_length, -1),
		       COALESCE(result_content, ''), COALESCE(result_length, -1),
		       COALESCE(result_omitted_reason, ''), COALESCE(is_error, -1),
		       turn_ordinal, search_period
		FROM tool_uses ORDER BY id`)
	if err != nil {
		t.Fatalf("read tool_uses: %v", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var turnID, sessionID, toolName, filePath, ts, toolUseID string
		var inputJSON string
		var inputLength int
		var resultContent string
		var resultLength int
		var omitted string
		var isError, turnOrdinal int
		var searchPeriod string
		if err := rows.Scan(&id, &turnID, &sessionID, &toolName, &filePath, &ts, &toolUseID,
			&inputJSON, &inputLength, &resultContent, &resultLength, &omitted,
			&isError, &turnOrdinal, &searchPeriod); err != nil {
			t.Fatalf("scan tool use: %v", err)
		}
		out[id] = strings.Join([]string{
			turnID, sessionID, toolName, filePath, ts, toolUseID, inputJSON,
			strconv.Itoa(inputLength), resultContent, strconv.Itoa(resultLength), omitted,
			strconv.Itoa(isError), strconv.Itoa(turnOrdinal), searchPeriod,
		}, "|")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read tool_uses: %v", err)
	}
	return out
}

// TestMigration016DoesNotReuseAnID is the other half of carrying the ids
// across. tool_uses_fts is keyed on tool_uses.id, so an id handed out twice
// would put two calls' text on one document. The author's archive holds
// 333,245 rows whose ids run to 961,068 — the ids are sparse, because rows
// have been deleted — and AUTOINCREMENT is what keeps the next one above the
// high-water mark rather than filling a gap.
func TestMigration016DoesNotReuseAnID(t *testing.T) {
	dir := seedPre016(t)

	database, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = database.Close() }()

	var maxID int64
	if err := database.QueryRow(`SELECT MAX(id) FROM tool_uses`).Scan(&maxID); err != nil {
		t.Fatalf("read max id: %v", err)
	}

	// Free a gap below the high-water mark, the way a re-sync of one session
	// does, then write a new call.
	if _, err := database.Exec(`DELETE FROM tool_uses WHERE id = ?`, maxID); err != nil {
		t.Fatalf("delete the last call: %v", err)
	}
	res, err := database.Exec(`
		INSERT INTO tool_uses (turn_id, session_id, tool_name, timestamp, turn_ordinal)
		VALUES ('shared-turn', 'sess-a', 'Write', ?, 7)`,
		FormatTimestamp(time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatalf("insert a new call: %v", err)
	}
	newID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("read the new id: %v", err)
	}

	if newID <= maxID {
		t.Errorf("the new call took id %d, at or below the previous high-water mark of %d: "+
			"AUTOINCREMENT did not survive the rebuild, so tool_uses_fts can be handed "+
			"two calls on one document", newID, maxID)
	}

	// And the index is still sound after a write through the re-created
	// triggers, which is the other thing the rebuild had to put back.
	report, err := database.CheckFTSStrict()
	if err != nil {
		t.Fatalf("CheckFTSStrict: %v", err)
	}
	if err := report.Err(); err != nil {
		t.Fatalf("writing through the re-created triggers left an index malformed: %v", err)
	}
}

// TestMigration016KeepsToolUsesFTSSound is the verdict migration 012
// established as the discipline for a rebuild of an FTS content table: FTS5's
// own strict integrity-check, which reads the index against the table and is
// the only thing that can tell a sound external-content index from a stranded
// one. The migration runs it as its last statement, so a bad rebuild rolls
// back; this asserts the result from outside.
func TestMigration016KeepsToolUsesFTSSound(t *testing.T) {
	dir := seedPre016(t)

	database, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = database.Close() }()

	report, err := database.CheckFTSStrict()
	if err != nil {
		t.Fatalf("CheckFTSStrict: %v", err)
	}
	if err := report.Err(); err != nil {
		t.Fatalf("an index is malformed after the rebuild: %v", err)
	}

	var sawToolUses bool
	for _, name := range report.Tables() {
		if name == "tool_uses_fts" {
			sawToolUses = true
		}
	}
	if !sawToolUses {
		t.Fatal("the strict check did not inspect tool_uses_fts, so it establishes nothing here")
	}

	// And the index still answers for the rows it describes. A rebuild that
	// reassigned ids would leave the counts agreeing while every document
	// pointed at the wrong call, which is exactly what the strict check is
	// for — but a query is the thing a user notices.
	var hits int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM tool_uses_fts WHERE tool_uses_fts MATCH ?`,
		`{input_json} : "sess-b"`).Scan(&hits); err != nil {
		t.Fatalf("query tool_uses_fts: %v", err)
	}
	if hits != 2 {
		t.Errorf("tool_uses_fts matched %d of sess-b's 2 calls", hits)
	}
}

// TestMigration016Replays holds the property every migration in this directory
// has: running it twice changes nothing. A replay is what a user who
// interrupts an upgrade gets.
func TestMigration016Replays(t *testing.T) {
	dir := seedPre016(t)

	database, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	before := toolUseFingerprints(t, database.DB)
	schemaBefore := tableSQL(t, database.DB, "tool_uses")
	if err := database.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	raw, err := sql.Open("sqlite", filepath.Join(dir, "ccvault.db"))
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer func() { _ = raw.Close() }()

	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	for _, m := range migrations {
		if m.version != 16 {
			continue
		}
		if err := applyMigration(raw, m); err != nil {
			t.Fatalf("replay 016: %v", err)
		}
	}

	if got := tableSQL(t, raw, "tool_uses"); got != schemaBefore {
		t.Errorf("the replay changed the schema:\n before %s\n after  %s", schemaBefore, got)
	}
	after := toolUseFingerprints(t, raw)
	if len(after) != len(before) {
		t.Fatalf("the replay changed the row count: %d -> %d", len(before), len(after))
	}
	for id, want := range before {
		if after[id] != want {
			t.Errorf("the replay changed id %d:\n before %s\n after  %s", id, want, after[id])
		}
	}
}
