// ABOUTME: Tests migration 009 — the tool payload columns, their backfill from raw_json, and the FTS index over them.
// ABOUTME: Seeds under the pre-009 schema so the backfill assertions cannot be satisfied by the insert path.

package db

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/2389-research/ccvault/pkg/models"
	"github.com/2389-research/ccvault/pkg/toolpayload"
	_ "modernc.org/sqlite"
)

// payloadFixture is one tool call plus the result answering it, as the two
// raw_json lines a Claude Code transcript would hold.
type payloadFixture struct {
	toolUseID    string
	toolName     string
	filePath     string
	assistantRaw string
	userRaw      string
}

func payloadFixtures() []payloadFixture {
	return []payloadFixture{
		{
			toolUseID: "toolu_BASH",
			toolName:  "Bash",
			assistantRaw: `{"uuid":"a-bash","sessionId":"sess-p","type":"assistant","timestamp":"2026-10-01T10:00:00.000Z",` +
				`"message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_BASH","name":"Bash",` +
				`"input":{"command":"git rebase --onto main"}}]}}`,
			userRaw: `{"uuid":"u-bash","sessionId":"sess-p","type":"user","timestamp":"2026-10-01T10:00:01.000Z",` +
				`"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_BASH",` +
				`"content":"Successfully rebased and updated refs/heads/topic."}]}}`,
		},
		{
			toolUseID: "toolu_READ",
			toolName:  "Read",
			filePath:  "/tmp/huge.txt",
			assistantRaw: `{"uuid":"a-read","sessionId":"sess-p","type":"assistant","timestamp":"2026-10-01T10:00:02.000Z",` +
				`"message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_READ","name":"Read",` +
				`"input":{"file_path":"/tmp/huge.txt"}}]}}`,
			userRaw: `{"uuid":"u-read","sessionId":"sess-p","type":"user","timestamp":"2026-10-01T10:00:03.000Z",` +
				`"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_READ",` +
				`"content":"` + strings.Repeat("file line ", 400) + `"}]}}`,
		},
		{
			toolUseID: "toolu_IMG",
			toolName:  "mcp__arbitrary__fetch",
			assistantRaw: `{"uuid":"a-img","sessionId":"sess-p","type":"assistant","timestamp":"2026-10-01T10:00:04.000Z",` +
				`"message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_IMG","name":"mcp__arbitrary__fetch",` +
				`"input":{"url":"http://example.test/chart"}}]}}`,
			userRaw: `{"uuid":"u-img","sessionId":"sess-p","type":"user","timestamp":"2026-10-01T10:00:05.000Z",` +
				`"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_IMG",` +
				`"content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` +
				backfillCanary + `"}}]}]}}`,
		},
		{
			toolUseID: "toolu_ARR",
			toolName:  "mcp__nanoclaw__ssh_localhost",
			assistantRaw: `{"uuid":"a-arr","sessionId":"sess-p","type":"assistant","timestamp":"2026-10-01T10:00:06.000Z",` +
				`"message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_ARR","name":"mcp__nanoclaw__ssh_localhost",` +
				`"input":{"command":"systemctl status nginx"}}]}}`,
			userRaw: `{"uuid":"u-arr","sessionId":"sess-p","type":"user","timestamp":"2026-10-01T10:00:07.000Z",` +
				`"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_ARR",` +
				`"content":[{"type":"text","text":"nginx.service - active (running)"}]}]}}`,
		},
		{
			// A call the transcript never answered — 24 of these in the real
			// archive. It must come out of the backfill distinguishable from a
			// call that answered with nothing.
			toolUseID: "toolu_ORPHAN",
			toolName:  "Bash",
			assistantRaw: `{"uuid":"a-orphan","sessionId":"sess-p","type":"assistant","timestamp":"2026-10-01T10:00:08.000Z",` +
				`"message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_ORPHAN","name":"Bash",` +
				`"input":{"command":"sleep 600"}}]}}`,
		},
	}
}

// backfillCanary is a token that exists only inside an image payload. If it
// ever appears in a payload column or an FTS hit, base64 leaked.
const backfillCanary = "BASE64PAYLOADCANARYAAAAAAAAAAAAAAAAAA"

// seedPre009 builds a database holding the fixtures under the schema as it
// stood *below* migration 009 — turns with raw_json, tool_uses rows with only
// the columns that existed then — then closes it and returns its directory.
//
// Applying only the migrations below 009 is what makes the backfill tests
// non-vacuous. At seed time there are no payload columns at all, so nothing
// can write a correct value early: the assertions can only be satisfied by
// migration 009 reading raw_json afterwards.
func seedPre009(t *testing.T, fixtures []payloadFixture) string {
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
		if m.version >= 9 {
			continue
		}
		if err := applyMigration(raw, m); err != nil {
			t.Fatalf("apply %03d: %v", m.version, err)
		}
	}

	// Guard the guard: if a future edit lets a payload column exist before 009
	// runs, these tests would pass without proving anything.
	for _, column := range []string{
		"tool_use_id", "input_json", "input_length",
		"result_content", "result_length", "result_omitted_reason",
	} {
		if columnExists(t, raw, "tool_uses", column) {
			t.Fatalf("tool_uses.%s exists below migration 009, so the backfill tests prove nothing", column)
		}
	}
	if tableExists(t, raw, "tool_uses_fts") {
		t.Fatal("tool_uses_fts exists below migration 009, so the index tests prove nothing")
	}

	if _, err := raw.Exec(`INSERT INTO sessions (id, started_at, source_file, source, model, git_branch)
		VALUES ('sess-p', ?, '/fake/sess-p.jsonl', 'claude-code', '', '')`,
		time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	ts := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	ordinal := 0
	for i, f := range fixtures {
		turnID := "turn-" + f.toolUseID
		if _, err := raw.Exec(
			`INSERT INTO turns (id, session_id, type, timestamp, content, raw_json, ordinal)
			 VALUES (?, 'sess-p', 'assistant', ?, ?, ?, ?)`,
			turnID, ts.Add(time.Duration(i)*time.Second), "[Tool: "+f.toolName+"]", f.assistantRaw, ordinal); err != nil {
			t.Fatalf("seed assistant turn %s: %v", turnID, err)
		}
		ordinal++
		// The pre-009 tool_uses row: name and path, which is all the schema
		// could hold.
		if _, err := raw.Exec(
			`INSERT INTO tool_uses (turn_id, session_id, tool_name, file_path, timestamp) VALUES (?, 'sess-p', ?, ?, ?)`,
			turnID, f.toolName, f.filePath, ts.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("seed tool use %s: %v", f.toolUseID, err)
		}
		if f.userRaw != "" {
			resultTurnID := "result-" + f.toolUseID
			if _, err := raw.Exec(
				`INSERT INTO turns (id, session_id, type, timestamp, content, raw_json, ordinal)
				 VALUES (?, 'sess-p', 'user', ?, '', ?, ?)`,
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

func tableExists(t *testing.T, raw *sql.DB, name string) bool {
	t.Helper()
	var count int
	if err := raw.QueryRow(
		"SELECT COUNT(*) FROM sqlite_master WHERE name = ?", name).Scan(&count); err != nil {
		t.Fatalf("sqlite_master lookup for %s: %v", name, err)
	}
	return count > 0
}

// backfilledRow is what a payload column set looks like after the migration.
type backfilledRow struct {
	toolUseID    sql.NullString
	inputJSON    sql.NullString
	inputLength  sql.NullInt64
	result       sql.NullString
	resultLength sql.NullInt64
	omitReason   sql.NullString
}

func readBackfilled(t *testing.T, database *DB, toolName, turnID string) backfilledRow {
	t.Helper()
	var r backfilledRow
	err := database.QueryRow(`SELECT tool_use_id, input_json, input_length,
		result_content, result_length, result_omitted_reason
		FROM tool_uses WHERE turn_id = ? AND tool_name = ?`, turnID, toolName).Scan(
		&r.toolUseID, &r.inputJSON, &r.inputLength, &r.result, &r.resultLength, &r.omitReason)
	if err != nil {
		t.Fatalf("read backfilled row for %s/%s: %v", turnID, toolName, err)
	}
	return r
}

// TestMigration009BackfillsPayloadsFromRawJSON is the core backfill proof. The
// seeded tool_uses rows carry nothing but a name and a path; every assertion
// below is about data that exists only inside turns.raw_json until 009 runs.
func TestMigration009BackfillsPayloadsFromRawJSON(t *testing.T) {
	dir := seedPre009(t, payloadFixtures())

	database, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = database.Close() }()

	t.Run("id and input", func(t *testing.T) {
		row := readBackfilled(t, database, "Bash", "turn-toolu_BASH")
		if row.toolUseID.String != "toolu_BASH" {
			t.Errorf("tool_use_id = %q, want %q", row.toolUseID.String, "toolu_BASH")
		}
		if !strings.Contains(row.inputJSON.String, "git rebase --onto main") {
			t.Errorf("input_json = %q, want the command", row.inputJSON.String)
		}
		if row.inputLength.Int64 != int64(len(row.inputJSON.String)) {
			t.Errorf("input_length = %d, want %d", row.inputLength.Int64, len(row.inputJSON.String))
		}
	})

	t.Run("non-bulk result stored whole", func(t *testing.T) {
		row := readBackfilled(t, database, "Bash", "turn-toolu_BASH")
		want := "Successfully rebased and updated refs/heads/topic."
		if row.result.String != want {
			t.Errorf("result_content = %q, want %q", row.result.String, want)
		}
		if row.resultLength.Int64 != int64(len(want)) {
			t.Errorf("result_length = %d, want %d", row.resultLength.Int64, len(want))
		}
		if row.omitReason.Valid {
			t.Errorf("result_omitted_reason = %q, want NULL", row.omitReason.String)
		}
	})

	t.Run("bulk read keeps length only", func(t *testing.T) {
		row := readBackfilled(t, database, "Read", "turn-toolu_READ")
		if row.result.Valid {
			t.Errorf("result_content = %q, want NULL for a bulk read", row.result.String)
		}
		if row.omitReason.String != toolpayload.OmitBulkRead {
			t.Errorf("result_omitted_reason = %q, want %q", row.omitReason.String, toolpayload.OmitBulkRead)
		}
		if row.resultLength.Int64 != 4000 {
			t.Errorf("result_length = %d, want 4000", row.resultLength.Int64)
		}
	})

	t.Run("image detected by shape not name", func(t *testing.T) {
		row := readBackfilled(t, database, "mcp__arbitrary__fetch", "turn-toolu_IMG")
		if strings.Contains(row.result.String, backfillCanary) {
			t.Fatalf("base64 payload reached result_content")
		}
		if row.result.Valid {
			t.Errorf("result_content = %q, want NULL", row.result.String)
		}
		if row.omitReason.String != toolpayload.OmitImage {
			t.Errorf("result_omitted_reason = %q, want %q", row.omitReason.String, toolpayload.OmitImage)
		}
		if row.resultLength.Int64 == 0 {
			t.Error("result_length = 0, want the payload's size")
		}
	})

	t.Run("array content concatenated", func(t *testing.T) {
		row := readBackfilled(t, database, "mcp__nanoclaw__ssh_localhost", "turn-toolu_ARR")
		if row.result.String != "nginx.service - active (running)" {
			t.Errorf("result_content = %q, want the text block", row.result.String)
		}
	})

	t.Run("unanswered call has no result", func(t *testing.T) {
		row := readBackfilled(t, database, "Bash", "turn-toolu_ORPHAN")
		if row.resultLength.Valid {
			t.Errorf("result_length = %d, want NULL — nothing answered this call", row.resultLength.Int64)
		}
		if row.result.Valid || row.omitReason.Valid {
			t.Errorf("got result=%v reason=%v, want both NULL", row.result, row.omitReason)
		}
		// The id and input still backfill; only the result half is absent.
		if row.toolUseID.String != "toolu_ORPHAN" {
			t.Errorf("tool_use_id = %q, want %q", row.toolUseID.String, "toolu_ORPHAN")
		}
	})
}

// TestMigration009BackfillsFTS covers the half of the change that makes the
// payloads reachable. Searching for a command that was only ever inside
// raw_json has to find the row.
func TestMigration009BackfillsFTS(t *testing.T) {
	dir := seedPre009(t, payloadFixtures())

	database, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = database.Close() }()

	t.Run("input is searchable", func(t *testing.T) {
		if got := toolUsesFTSMatchCount(t, database, `"git rebase"`); got != 1 {
			t.Errorf("matches for a command in a tool input = %d, want 1", got)
		}
	})

	t.Run("result is searchable", func(t *testing.T) {
		if got := toolUsesFTSMatchCount(t, database, `"refs/heads/topic"`); got != 1 {
			t.Errorf("matches for text in a tool result = %d, want 1", got)
		}
	})

	t.Run("omitted content is not indexed", func(t *testing.T) {
		if got := toolUsesFTSMatchCount(t, database, backfillCanary); got != 0 {
			t.Errorf("base64 canary matched %d rows, want 0", got)
		}
		if got := toolUsesFTSMatchCount(t, database, `"file line"`); got != 0 {
			t.Errorf("bulk read body matched %d rows, want 0", got)
		}
	})
}

func toolUsesFTSMatchCount(t *testing.T, database *DB, match string) int {
	t.Helper()
	var n int
	if err := database.QueryRow(
		"SELECT COUNT(*) FROM tool_uses_fts WHERE tool_uses_fts MATCH ?", match).Scan(&n); err != nil {
		t.Fatalf("fts match %q: %v", match, err)
	}
	return n
}

// TestMigration009Replays covers the rewind path. Anything that resets
// schema_version re-runs every migration above the rewind point, and a
// backfill that is not idempotent would double-apply or abort.
func TestMigration009Replays(t *testing.T) {
	dir := seedPre009(t, payloadFixtures())

	database, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	before := readBackfilled(t, database, "Bash", "turn-toolu_BASH")
	beforeFTS := toolUsesFTSMatchCount(t, database, `"git rebase"`)

	if _, err := database.Exec("DELETE FROM schema_version WHERE version >= 9"); err != nil {
		t.Fatalf("rewind schema_version: %v", err)
	}
	if err := RunMigrations(database.DB); err != nil {
		t.Fatalf("replay migrations: %v", err)
	}

	after := readBackfilled(t, database, "Bash", "turn-toolu_BASH")
	if after != before {
		t.Errorf("replay changed the row:\n before = %+v\n  after = %+v", before, after)
	}
	if got := toolUsesFTSMatchCount(t, database, `"git rebase"`); got != beforeFTS {
		t.Errorf("replay changed FTS matches: %d, want %d", got, beforeFTS)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestInsertToolUsesWritesPayloads covers the live path, which has to agree
// with what the backfill produces: the same policy applied to the same
// transcript has to give the same row either way.
func TestInsertToolUsesWritesPayloads(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	seedSessionForToolUses(t, database, "sess-live", "turn-live")

	ts := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	in := []models.ToolUse{
		{
			TurnID: "turn-live", SessionID: "sess-live", ToolName: "Bash", Timestamp: ts,
			ToolUseID: "toolu_LIVE", InputJSON: `{"command":"make test"}`, InputLength: 23,
			HasResult: true, ResultContent: "ok ccvault 1.2s", ResultLength: 15,
		},
		{
			TurnID: "turn-live", SessionID: "sess-live", ToolName: "Read", Timestamp: ts,
			ToolUseID: "toolu_LIVEREAD", FilePath: "/tmp/x", InputJSON: `{"file_path":"/tmp/x"}`, InputLength: 22,
			HasResult: true, ResultLength: 90210, ResultOmittedReason: toolpayload.OmitBulkRead,
		},
		{
			TurnID: "turn-live", SessionID: "sess-live", ToolName: "Bash", Timestamp: ts,
			ToolUseID: "toolu_LIVEORPHAN", InputJSON: `{"command":"sleep 1"}`, InputLength: 21,
		},
	}
	if err := database.InsertToolUses(in); err != nil {
		t.Fatalf("InsertToolUses: %v", err)
	}

	t.Run("stored result round trips", func(t *testing.T) {
		row := readBackfilled(t, database, "Bash", "turn-live")
		if row.toolUseID.String != "toolu_LIVE" || row.result.String != "ok ccvault 1.2s" {
			t.Errorf("got %+v", row)
		}
		if row.omitReason.Valid {
			t.Errorf("result_omitted_reason = %q, want NULL", row.omitReason.String)
		}
	})

	t.Run("omitted result keeps its length and reason", func(t *testing.T) {
		var length sql.NullInt64
		var content, reason sql.NullString
		if err := database.QueryRow(`SELECT result_content, result_length, result_omitted_reason
			FROM tool_uses WHERE tool_use_id = 'toolu_LIVEREAD'`).Scan(&content, &length, &reason); err != nil {
			t.Fatalf("query: %v", err)
		}
		if content.Valid {
			t.Errorf("result_content = %q, want NULL", content.String)
		}
		if length.Int64 != 90210 || reason.String != toolpayload.OmitBulkRead {
			t.Errorf("got length=%v reason=%v", length, reason)
		}
	})

	t.Run("no result stores NULL length", func(t *testing.T) {
		var length sql.NullInt64
		if err := database.QueryRow(
			`SELECT result_length FROM tool_uses WHERE tool_use_id = 'toolu_LIVEORPHAN'`).Scan(&length); err != nil {
			t.Fatalf("query: %v", err)
		}
		if length.Valid {
			t.Errorf("result_length = %d, want NULL", length.Int64)
		}
	})

	t.Run("empty id stores NULL, not the empty string", func(t *testing.T) {
		if err := database.InsertToolUses([]models.ToolUse{{
			TurnID: "turn-live", SessionID: "sess-live", ToolName: "email", Timestamp: ts,
			ToolUseID: "", InputJSON: `{"operation":"list"}`, InputLength: 20,
		}}); err != nil {
			t.Fatalf("InsertToolUses: %v", err)
		}
		var nonNull int
		if err := database.QueryRow(
			`SELECT COUNT(*) FROM tool_uses WHERE tool_name = 'email' AND tool_use_id IS NOT NULL`).Scan(&nonNull); err != nil {
			t.Fatalf("query: %v", err)
		}
		if nonNull != 0 {
			t.Error("an unrecorded tool_use_id was stored as '' rather than NULL, which makes the empty string a third state")
		}
	})

	t.Run("live inserts are searchable", func(t *testing.T) {
		if got := toolUsesFTSMatchCount(t, database, `"make test"`); got != 1 {
			t.Errorf("matches = %d, want 1", got)
		}
	})
}

// TestToolUsesFTSLeavesNoGhosts is the lesson from #42 applied to the new
// index. COUNT(*) on an external-content FTS table resolves through the base
// table and cannot see an orphaned entry, and PRAGMA integrity_check with no
// argument also returns clean. Only the docsize shadow table gives a real
// count, and only integrity-check with argument 1 inspects the index itself.
func TestToolUsesFTSLeavesNoGhosts(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()
	seedSessionForToolUses(t, database, "sess-ghost", "turn-ghost")

	ts := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	rows := []models.ToolUse{
		{TurnID: "turn-ghost", SessionID: "sess-ghost", ToolName: "Bash", Timestamp: ts,
			ToolUseID: "toolu_G1", InputJSON: `{"command":"ghostprobeone"}`, InputLength: 26,
			HasResult: true, ResultContent: "ghostresultone", ResultLength: 14},
		{TurnID: "turn-ghost", SessionID: "sess-ghost", ToolName: "Grep", Timestamp: ts,
			ToolUseID: "toolu_G2", InputJSON: `{"pattern":"ghostprobetwo"}`, InputLength: 27,
			HasResult: true, ResultContent: "ghostresulttwo", ResultLength: 14},
	}
	if err := database.InsertToolUses(rows); err != nil {
		t.Fatalf("InsertToolUses: %v", err)
	}

	if got := docsizeCount(t, database); got != 2 {
		t.Fatalf("docsize rows after insert = %d, want 2", got)
	}

	// The re-sync shape: delete the session's tool uses, insert them again.
	if err := database.WithTx(func(tx *sql.Tx) error {
		if err := database.DeleteToolUsesForSessionTx(tx, "sess-ghost"); err != nil {
			return err
		}
		return database.InsertToolUsesTx(tx, rows)
	}); err != nil {
		t.Fatalf("re-sync: %v", err)
	}

	if got := docsizeCount(t, database); got != 2 {
		t.Errorf("docsize rows after re-sync = %d, want 2 — the delete trigger left %d orphan(s)", got, got-2)
	}

	// Delete for real and confirm the index empties with the table.
	if err := database.WithTx(func(tx *sql.Tx) error {
		return database.DeleteToolUsesForSessionTx(tx, "sess-ghost")
	}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := docsizeCount(t, database); got != 0 {
		t.Errorf("docsize rows after delete = %d, want 0", got)
	}

	// integrity-check with argument 1 is the only form that inspects the
	// index rather than resolving through the content table.
	var result string
	if err := database.QueryRow(
		`INSERT INTO tool_uses_fts(tool_uses_fts, rank) VALUES('integrity-check', 1) RETURNING 'ok'`).Scan(&result); err != nil {
		// RETURNING is not available on an fts5 command insert; fall back to
		// running it for its error alone.
		if _, err := database.Exec(
			`INSERT INTO tool_uses_fts(tool_uses_fts, rank) VALUES('integrity-check', 1)`); err != nil {
			t.Errorf("fts5 integrity-check(1): %v", err)
		}
	}
}

func docsizeCount(t *testing.T, database *DB) int {
	t.Helper()
	var n int
	if err := database.QueryRow("SELECT COUNT(*) FROM tool_uses_fts_docsize").Scan(&n); err != nil {
		t.Fatalf("count docsize: %v", err)
	}
	return n
}

func seedSessionForToolUses(t *testing.T, database *DB, sessionID, turnID string) {
	t.Helper()
	ts := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if _, err := database.Exec(
		`INSERT INTO sessions (id, started_at, source_file, source) VALUES (?, ?, ?, 'claude-code')`,
		sessionID, ts, fmt.Sprintf("/fake/%s.jsonl", sessionID)); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if _, err := database.Exec(
		`INSERT INTO turns (id, session_id, type, timestamp, content) VALUES (?, ?, 'assistant', ?, '')`,
		turnID, sessionID, ts); err != nil {
		t.Fatalf("seed turn: %v", err)
	}
}
