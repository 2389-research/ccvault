// ABOUTME: Tests migration 013 — the tool_uses.is_error column, its backfill from raw_json, and the failed-call reader.
// ABOUTME: Seeds under the pre-013 schema so every backfill assertion can only be satisfied by the migration reading raw_json.

package db

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/2389-research/ccvault/pkg/models"
	_ "modernc.org/sqlite"
)

// errorFixture is one tool call plus the result answering it, as the two
// raw_json lines a Claude Code transcript holds. wantIsError is the tri-state
// the column must come out with, rendered as a string so a failure names the
// state rather than printing a pointer.
type errorFixture struct {
	toolUseID    string
	toolName     string
	assistantRaw string
	userRaw      string
	wantIsError  string // "error", "ok" or "unknown"
	why          string
}

// errorFixtures covers all three states and both result content shapes, drawn
// from the proportions measured on the author's archive: 10,909 blocks write
// is_error true, 44,533 write it false, 217,581 omit it entirely, and some
// calls are never answered at all.
func errorFixtures() []errorFixture {
	const sess = "sess-e"
	return []errorFixture{
		{
			toolUseID: "toolu_FAILED",
			toolName:  "Bash",
			assistantRaw: `{"uuid":"a-failed","sessionId":"` + sess + `","type":"assistant","timestamp":"2026-10-01T10:00:00.000Z",` +
				`"message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_FAILED","name":"Bash",` +
				`"input":{"command":"git push --force-with-lease"}}]}}`,
			userRaw: `{"uuid":"u-failed","sessionId":"` + sess + `","type":"user","timestamp":"2026-10-01T10:00:01.000Z",` +
				`"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_FAILED","is_error":true,` +
				`"content":"! [rejected] main -> main (stale info)"}]}}`,
			wantIsError: "error",
			why:         "the result block says is_error: true",
		},
		{
			toolUseID: "toolu_EXPLICITOK",
			toolName:  "Grep",
			assistantRaw: `{"uuid":"a-okexp","sessionId":"` + sess + `","type":"assistant","timestamp":"2026-10-01T10:00:02.000Z",` +
				`"message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_EXPLICITOK","name":"Grep",` +
				`"input":{"pattern":"func main"}}]}}`,
			userRaw: `{"uuid":"u-okexp","sessionId":"` + sess + `","type":"user","timestamp":"2026-10-01T10:00:03.000Z",` +
				`"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_EXPLICITOK","is_error":false,` +
				`"content":"cmd/ccvault/main.go:21:func main() {"}]}}`,
			wantIsError: "ok",
			why:         "the result block says is_error: false, which is a statement of success, not an absence",
		},
		{
			toolUseID: "toolu_SILENTOK",
			toolName:  "Bash",
			assistantRaw: `{"uuid":"a-oksil","sessionId":"` + sess + `","type":"assistant","timestamp":"2026-10-01T10:00:04.000Z",` +
				`"message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_SILENTOK","name":"Bash",` +
				`"input":{"command":"make build"}}]}}`,
			userRaw: `{"uuid":"u-oksil","sessionId":"` + sess + `","type":"user","timestamp":"2026-10-01T10:00:05.000Z",` +
				`"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_SILENTOK",` +
				`"content":"go build -o bin/ccvault ./cmd/ccvault"}]}}`,
			wantIsError: "ok",
			why:         "the key is absent, which is how 217,581 of the archive's successful results are written",
		},
		{
			toolUseID: "toolu_ARRFAIL",
			toolName:  "mcp__nanoclaw__ssh_localhost",
			assistantRaw: `{"uuid":"a-arrfail","sessionId":"` + sess + `","type":"assistant","timestamp":"2026-10-01T10:00:06.000Z",` +
				`"message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_ARRFAIL","name":"mcp__nanoclaw__ssh_localhost",` +
				`"input":{"command":"systemctl start nginx"}}]}}`,
			userRaw: `{"uuid":"u-arrfail","sessionId":"` + sess + `","type":"user","timestamp":"2026-10-01T10:00:07.000Z",` +
				`"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_ARRFAIL","is_error":true,` +
				`"content":[{"type":"text","text":"Failed to start nginx.service: Unit not found."}]}]}}`,
			wantIsError: "error",
			why:         "is_error sits beside the content array, so the flag must not depend on the content's shape",
		},
		{
			// A call the transcript never answered. NULL rather than 0: it did
			// not succeed, nothing is known about it.
			toolUseID: "toolu_UNANSWERED",
			toolName:  "Bash",
			assistantRaw: `{"uuid":"a-unans","sessionId":"` + sess + `","type":"assistant","timestamp":"2026-10-01T10:00:08.000Z",` +
				`"message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_UNANSWERED","name":"Bash",` +
				`"input":{"command":"sleep 600"}}]}}`,
			wantIsError: "unknown",
			why:         "nothing ever answered this call, so 0 would claim a success that never happened",
		},
		{
			// #101: 216,978 turns hold raw_json that does not parse. The
			// backfill reads raw_json, so it cannot recover anything from
			// them. This fixture is the archive's blind spot, written down as
			// a test rather than left to be discovered.
			toolUseID: "toolu_CORRUPT",
			toolName:  "Bash",
			assistantRaw: `{"uuid":"a-corrupt","sessionId":"` + sess + `","type":"assistant","timestamp":"2026-10-01T10:00:10.000Z",` +
				`"message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_CORRUPT","name":"Bash",` +
				`"input":{"command":"terraform apply"}}]}}`,
			// Deliberately truncated mid-document, with no closing braces —
			// the shape #101's rows actually have, left by syncs that predate
			// the oversized-line fix.
			userRaw: `{"uuid":"u-corrupt","sessionId":"` + sess + `","type":"user","timestamp":"2026-10-01T10:00:11.000Z",` +
				`"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_CORRUPT","is_error":true,` +
				`"content":"Error: resource already exists"`,
			wantIsError: "unknown",
			why:         "#101 — the result turn's raw_json does not parse, so the flag in it is unreachable",
		},
	}
}

// seedPre013 builds a database holding the fixtures under the schema as it
// stood *below* migration 013, then closes it and returns its directory.
//
// Applying only the migrations below 013 is what makes the backfill tests
// non-vacuous: at seed time there is no is_error column at all, so nothing can
// write a correct value early. The assertions can only be satisfied by
// migration 013 reading raw_json afterwards.
func seedPre013(t *testing.T, fixtures []errorFixture) string {
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
		if m.version >= 13 {
			continue
		}
		if err := applyMigration(raw, m); err != nil {
			t.Fatalf("apply %03d: %v", m.version, err)
		}
	}

	// Guard the guard: if a future edit lets the column exist before 013 runs,
	// every assertion below would pass without proving anything.
	if columnExists(t, raw, "tool_uses", "is_error") {
		t.Fatal("tool_uses.is_error exists below migration 013, so the backfill tests prove nothing")
	}

	if _, err := raw.Exec(`INSERT INTO sessions (id, started_at, source_file, source, model, git_branch)
		VALUES ('sess-e', ?, '/fake/sess-e.jsonl', 'claude-code', '', '')`,
		time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	ts := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	ordinal := 0
	for i, f := range fixtures {
		turnID := "turn-" + f.toolUseID
		if _, err := raw.Exec(
			`INSERT INTO turns (id, session_id, type, timestamp, content, raw_json, ordinal)
			 VALUES (?, 'sess-e', 'assistant', ?, ?, ?, ?)`,
			turnID, ts.Add(time.Duration(i)*time.Second), "[Tool: "+f.toolName+"]", f.assistantRaw, ordinal); err != nil {
			t.Fatalf("seed assistant turn %s: %v", turnID, err)
		}
		ordinal++
		// The row as a pre-013 sync wrote it: payloads present (migration 009
		// has run), is_error nowhere in the schema.
		if _, err := raw.Exec(
			`INSERT INTO tool_uses (turn_id, session_id, tool_name, timestamp, tool_use_id, turn_ordinal)
			 VALUES (?, 'sess-e', ?, ?, ?, 0)`,
			turnID, f.toolName, ts.Add(time.Duration(i)*time.Second), f.toolUseID); err != nil {
			t.Fatalf("seed tool use %s: %v", f.toolUseID, err)
		}
		if f.userRaw != "" {
			resultTurnID := "result-" + f.toolUseID
			if _, err := raw.Exec(
				`INSERT INTO turns (id, session_id, type, timestamp, content, raw_json, ordinal)
				 VALUES (?, 'sess-e', 'user', ?, '', ?, ?)`,
				resultTurnID, ts.Add(time.Duration(i)*time.Second+500*time.Millisecond), f.userRaw, ordinal); err != nil {
				t.Fatalf("seed result turn %s: %v", resultTurnID, err)
			}
			ordinal++
		}
	}

	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}
	return dir
}

// readIsError returns the stored tri-state for a call, named rather than
// pointered so a failure message says which of the three came back.
func readIsError(t *testing.T, database *DB, toolUseID string) string {
	t.Helper()
	var flag sql.NullInt64
	err := database.QueryRow(
		`SELECT is_error FROM tool_uses WHERE tool_use_id = ?`, toolUseID).Scan(&flag)
	if err != nil {
		t.Fatalf("read is_error for %s: %v", toolUseID, err)
	}
	switch {
	case !flag.Valid:
		return "unknown"
	case flag.Int64 == 1:
		return "error"
	case flag.Int64 == 0:
		return "ok"
	default:
		t.Fatalf("is_error for %s = %d, which is not one of the three states the column has", toolUseID, flag.Int64)
		return ""
	}
}

// TestMigration013BackfillsIsErrorFromRawJSON is issue #83's deliverable: the
// flag the transcripts already carried becomes a column, for an archive that
// is never re-synced.
func TestMigration013BackfillsIsErrorFromRawJSON(t *testing.T) {
	fixtures := errorFixtures()
	dir := seedPre013(t, fixtures)

	database, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = database.Close() }()

	for _, f := range fixtures {
		if got := readIsError(t, database, f.toolUseID); got != f.wantIsError {
			t.Errorf("%s: is_error = %s, want %s — %s", f.toolUseID, got, f.wantIsError, f.why)
		}
	}
}

// TestMigration013IsErrorIsQueryable is the question the issue says the archive
// cannot answer today. One predicate, no guessing at error wording.
func TestMigration013IsErrorIsQueryable(t *testing.T) {
	dir := seedPre013(t, errorFixtures())

	database, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = database.Close() }()

	rows, err := database.Query(
		`SELECT tool_use_id FROM tool_uses WHERE is_error = 1 ORDER BY tool_use_id`)
	if err != nil {
		t.Fatalf("query failures: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var got []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	want := []string{"toolu_ARRFAIL", "toolu_FAILED"}
	if len(got) != len(want) {
		t.Fatalf("is_error = 1 selected %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("is_error = 1 selected %v, want %v", got, want)
			break
		}
	}
}

// TestMigration013UsesThePartialIndex pins that the one query the column
// exists to serve is an index seek rather than a scan of 279,383 rows. A flag
// nobody can filter cheaply is a flag nobody filters.
func TestMigration013UsesThePartialIndex(t *testing.T) {
	dir := seedPre013(t, errorFixtures())

	database, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = database.Close() }()

	var detail string
	rows, err := database.Query(`EXPLAIN QUERY PLAN SELECT id FROM tool_uses WHERE is_error = 1`)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, parent, notUsed int
		var d string
		if err := rows.Scan(&id, &parent, &notUsed, &d); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		detail += d + "\n"
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	if !strings.Contains(detail, "idx_tool_uses_is_error") {
		t.Errorf("plan for the failed-call query does not use the partial index:\n%s", detail)
	}
}

// TestMigration013Replays holds the property every migration in this directory
// has: running it twice changes nothing. Replay matters here because a replay
// is what a user who interrupts an upgrade gets.
func TestMigration013Replays(t *testing.T) {
	fixtures := errorFixtures()
	dir := seedPre013(t, fixtures)

	database, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	before := map[string]string{}
	for _, f := range fixtures {
		before[f.toolUseID] = readIsError(t, database, f.toolUseID)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Forget that 013 ran, so Open applies it a second time over its own output.
	raw, err := sql.Open("sqlite", filepath.Join(dir, "ccvault.db"))
	if err != nil {
		t.Fatalf("reopen raw: %v", err)
	}
	if _, err := raw.Exec(`DELETE FROM schema_version WHERE version = 13`); err != nil {
		t.Fatalf("rewind schema_version: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	again, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = again.Close() }()

	for _, f := range fixtures {
		if got := readIsError(t, again, f.toolUseID); got != before[f.toolUseID] {
			t.Errorf("%s: replay changed is_error from %s to %s", f.toolUseID, before[f.toolUseID], got)
		}
	}
}

// TestMigration013KeepsFlagsWithinTheirSession is the cross-session
// contamination guard migration 009 established, applied to the new column.
// Both sessions made a call carrying the id "call_1"; only one of them failed,
// and the flag must not cross over.
//
// A one-bit column makes this worse than 009's, not better: a wrong
// result_content is visibly another session's text, while a wrong is_error is
// indistinguishable from a right one.
func TestMigration013KeepsFlagsWithinTheirSession(t *testing.T) {
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
		if m.version >= 13 {
			continue
		}
		if err := applyMigration(raw, m); err != nil {
			t.Fatalf("apply %03d: %v", m.version, err)
		}
	}
	if columnExists(t, raw, "tool_uses", "is_error") {
		t.Fatal("tool_uses.is_error exists below migration 013, so this test proves nothing")
	}

	ts := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	const sharedID = "call_1"

	for _, s := range []struct {
		session string
		isError string
	}{
		{"sess-alpha", `"is_error":true,`},
		{"sess-beta", ``},
	} {
		if _, err := raw.Exec(`INSERT INTO sessions (id, started_at, source_file, source, model, git_branch)
			VALUES (?, ?, ?, 'codex', '', '')`, s.session, ts, "/fake/"+s.session+".jsonl"); err != nil {
			t.Fatalf("seed session %s: %v", s.session, err)
		}

		callTurn := s.session + "-call"
		resultTurn := s.session + "-result"

		assistantRaw := `{"uuid":"` + callTurn + `","sessionId":"` + s.session + `","type":"assistant",` +
			`"timestamp":"2026-10-01T10:00:00.000Z","message":{"role":"assistant","content":[` +
			`{"type":"tool_use","id":"` + sharedID + `","name":"exec_command","input":{"cmd":"` + s.session + `"}}]}}`
		userRaw := `{"uuid":"` + resultTurn + `","sessionId":"` + s.session + `","type":"user",` +
			`"timestamp":"2026-10-01T10:00:01.000Z","message":{"role":"user","content":[` +
			`{"type":"tool_result","tool_use_id":"` + sharedID + `",` + s.isError + `"content":"output"}]}}`

		if _, err := raw.Exec(
			`INSERT INTO turns (id, session_id, type, timestamp, content, raw_json, ordinal)
			 VALUES (?, ?, 'assistant', ?, '', ?, 0)`,
			callTurn, s.session, ts, assistantRaw); err != nil {
			t.Fatalf("seed call turn %s: %v", callTurn, err)
		}
		if _, err := raw.Exec(
			`INSERT INTO turns (id, session_id, type, timestamp, content, raw_json, ordinal)
			 VALUES (?, ?, 'user', ?, '', ?, 1)`,
			resultTurn, s.session, ts.Add(time.Second), userRaw); err != nil {
			t.Fatalf("seed result turn %s: %v", resultTurn, err)
		}
		if _, err := raw.Exec(
			`INSERT INTO tool_uses (turn_id, session_id, tool_name, timestamp, tool_use_id, turn_ordinal)
			 VALUES (?, ?, 'exec_command', ?, ?, 0)`,
			callTurn, s.session, ts, sharedID); err != nil {
			t.Fatalf("seed tool use for %s: %v", s.session, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	database, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = database.Close() }()

	want := map[string]string{"sess-alpha": "error", "sess-beta": "ok"}
	rows, err := database.Query(`SELECT session_id, is_error FROM tool_uses ORDER BY session_id`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer func() { _ = rows.Close() }()

	seen := 0
	for rows.Next() {
		var session string
		var flag sql.NullInt64
		if err := rows.Scan(&session, &flag); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got := "unknown"
		if flag.Valid {
			if flag.Int64 == 1 {
				got = "error"
			} else {
				got = "ok"
			}
		}
		if got != want[session] {
			t.Errorf("%s: is_error = %s, want %s — the flag crossed sessions on a shared provider id", session, got, want[session])
		}
		seen++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if seen != 2 {
		t.Fatalf("got %d rows, want 2", seen)
	}
}

// TestInsertToolUsesWritesIsError covers the sync write path, which is the
// other way a row gets its flag. Three states in, three states out — and a
// re-sync of the same session must land on the same values, since
// InsertToolUsesTx deletes and re-inserts by position.
func TestInsertToolUsesWritesIsError(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	seedSessionForToolUses(t, database, "sess-w", "turn-w")

	yes, no := true, false
	uses := []models.ToolUse{
		{TurnID: "turn-w", SessionID: "sess-w", ToolName: "Bash", Timestamp: time.Now(),
			ToolUseID: "toolu_W_FAIL", InputJSON: `{"command":"false"}`, InputLength: 19,
			HasResult: true, ResultContent: "exit 1", ResultLength: 6, IsError: &yes},
		{TurnID: "turn-w", SessionID: "sess-w", ToolName: "Bash", Timestamp: time.Now(),
			ToolUseID: "toolu_W_OK", InputJSON: `{"command":"true"}`, InputLength: 18,
			HasResult: true, ResultContent: "", ResultLength: 0, IsError: &no},
		{TurnID: "turn-w", SessionID: "sess-w", ToolName: "Bash", Timestamp: time.Now(),
			ToolUseID: "toolu_W_UNKNOWN", InputJSON: `{"command":"sleep 600"}`, InputLength: 23,
			HasResult: false},
	}
	if err := database.InsertToolUses(uses); err != nil {
		t.Fatalf("InsertToolUses: %v", err)
	}

	want := map[string]string{
		"toolu_W_FAIL":    "error",
		"toolu_W_OK":      "ok",
		"toolu_W_UNKNOWN": "unknown",
	}
	for id, state := range want {
		if got := readIsError(t, database, id); got != state {
			t.Errorf("%s: is_error = %s, want %s", id, got, state)
		}
	}

	// A re-sync writes the same session again. The delete-then-insert must not
	// leave a stale flag behind on the row it replaces.
	flipped := make([]models.ToolUse, len(uses))
	copy(flipped, uses)
	flipped[0].IsError = &no
	if err := database.InsertToolUses(flipped); err != nil {
		t.Fatalf("re-insert: %v", err)
	}
	if got := readIsError(t, database, "toolu_W_FAIL"); got != "ok" {
		t.Errorf("after re-sync is_error = %s, want ok — the replaced row kept its old flag", got)
	}
}

// TestGetFailedToolCallsReadsTheColumn covers the reader the MCP surface and
// the TUI both use. It reads the stored column rather than re-parsing
// raw_json, which is the point: 216,978 turns hold raw_json that does not
// parse (#101), and a surface that re-derives the flag per read would lose
// exactly the rows the backfill did manage to recover.
func TestGetFailedToolCallsReadsTheColumn(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	seedSessionForToolUses(t, database, "sess-r", "turn-r")

	yes, no := true, false
	uses := []models.ToolUse{
		{TurnID: "turn-r", SessionID: "sess-r", ToolName: "Bash", Timestamp: time.Now(),
			ToolUseID: "toolu_R_OK", HasResult: true, IsError: &no},
		{TurnID: "turn-r", SessionID: "sess-r", ToolName: "Edit", Timestamp: time.Now(),
			ToolUseID: "toolu_R_FAIL", HasResult: true, ResultContent: "String not found", ResultLength: 16, IsError: &yes},
		{TurnID: "turn-r", SessionID: "sess-r", ToolName: "Bash", Timestamp: time.Now(),
			ToolUseID: "toolu_R_UNKNOWN", HasResult: false},
	}
	if err := database.InsertToolUses(uses); err != nil {
		t.Fatalf("InsertToolUses: %v", err)
	}

	got, err := database.GetFailedToolCalls("sess-r")
	if err != nil {
		t.Fatalf("GetFailedToolCalls: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d failed calls, want 1: %+v", len(got), got)
	}
	if got[0].ToolUseID != "toolu_R_FAIL" || got[0].ToolName != "Edit" || got[0].TurnID != "turn-r" {
		t.Errorf("failed call = %+v, want toolu_R_FAIL / Edit / turn-r", got[0])
	}
	if got[0].TurnOrdinal != 1 {
		t.Errorf("TurnOrdinal = %d, want 1 — the failing call is the second of its turn", got[0].TurnOrdinal)
	}

	// A session with nothing wrong in it returns nothing, not an error. The
	// callers branch on length, and an error here would make an uneventful
	// session look like a broken read.
	seedSessionForToolUses(t, database, "sess-clean", "turn-clean")
	clean, err := database.GetFailedToolCalls("sess-clean")
	if err != nil {
		t.Fatalf("GetFailedToolCalls on a clean session: %v", err)
	}
	if len(clean) != 0 {
		t.Errorf("got %d failed calls for a session with none: %+v", len(clean), clean)
	}
}

// TestMergeFromCarriesIsError covers `ccvault import`. MergeFrom copies
// whatever columns the two schemas share, so the flag travels without the
// merge naming it — but nothing in that mechanism is a compile error if it
// stops working, and no merge test covered a payload column travelling at all
// before this one. An archive imported from another machine arriving with
// every failure reset to unknown is exactly the silent loss the recovery
// import of #30 is still being cleaned up after.
func TestMergeFromCarriesIsError(t *testing.T) {
	dest, cleanup := setupTestDB(t)
	defer cleanup()

	now := time.Now().UTC().Truncate(time.Second)

	src := newSourceDB(t, func(s *DB) {
		seedSession(t, s, "/work/imported", "session-imported", "from the backup", now)

		yes := true
		if err := s.InsertToolUses([]models.ToolUse{{
			TurnID: "session-imported-turn-1", SessionID: "session-imported",
			ToolName: "Bash", Timestamp: now,
			ToolUseID: "toolu_IMPORTED", InputJSON: `{"command":"false"}`, InputLength: 19,
			HasResult: true, ResultContent: "exit 1", ResultLength: 6, IsError: &yes,
		}}); err != nil {
			t.Fatalf("seed failing tool use: %v", err)
		}
	})

	if _, err := dest.MergeFrom(src); err != nil {
		t.Fatalf("MergeFrom: %v", err)
	}

	if got := readIsError(t, dest, "toolu_IMPORTED"); got != "error" {
		t.Errorf("imported call's is_error = %s, want error", got)
	}
}

// TestIsErrorAgreesWithSessionHasError keeps the two granularities from
// disagreeing confusingly, which issue #83 raises and #73 made checkable. They
// answer different questions — one session held a failure, this call failed —
// but a session whose call failed must also be flagged, or a user filtering one
// way sees a different archive from a user filtering the other.
func TestIsErrorAgreesWithSessionHasError(t *testing.T) {
	dir := seedPre013(t, errorFixtures())

	database, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = database.Close() }()

	// seedPre013 writes sessions.has_error as the schema default, so set it to
	// what a sync of this transcript computes: the session holds a failure.
	if _, err := database.Exec(`UPDATE sessions SET has_error = 1 WHERE id = 'sess-e'`); err != nil {
		t.Fatalf("set has_error: %v", err)
	}

	var mismatched int
	err = database.QueryRow(`
		SELECT COUNT(*) FROM sessions s
		WHERE EXISTS (SELECT 1 FROM tool_uses tu WHERE tu.session_id = s.id AND tu.is_error = 1)
		  AND s.has_error = 0`).Scan(&mismatched)
	if err != nil {
		t.Fatalf("cross-check: %v", err)
	}
	if mismatched != 0 {
		t.Errorf("%d sessions hold a failed call but are not flagged has_error", mismatched)
	}
}
