// ABOUTME: Tests migration 017 — the user turns that are really tool results are retagged, their text recovered, and nothing else touched (#185).
// ABOUTME: Red first: before the migration every one of these rows is type 'user', and 145,798 of them carry no content at all.

package db

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/2389-research/ccvault/pkg/parser"
)

// Every fixture line below is a real transcript line — verbatim from
// ~/.claude/projects/*/*.jsonl, or reproducing a shape measured in the
// author's 52,375-session archive with a base64 payload shortened to a canary.
// The archive's own numbers, for scale:
//
//	315,985  user turns whose blocks are all tool_result (all mislabeled)
//	145,798  of those stored with no content at all
//	    642  carrying an image payload
//	    287  carrying only tool_reference blocks
//	    150  whose result content is the empty string
//	  4,557  user turns with no raw_json at all, which no migration can read
//	      0  user turns mixing prose with a tool_result
const (
	fixtureStringResult = `{"parentUuid":"58a90e59-4beb-4c74-ae64-f4162834e22c","type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01PVw7s4BZn2inWM92iqezJb","content":"Launching skill: terminal-title"}]},"uuid":"b31a72c7-bc09-48a0-be90-127808042c81","timestamp":"2026-10-21T19:51:59.142Z","sessionId":"e1a3e1a3-cddb-45df-87d0-a0dce6720cd9"}`

	fixtureArrayResult = `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_017GM1tmyfaN9gZUwQRwfLg6","type":"tool_result","content":[{"type":"text","text":"Navigated to https://www.grainger.com"}]}]},"uuid":"84517671-4d85-45ec-97fd-a2e8176a0d46","timestamp":"2026-10-07T18:29:14.572Z","sessionId":"7e1a1285-b69f-424e-aa80-8a17211c9cec"}`

	fixtureTwoTextBlocks = `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01AWoBPAManu4bwD5hzEPwKC","content":[{"type":"text","text":"first line of the result"},{"type":"text","text":"second line of the result"}]}]},"uuid":"two-blocks","timestamp":"2026-10-07T18:29:14.572Z","sessionId":"sess-tr"}`

	// A result whose text runs past the 200-character preview and is not
	// ASCII. 67,845 of the 145,006 rows this migration fills carry a
	// non-ASCII character inside the first 200, which is what makes the
	// character-counting cut load-bearing rather than cosmetic.
	fixtureLongUnicodeResult = `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_long","content":[{"type":"text","text":"ああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああああ"}]}]},"uuid":"long-unicode","timestamp":"2026-10-07T18:29:14.572Z","sessionId":"sess-tr"}`

	fixtureToolReferenceResult = `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_0156PaJs1qt3Q1CvJUXaw8g3","content":[{"type":"tool_reference","tool_name":"TaskCreate"},{"type":"tool_reference","tool_name":"TaskList"}]}]},"uuid":"246eee6e-4d90-43c8-ab29-cfb313adc075","timestamp":"2026-10-06T15:05:49.349Z","sessionId":"7d7e2870-48aa-4e0b-ba3a-c8a777007e75"}`

	fixtureEmptyStringResult = `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_01FJB7n5bKmJ1RSdCrG6Yx6E","type":"tool_result","content":"","is_error":false}]},"uuid":"empty-string-result","timestamp":"2026-08-19T20:45:19.702Z","sessionId":"sess-tr"}`

	// The base64 is shortened to a canary; the shape is the archive's.
	fixtureImageResult = `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_01EET9Fq7bw6Zy6iY7EFFoFy","type":"tool_result","content":[{"type":"text","text":"Screenshot 'es-home-supporters' taken at 1440x700"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"BASE64PAYLOADCANARYAAAAAAAAAAAAAAAAAA"}}]}]},"uuid":"image-result","timestamp":"2026-10-07T18:29:14.572Z","sessionId":"sess-tr"}`

	// --- the negative controls ---

	fixtureMixedProseAndResult = `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"stop and read the failing test first"},{"type":"tool_result","tool_use_id":"toolu_mixed","content":[{"type":"text","text":"exit status 1"}]}]},"uuid":"mixed-blocks","timestamp":"2026-10-07T18:29:14.572Z","sessionId":"sess-tr"}`

	fixturePlainUserString = `{"type":"user","message":{"role":"user","content":"run the tests"},"uuid":"plain-string","timestamp":"2026-10-07T18:29:14.572Z","sessionId":"sess-tr"}`

	fixturePlainUserTextBlock = `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"run the tests"}]},"uuid":"plain-block","timestamp":"2026-10-07T18:29:14.572Z","sessionId":"sess-tr"}`

	// A pasted screenshot. Six of the archive's contentless user turns are
	// this, and three more are a pasted PDF: real user messages with no text
	// in them, which must stay `user` however empty they look.
	fixturePastedImage = `{"type":"user","message":{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"PASTEDIMAGECANARY"}}]},"uuid":"pasted-image","timestamp":"2026-10-07T18:29:14.572Z","sessionId":"sess-tr"}`

	fixtureEmptyBlockArray = `{"type":"user","message":{"role":"user","content":[]},"uuid":"empty-array","timestamp":"2026-10-07T18:29:14.572Z","sessionId":"sess-tr"}`

	// The oversized-line placeholder pkg/parser writes in place of a raw line
	// over 1 MB. Six of the archive's contentless user turns are tool results
	// reduced to this; the message is gone, so no SQL can recover the text and
	// the row must not be retagged on a guess.
	fixtureStrippedPlaceholder = `{"_ccvault_stripped":true,"_ccvault_original_size":1353901,"uuid":"stripped-line","parentUuid":"90efb867-90b3-4ee7-b97b-db7c0055b513","sessionId":"sess-tr","type":"user","timestamp":"2026-08-24T20:27:49.263Z"}`

	fixtureAssistantToolUse = `{"type":"assistant","message":{"model":"claude","role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls"}}]},"uuid":"assistant-call","timestamp":"2026-10-07T18:29:14.572Z","sessionId":"sess-tr"}`
)

// retagFixture is one seeded turn: its raw line, and the content the archive
// stored for it before migration 017.
type retagFixture struct {
	id      string
	rawJSON string
	// content as it was stored before the migration, which for these rows is
	// whatever the old extractUserContent produced.
	content string
}

// retagFixtures lists every seeded turn. The `content` values are what the
// pre-#185 parser produced: a byte-truncated preview for a string-valued
// result, and nothing at all for every array-valued one, because
// UserContentBlock.Content was typed as a string and the whole block array
// failed to unmarshal.
var retagFixtures = []retagFixture{
	{id: "string-result", rawJSON: fixtureStringResult, content: "[Tool Result: Launching skill: terminal-title]"},
	{id: "array-result", rawJSON: fixtureArrayResult, content: ""},
	{id: "two-text-blocks", rawJSON: fixtureTwoTextBlocks, content: ""},
	{id: "long-unicode", rawJSON: fixtureLongUnicodeResult, content: ""},
	{id: "tool-reference", rawJSON: fixtureToolReferenceResult, content: ""},
	{id: "empty-string-result", rawJSON: fixtureEmptyStringResult, content: ""},
	{id: "image-result", rawJSON: fixtureImageResult, content: ""},
	{id: "mixed-blocks", rawJSON: fixtureMixedProseAndResult, content: ""},
	{id: "plain-string", rawJSON: fixturePlainUserString, content: "run the tests"},
	{id: "plain-block", rawJSON: fixturePlainUserTextBlock, content: "run the tests"},
	{id: "pasted-image", rawJSON: fixturePastedImage, content: ""},
	{id: "empty-array", rawJSON: fixtureEmptyBlockArray, content: ""},
	{id: "stripped-line", rawJSON: fixtureStrippedPlaceholder, content: ""},
	{id: "no-raw-json", rawJSON: "", content: ""},
	{id: "assistant-call", rawJSON: fixtureAssistantToolUse, content: "[Tool: Bash] $ ls"},
}

// seedPre017 builds an archive at migration 016 holding every fixture above
// and returns its data directory.
func seedPre017(t *testing.T) string {
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
	applied := 0
	for _, m := range migrations {
		if m.version >= 17 {
			continue
		}
		if err := applyMigration(raw, m); err != nil {
			t.Fatalf("apply %03d: %v", m.version, err)
		}
		applied = m.version
	}
	if applied != 16 {
		t.Fatalf("seeded at migration %03d, want 016 — the fixture is not below the migration under test", applied)
	}

	ts := time.Date(2026, 10, 7, 18, 29, 14, 0, time.UTC)
	if _, err := raw.Exec(`INSERT INTO sessions (id, started_at, source_file, source, model, git_branch)
		VALUES ('sess-tr', ?, '/fake/sess-tr.jsonl', 'claude-code', '', '')`,
		FormatTimestamp(ts)); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	for ordinal, f := range retagFixtures {
		turnType := "user"
		if f.id == "assistant-call" {
			turnType = "assistant"
		}
		var rawJSON interface{}
		if f.rawJSON != "" {
			rawJSON = f.rawJSON
		}
		if _, err := raw.Exec(`INSERT INTO turns (id, session_id, type, timestamp, content, raw_json, ordinal)
			VALUES (?, 'sess-tr', ?, ?, ?, ?, ?)`,
			f.id, turnType, FormatTimestamp(ts), f.content, rawJSON, ordinal); err != nil {
			t.Fatalf("seed turn %s: %v", f.id, err)
		}
	}

	// Guard the guard. Every one of these rows is type 'user' (bar the
	// assistant control) before the migration runs, so an assertion that the
	// retag happened cannot pass by accident.
	var userCount int
	if err := raw.QueryRow(
		`SELECT COUNT(*) FROM turns WHERE type = 'tool_result'`).Scan(&userCount); err != nil {
		t.Fatalf("count tool_result: %v", err)
	}
	if userCount != 0 {
		t.Fatalf("%d turns already typed tool_result below migration 017", userCount)
	}

	return dir
}

// turnRow reads one seeded turn's type and content back.
func turnRow(t *testing.T, raw *sql.DB, id string) (string, string) {
	t.Helper()

	var turnType string
	var content sql.NullString
	if err := raw.QueryRow(
		`SELECT type, content FROM turns WHERE id = ?`, id).Scan(&turnType, &content); err != nil {
		t.Fatalf("read turn %s: %v", id, err)
	}
	return turnType, content.String
}

// openSeeded opens a seeded archive and runs every pending migration, which is
// what applies 017.
func openRetagSeeded(t *testing.T, dir string) *sql.DB {
	t.Helper()

	raw, err := sql.Open("sqlite", filepath.Join(dir, "ccvault.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })

	if err := RunMigrations(raw); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	return raw
}

// TestMigration017RetagsToolResultTurns is #185's backfill. Normally the
// write-path fix is the whole answer and a backfill is an admission — here it
// is the only option, because the mislabeled rows run from 2026-02 while the
// oldest transcript left on disk is dated Sep 25, so ~78% of them have no
// source file to resync from. raw_json is present on 100% of them and is the
// only surviving record.
func TestMigration017RetagsToolResultTurns(t *testing.T) {
	raw := openRetagSeeded(t, seedPre017(t))

	retagged := []string{
		"string-result", "array-result", "two-text-blocks", "long-unicode",
		"tool-reference", "empty-string-result", "image-result",
	}
	for _, id := range retagged {
		if turnType, _ := turnRow(t, raw, id); turnType != "tool_result" {
			t.Errorf("turn %s: type = %q, want %q", id, turnType, "tool_result")
		}
	}
}

// TestMigration017RecoversTheResultText covers the half that needs raw_json
// read rather than a column flipped: the 145,648 rows whose nested content the
// string-typed block struct could never unmarshal.
func TestMigration017RecoversTheResultText(t *testing.T) {
	raw := openRetagSeeded(t, seedPre017(t))

	for _, tc := range []struct{ id, want string }{
		{"array-result", "[Tool Result: Navigated to https://www.grainger.com]"},
		{"two-text-blocks", "[Tool Result: first line of the result\nsecond line of the result]"},
	} {
		if _, content := turnRow(t, raw, tc.id); content != tc.want {
			t.Errorf("turn %s: content = %q, want %q", tc.id, content, tc.want)
		}
	}
}

// TestMigration017AgreesWithTheParser is the assertion that keeps the two
// implementations of one rule from drifting. The migration writes `content` in
// SQL; pkg/parser writes it in Go for every session synced from here on. If
// they disagree, the next resync of a session silently rewrites its rows and
// turns_fts behind them.
//
// This is where the character-counting cut earns its place: SQLite's substr()
// counts characters and Go's slice counts bytes, and 47% of the rows the
// migration fills hold a non-ASCII character inside the first 200.
func TestMigration017AgreesWithTheParser(t *testing.T) {
	raw := openRetagSeeded(t, seedPre017(t))

	for _, f := range retagFixtures {
		if f.rawJSON == "" {
			continue
		}
		turn, err := parser.ParseTurn([]byte(f.rawJSON))
		if err != nil || turn == nil {
			t.Fatalf("fixture %s: ParseTurn: %v", f.id, err)
		}

		gotType, gotContent := turnRow(t, raw, f.id)
		if gotType != turn.Type {
			t.Errorf("turn %s: migration stored type %q, parser produces %q", f.id, gotType, turn.Type)
		}
		// Only rows the migration had to fill are compared on content. A row
		// that already carried a preview is left alone on purpose — see the
		// migration's own note — and so is a row that stays `user`: the
		// mixed-content turn's prose is recoverable by a resync and the
		// archive holds none of them, so the migration does not go near it.
		if turn.Type == "tool_result" && f.content == "" && gotContent != turn.Content {
			t.Errorf("turn %s: migration stored content %q, parser produces %q", f.id, gotContent, turn.Content)
		}
	}
}

// TestMigration017LeavesEverythingElseAlone is the negative control, and the
// reason the retag is stated as "every block is a tool_result" rather than
// "any block is".
func TestMigration017LeavesEverythingElseAlone(t *testing.T) {
	raw := openRetagSeeded(t, seedPre017(t))

	for _, tc := range []struct {
		id       string
		wantType string
		why      string
	}{
		{"mixed-blocks", "user", "prose beside a result is still a user message"},
		{"plain-string", "user", "a plain string message carries no result"},
		{"plain-block", "user", "a text block carries no result"},
		{"pasted-image", "user", "a pasted screenshot is a user message with no text"},
		{"empty-array", "user", "an empty block array is not a tool result"},
		{"stripped-line", "user", "the message is gone; nothing says this was a result"},
		{"no-raw-json", "user", "no raw_json at all, so nothing to read"},
		{"assistant-call", "assistant", "the retag must not reach another type"},
	} {
		if turnType, _ := turnRow(t, raw, tc.id); turnType != tc.wantType {
			t.Errorf("turn %s: type = %q, want %q — %s", tc.id, turnType, tc.wantType, tc.why)
		}
	}

	// The mixed turn's prose has to survive too. It is the thing the retag
	// would have hidden, and a resync now recovers it: the pre-#185 parser
	// stored nothing for this row because the array-valued result took the
	// whole block array's unmarshal down with it.
	if _, content := turnRow(t, raw, "mixed-blocks"); content != "" {
		t.Errorf("turn mixed-blocks: content = %q — the migration must not write content for a user turn", content)
	}
}

// TestMigration017KeepsBase64OutOfTheIndex is the half of issue #28's policy
// that still applies: an image payload is recognised by shape and never
// reaches turns.content, which feeds turns_fts. 642 of the archive's affected
// rows carry one, 191 of them beside text, and a stored partial that keeps the
// caption while dropping the screenshot is more misleading than no text.
func TestMigration017KeepsBase64OutOfTheIndex(t *testing.T) {
	raw := openRetagSeeded(t, seedPre017(t))

	var hits int
	if err := raw.QueryRow(
		`SELECT COUNT(*) FROM turns WHERE content LIKE '%BASE64PAYLOADCANARY%'
		    OR content LIKE '%PASTEDIMAGECANARY%'`).Scan(&hits); err != nil {
		t.Fatalf("scan content for canaries: %v", err)
	}
	if hits != 0 {
		t.Errorf("%d turns carry a base64 canary in content", hits)
	}

	if _, content := turnRow(t, raw, "image-result"); content != "" {
		t.Errorf("turn image-result: content = %q, want empty", content)
	}
}

// TestMigration017IsIdempotent runs the migration's statements a second time
// against an archive that has already had them. A second run must write zero
// rows — otherwise re-running it, or a replay like the one
// TestApplyMigrationReplaysAddColumn covers, would churn turns_fts for no
// reason.
func TestMigration017IsIdempotent(t *testing.T) {
	dir := seedPre017(t)
	raw := openRetagSeeded(t, dir)

	before := snapshotTurns(t, raw)

	m := migrationByVersion(t, 17)
	// Rewind so the migrator runs 017 again rather than skipping it.
	if _, err := raw.Exec(`DELETE FROM schema_version WHERE version = 17`); err != nil {
		t.Fatalf("rewind schema_version: %v", err)
	}
	if err := applyMigration(raw, m); err != nil {
		t.Fatalf("second apply: %v", err)
	}

	after := snapshotTurns(t, raw)
	if len(before) != len(after) {
		t.Fatalf("turn count changed: %d -> %d", len(before), len(after))
	}
	for id, want := range before {
		if got := after[id]; got != want {
			t.Errorf("turn %s changed on the second run: %q -> %q", id, want, got)
		}
	}
}

// TestMigration017KeepsFTSSearchable is what the integrity-check cannot say on
// its own: the index agrees with the table, and the recovered text is findable
// through it.
func TestMigration017KeepsFTSSearchable(t *testing.T) {
	raw := openRetagSeeded(t, seedPre017(t))

	var id string
	if err := raw.QueryRow(
		`SELECT t.id FROM turns_fts f JOIN turns t ON t.rowid = f.rowid
		  WHERE turns_fts MATCH 'grainger'`).Scan(&id); err != nil {
		t.Fatalf("search the recovered text: %v", err)
	}
	if id != "array-result" {
		t.Errorf("search matched turn %q, want %q", id, "array-result")
	}

	// The strict check the migration ends on, run again here so a failure
	// names FTS rather than arriving as a rolled-back migration.
	if _, err := raw.Exec(`INSERT INTO turns_fts(turns_fts, rank) VALUES('integrity-check', 1)`); err != nil {
		t.Errorf("turns_fts integrity-check: %v", err)
	}
}

// snapshotTurns reads every turn's type and content, keyed by id.
func snapshotTurns(t *testing.T, raw *sql.DB) map[string]string {
	t.Helper()

	rows, err := raw.Query(`SELECT id, type, COALESCE(content, '') FROM turns`)
	if err != nil {
		t.Fatalf("snapshot turns: %v", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]string)
	for rows.Next() {
		var id, turnType, content string
		if err := rows.Scan(&id, &turnType, &content); err != nil {
			t.Fatalf("scan snapshot: %v", err)
		}
		out[id] = turnType + "\x00" + content
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("snapshot rows: %v", err)
	}
	return out
}

// migrationByVersion returns one embedded migration by its number.
func migrationByVersion(t *testing.T, version int) migration {
	t.Helper()

	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	for _, m := range migrations {
		if m.version == version {
			return m
		}
	}
	t.Fatalf("migration %03d not found", version)
	return migration{}
}
