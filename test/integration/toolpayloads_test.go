// ABOUTME: Drives the real binary through the upgrade an existing archive takes when migration 009 lands.
// ABOUTME: The source file is unchanged, so sync skips it and the payloads can only have come from the backfill.

package integration

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// timeLater is a timestamp comfortably after the fixture's recorded mtimes, so
// a touched file reads as newer than what source_files holds.
func timeLater() time.Time { return time.Now().Add(time.Hour) }

// The two needles this fixture proves are newly reachable, chosen because
// turns.content cannot hold either of them.
//
// turns.content is not empty of tool material — it never was. An assistant
// turn renders a call as "[Tool: Bash] $ <command>" with the command cut at
// 100 characters, and a string-valued tool_result renders as
// "[Tool Result: <text>]" cut at 200. So short commands and short string
// results have always been findable, in summary form.
//
// What was never reachable is what these two needles sit in:
//
//   - commandNeedle is past character 100 of its command, so the summary
//     truncates it away.
//   - resultNeedle is inside an array-valued tool_result. Half the archive's
//     results use that shape (130,516 of 259,812), and
//     models.UserContentBlock types Content as a string, so the array fails
//     the unmarshal and the turn stores no content at all.
const (
	commandNeedle = "lithesome-walrus"
	resultNeedle  = "quixotic-badger"
)

// toolSessionJSONL is a session holding a tool call and its result, in the
// shape Claude Code writes: the tool_use in an assistant message, the
// tool_result in the user message that follows, joined by tool_use_id.
const toolSessionJSONL = `{"uuid":"%[1]s-t1","parentUuid":null,"type":"user","message":{"role":"user","content":"check the cluster"},"timestamp":"2026-01-01T00:00:01Z","sessionId":%[1]q,"cwd":"/Users/test/fixture","version":"1.0"}
{"uuid":"%[1]s-t2","parentUuid":"%[1]s-t1","type":"assistant","message":{"id":"%[1]s-m1","model":"claude-sonnet-4-20250514","role":"assistant","content":[{"type":"tool_use","id":"toolu_UPGRADE","name":"Bash","input":{"command":"kubectl --namespace production --context staging-west describe pod api-gateway-7d9f8b6c5d --output wide ` + commandNeedle + `"}}],"usage":{"input_tokens":11,"output_tokens":7}},"timestamp":"2026-01-01T00:00:02Z","sessionId":%[1]q}
{"uuid":"%[1]s-t3","parentUuid":"%[1]s-t2","type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_UPGRADE","content":[{"type":"text","text":"Events: FailedScheduling insufficient ephemeral-storage on node ` + resultNeedle + `"}]}]},"timestamp":"2026-01-01T00:00:03Z","sessionId":%[1]q}
`

// writeToolSession writes a session whose only interesting text is inside a
// tool payload.
func writeToolSession(t *testing.T, claudeHome, sessionID string) string {
	t.Helper()

	dir := filepath.Join(claudeHome, "projects", "-Users-test-fixture")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, sessionID+".jsonl")
	if err := os.WriteFile(path, []byte(fmt.Sprintf(toolSessionJSONL, sessionID)), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// openArchive opens the fixture's database read-write for the surgery and
// assertions below.
func openArchive(t *testing.T, f *cliFixture) *sql.DB {
	t.Helper()

	database, err := sql.Open("sqlite", filepath.Join(f.DataDir, "ccvault.db"))
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	database.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = database.Close() })
	return database
}

// rewindBelow009 turns a fully-migrated archive back into the one the previous
// release left behind: the payload columns gone, the index over them gone, and
// schema_version back to 8.
//
// Reconstructing the old shape rather than building the old binary. The two
// produce the same archive — every other table, and crucially source_files
// with its recorded mtimes, is written by code this change does not touch — so
// the only difference between them is the six columns dropped here.
func rewindBelow009(t *testing.T, database *sql.DB) {
	t.Helper()

	stmts := []string{
		// The FTS table and its triggers reference the columns, so they go first.
		"DROP TRIGGER IF EXISTS tool_uses_ai",
		"DROP TRIGGER IF EXISTS tool_uses_ad",
		"DROP TRIGGER IF EXISTS tool_uses_au",
		"DROP TABLE IF EXISTS tool_uses_fts",
		"DROP INDEX IF EXISTS idx_tool_uses_tool_use_id",
		"ALTER TABLE tool_uses DROP COLUMN tool_use_id",
		"ALTER TABLE tool_uses DROP COLUMN input_json",
		"ALTER TABLE tool_uses DROP COLUMN input_length",
		"ALTER TABLE tool_uses DROP COLUMN result_content",
		"ALTER TABLE tool_uses DROP COLUMN result_length",
		"ALTER TABLE tool_uses DROP COLUMN result_omitted_reason",
		"DELETE FROM schema_version WHERE version >= 9",
	}
	for _, stmt := range stmts {
		if _, err := database.Exec(stmt); err != nil {
			t.Fatalf("rewind %q: %v", stmt, err)
		}
	}

	// Guard the guard. If the rewind silently left a column behind, the
	// assertions after the re-sync would prove nothing.
	var count int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('tool_uses')
		 WHERE name IN ('tool_use_id','input_json','input_length',
		                'result_content','result_length','result_omitted_reason')`).Scan(&count); err != nil {
		t.Fatalf("probe columns: %v", err)
	}
	if count != 0 {
		t.Fatalf("%d payload column(s) survived the rewind, so the upgrade test proves nothing", count)
	}
}

// TestUpgrade_BackfillsPayloadsWithoutReparsingAnything is the rehearsal that
// matters for an existing archive: the migration has to recover the payloads
// from raw_json, not quietly rely on a re-sync having re-read every file.
//
// The proof is the skip. Between the two runs the session file is untouched,
// so its recorded mtime still matches and sync reports it skipped — no turn is
// re-parsed, no tool_uses row is rewritten. Anything in a payload column
// afterwards came from migration 009 reading raw_json and nowhere else.
func TestUpgrade_BackfillsPayloadsWithoutReparsingAnything(t *testing.T) {
	f := newCLIFixture(t)
	sessionID := "11111111-2222-3333-4444-555555555555"
	writeToolSession(t, f.ClaudeHome, sessionID)

	// First run: a normal sync, which is what the previous release left behind
	// once the payload columns are taken back off.
	first := f.Run(t, "sync")
	if !strings.Contains(first.Stdout, "Tool uses: 1") {
		t.Fatalf("first sync did not index the tool call:\n%s", first.Stdout)
	}

	database := openArchive(t, f)
	rewindBelow009(t, database)

	// The archive is now a pre-009 archive, and rewindBelow009 has asserted
	// that none of the payload columns survive.
	//
	// There is deliberately no "search finds nothing yet" probe here. This
	// binary ships migration 009, so merely opening the archive applies it —
	// the pre-upgrade state is not observable through the only binary a Go
	// test has. The absence of the columns is the equivalent assertion, and it
	// is stronger: a column that does not exist cannot be searched. The
	// negative was confirmed separately by driving the actual pre-change
	// binary, which answers "No results found." for both needles.

	// Second run: the new binary. Migrations run on open; sync then finds
	// nothing to do.
	second := f.Run(t, "sync")
	// "0 indexed" is the load-bearing part: no session was re-parsed, so no
	// turn and no tool_uses row was rewritten. The skipped count is whatever
	// the fixture laid down, which is not what this test is about.
	if !strings.Contains(second.Stdout, "0 indexed") || strings.Contains(second.Stdout, ", 0 skipped") {
		t.Fatalf("the second sync re-parsed the sessions instead of skipping them, so the payloads "+
			"could have come from the parser rather than the backfill:\n%s", second.Stdout)
	}
	if !strings.Contains(second.Stdout, "Tool uses: 0") || !strings.Contains(second.Stdout, "Turns:     0") {
		t.Fatalf("the second sync wrote rows, so the backfill is not what filled them:\n%s",
			second.Stdout)
	}

	t.Run("the payloads are on the row", func(t *testing.T) {
		var toolUseID, input, result sql.NullString
		var resultLen sql.NullInt64
		if err := database.QueryRow(`SELECT tool_use_id, input_json, result_content, result_length
			FROM tool_uses WHERE tool_name = 'Bash'`).Scan(&toolUseID, &input, &result, &resultLen); err != nil {
			t.Fatalf("read the backfilled row: %v", err)
		}
		if toolUseID.String != "toolu_UPGRADE" {
			t.Errorf("tool_use_id = %q, want toolu_UPGRADE", toolUseID.String)
		}
		if !strings.Contains(input.String, commandNeedle) {
			t.Errorf("input_json = %q, want the whole command including %q", input.String, commandNeedle)
		}
		if !strings.Contains(result.String, resultNeedle) {
			t.Errorf("result_content = %q, want the tool output including %q", result.String, resultNeedle)
		}
		// An array-valued content records the content JSON's length, which runs
		// ahead of the extracted text by the JSON framing. See
		// toolpayload.Result.Length.
		if resultLen.Int64 <= int64(len(result.String)) {
			t.Errorf("result_length = %d, want more than the %d extracted bytes for an array content",
				resultLen.Int64, len(result.String))
		}
	})

	t.Run("and searchable through the binary", func(t *testing.T) {
		for _, probe := range []struct{ name, query, want string }{
			{"the tail of a long command, from the input", commandNeedle, commandNeedle},
			{"an array-shaped result, from the result", resultNeedle, resultNeedle},
		} {
			t.Run(probe.name, func(t *testing.T) {
				r := f.Run(t, "search", probe.query, "--json")
				if !strings.Contains(r.Stdout, probe.want) {
					t.Errorf("search %q found nothing containing %q:\n%s", probe.query, probe.want, r.Stdout)
				}
				if !strings.Contains(r.Stdout, `"matched_tool_name"`) &&
					!strings.Contains(r.Stdout, "matched_tool_name") {
					t.Errorf("search %q did not label the payload hit:\n%s", probe.query, r.Stdout)
				}
			})
		}
	})

	t.Run("a third sync is still a no-op", func(t *testing.T) {
		third := f.Run(t, "sync")
		if !strings.Contains(third.Stdout, "0 indexed") || strings.Contains(third.Stdout, ", 0 skipped") {
			t.Errorf("a settled archive re-synced:\n%s", third.Stdout)
		}
	})
}

// TestSync_WritesPayloadsOnAFreshArchive covers the other direction: a first
// sync has to produce the same row the backfill does, because the two are the
// same policy applied to the same transcript by different code.
func TestSync_WritesPayloadsOnAFreshArchive(t *testing.T) {
	f := newCLIFixture(t)
	sessionID := "66666666-7777-8888-9999-aaaaaaaaaaaa"
	writeToolSession(t, f.ClaudeHome, sessionID)

	f.Run(t, "sync")

	database := openArchive(t, f)
	var toolUseID, input, result sql.NullString
	var inputLen, resultLen sql.NullInt64
	var reason sql.NullString
	if err := database.QueryRow(`SELECT tool_use_id, input_json, input_length,
		result_content, result_length, result_omitted_reason
		FROM tool_uses WHERE tool_name = 'Bash'`).Scan(
		&toolUseID, &input, &inputLen, &result, &resultLen, &reason); err != nil {
		t.Fatalf("read the row: %v", err)
	}

	if toolUseID.String != "toolu_UPGRADE" {
		t.Errorf("tool_use_id = %q, want toolu_UPGRADE", toolUseID.String)
	}
	if inputLen.Int64 != int64(len(input.String)) {
		t.Errorf("input_length = %d, len(input_json) = %d", inputLen.Int64, len(input.String))
	}
	if !strings.Contains(result.String, resultNeedle) {
		t.Errorf("result_content = %q, want the tool output including %q", result.String, resultNeedle)
	}
	if resultLen.Int64 <= int64(len(result.String)) {
		t.Errorf("result_length = %d, want more than the %d extracted bytes for an array content",
			resultLen.Int64, len(result.String))
	}
	if reason.Valid {
		t.Errorf("result_omitted_reason = %q, want NULL", reason.String)
	}
}

// TestSync_ReSyncLeavesNoOrphanedFTSEntries applies the lesson from #42 to the
// new index, through the real binary. sync replaces a session's tool uses with
// a DELETE and an INSERT, and the docsize shadow table is the only place an
// orphan from that is visible — COUNT(*) on an external-content FTS table
// resolves through the base table and cannot see one.
func TestSync_ReSyncLeavesNoOrphanedFTSEntries(t *testing.T) {
	f := newCLIFixture(t)
	sessionID := "bbbbbbbb-cccc-dddd-eeee-ffffffffffff"
	path := writeToolSession(t, f.ClaudeHome, sessionID)

	f.Run(t, "sync")
	database := openArchive(t, f)

	count := func(query string) int64 {
		var n int64
		if err := database.QueryRow(query).Scan(&n); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return n
	}

	rows := count("SELECT COUNT(*) FROM tool_uses")
	if docs := count("SELECT COUNT(*) FROM tool_uses_fts_docsize"); docs != rows {
		t.Fatalf("after the first sync: %d indexed documents for %d rows", docs, rows)
	}

	// Touch the file so the next sync re-parses it rather than skipping.
	if err := os.Chtimes(path, timeLater(), timeLater()); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	second := f.Run(t, "sync")
	if !strings.Contains(second.Stdout, "1 indexed") {
		t.Fatalf("the touched file was not re-parsed:\n%s", second.Stdout)
	}

	rows = count("SELECT COUNT(*) FROM tool_uses")
	docs := count("SELECT COUNT(*) FROM tool_uses_fts_docsize")
	if docs != rows {
		t.Errorf("after a re-sync: %d indexed documents for %d rows — %d orphan(s) left behind",
			docs, rows, docs-rows)
	}

	// integrity-check with argument 1 is the form that inspects the index
	// itself; with no argument it resolves through the content table and
	// reports clean regardless.
	if _, err := database.Exec(
		`INSERT INTO tool_uses_fts(tool_uses_fts, rank) VALUES('integrity-check', 1)`); err != nil {
		t.Errorf("tool_uses_fts integrity-check(1): %v", err)
	}
}
