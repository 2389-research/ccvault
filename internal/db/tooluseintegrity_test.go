// ABOUTME: Tests for tool_uses referential integrity — migration 011's cleanup and
// ABOUTME: the (turn_id, turn_ordinal) key that stops orphans and duplicates recurring.

package db

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/2389-research/ccvault/pkg/models"
)

// toolUseSeed is one pre-011 tool_uses row, written with the columns the schema
// held below migration 011 — which is every column except turn_ordinal.
type toolUseSeed struct {
	turnID    string
	sessionID string
	toolName  string
	toolUseID string // empty means the column stays NULL
	inputJSON string // empty means the column stays NULL
	hasResult bool
}

// seedPre011 builds an archive holding the given turns and tool uses under the
// schema as it stood *below* migration 011, then closes it and returns its
// directory.
//
// Applying only the migrations below 011 is what makes the cleanup tests
// non-vacuous: at seed time there is no turn_ordinal column and no unique
// index, and nothing stops a duplicate or an orphan from being written. Every
// assertion below can therefore only be satisfied by migration 011 running
// afterwards.
func seedPre011(t *testing.T, seed integritySeed) string {
	t.Helper()

	dir := t.TempDir()
	raw, err := sql.Open("sqlite", filepath.Join(dir, dbFileName))
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
		if m.version >= 11 {
			continue
		}
		if err := applyMigration(raw, m); err != nil {
			t.Fatalf("apply %03d: %v", m.version, err)
		}
	}

	// Guard the guard. If a future edit lets the column or the index exist
	// below 011, these tests would pass without proving anything.
	if columnExists(t, raw, "tool_uses", "turn_ordinal") {
		t.Fatal("tool_uses.turn_ordinal exists below migration 011, so the cleanup tests prove nothing")
	}
	if indexExists(t, raw, toolUsesTurnOrdinalIndex) {
		t.Fatalf("%s exists below migration 011, so the key tests prove nothing", toolUsesTurnOrdinalIndex)
	}

	base := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	for _, id := range seed.sessions {
		if _, err := raw.Exec(`INSERT INTO sessions (id, started_at, source_file, source, model, git_branch)
			VALUES (?, ?, ?, 'claude-code', '', '')`, id, base, "/fake/"+id+".jsonl"); err != nil {
			t.Fatalf("seed session %s: %v", id, err)
		}
	}
	for _, turn := range seed.turns {
		if _, err := raw.Exec(
			`INSERT INTO turns (id, session_id, type, timestamp, content, raw_json, ordinal)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			turn.ID, turn.SessionID, turn.Type, turn.Timestamp, turn.Content, string(turn.RawJSON), turn.Ordinal); err != nil {
			t.Fatalf("seed turn %s: %v", turn.ID, err)
		}
	}
	for i, tu := range seed.toolUses {
		var resultLength any
		if tu.hasResult {
			resultLength = 7
		}
		if _, err := raw.Exec(
			`INSERT INTO tool_uses (turn_id, session_id, tool_name, timestamp, tool_use_id, input_json, result_length)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			tu.turnID, tu.sessionID, tu.toolName, base.Add(time.Duration(i)*time.Second),
			nullIfEmpty(tu.toolUseID), nullIfEmpty(tu.inputJSON), resultLength); err != nil {
			t.Fatalf("seed tool use %d: %v", i, err)
		}
	}

	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}
	return dir
}

// indexExists reports whether an index of that name is in the schema.
func indexExists(t *testing.T, raw *sql.DB, name string) bool {
	t.Helper()
	var count int
	if err := raw.QueryRow(
		"SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?", name).Scan(&count); err != nil {
		t.Fatalf("look up index %s: %v", name, err)
	}
	return count > 0
}

// assistantRawWithCalls renders an assistant turn's raw_json carrying n
// tool_use blocks, which is the transcript's own count of how many calls the
// turn really made.
func assistantRawWithCalls(toolName string, ids ...string) string {
	blocks := ""
	for i, id := range ids {
		if i > 0 {
			blocks += ","
		}
		blocks += fmt.Sprintf(`{"type":"tool_use","id":%q,"name":%q,"input":{"n":%d}}`, id, toolName, i)
	}
	return fmt.Sprintf(`{"message":{"content":[%s]}}`, blocks)
}

// integritySeed is one pre-011 archive: its sessions, its turns, and its
// tool_uses rows.
type integritySeed struct {
	sessions []string
	turns    []models.Turn
	toolUses []toolUseSeed
}

// integrityFixture is the shape of every defect #82 and #87 describe, plus the
// legitimate shapes a cleanup must not touch, in one archive.
//
// The duplicate shapes are drawn from the author's archive, where they account
// for every one of the 1,298 (turn_id, tool_name) groups holding more than one
// row:
//
//   - 1,281 turns with two rows for one real call, the first carrying the
//     provider id migration 009 backfilled onto it and the second carrying
//     nothing. Their first row's session_id names the session the turn was
//     first ingested under, not the one it belongs to now.
//   - 7 turns with four rows repeating one provider id.
//   - 10 turns — including the two #82 reports as "15x" — that really did
//     issue that many calls of the same tool. Not duplicates at all, and the
//     thing a dedup keyed on (turn_id, tool_name) would destroy.
func integrityFixture() integritySeed {
	base := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)

	turn := func(id, sessionID string, ordinal int, raw string) models.Turn {
		return models.Turn{
			ID: id, SessionID: sessionID, Type: "assistant", Ordinal: ordinal,
			Timestamp: base.Add(time.Duration(ordinal) * time.Second),
			Content:   "[Tool use in " + id + "]",
			RawJSON:   []byte(raw),
		}
	}

	turns := []models.Turn{
		// A turn ingested twice: it belongs to sess-new now, and the stale
		// tool_uses row left behind still names sess-old.
		turn("turn-dup2", "sess-new", 0, assistantRawWithCalls("Bash", "toolu_dup2")),
		// A turn whose four rows all repeat one provider id.
		turn("turn-dup4", "sess-new", 1, assistantRawWithCalls("Grep", "toolu_dup4")),
		// A turn that really made three Read calls.
		turn("turn-parallel", "sess-new", 2, assistantRawWithCalls("Read", "toolu_p1", "toolu_p2", "toolu_p3")),
	}

	toolUses := []toolUseSeed{
		// turn-dup2: the row 009 filled, under the session the turn used to
		// belong to, then the empty extra.
		{turnID: "turn-dup2", sessionID: "sess-old", toolName: "Bash", toolUseID: "toolu_dup2", inputJSON: `{"n":0}`},
		{turnID: "turn-dup2", sessionID: "sess-new", toolName: "Bash"},
		// turn-dup4: one call, four rows, same id on all of them.
		{turnID: "turn-dup4", sessionID: "sess-new", toolName: "Grep", toolUseID: "toolu_dup4", inputJSON: `{"n":0}`, hasResult: true},
		{turnID: "turn-dup4", sessionID: "sess-new", toolName: "Grep", toolUseID: "toolu_dup4", inputJSON: `{"n":0}`, hasResult: true},
		{turnID: "turn-dup4", sessionID: "sess-new", toolName: "Grep", toolUseID: "toolu_dup4", inputJSON: `{"n":0}`, hasResult: true},
		{turnID: "turn-dup4", sessionID: "sess-new", toolName: "Grep", toolUseID: "toolu_dup4", inputJSON: `{"n":0}`, hasResult: true},
		// turn-parallel: three real calls of one tool. Every one of these must
		// survive.
		{turnID: "turn-parallel", sessionID: "sess-new", toolName: "Read", toolUseID: "toolu_p1", inputJSON: `{"n":0}`},
		{turnID: "turn-parallel", sessionID: "sess-new", toolName: "Read", toolUseID: "toolu_p2", inputJSON: `{"n":1}`},
		{turnID: "turn-parallel", sessionID: "sess-new", toolName: "Read", toolUseID: "toolu_p3", inputJSON: `{"n":2}`},
		// Orphans: a turn_id naming no turns row. Invisible to search, which
		// joins through turns, but counted by GetToolUsageStats.
		{turnID: "turn-gone", sessionID: "sess-new", toolName: "Edit", toolUseID: "toolu_orphan1", inputJSON: `{}`},
		{turnID: "turn-gone", sessionID: "sess-new", toolName: "Write", toolUseID: "toolu_orphan2", inputJSON: `{}`},
	}
	return integritySeed{sessions: []string{"sess-old", "sess-new"}, turns: turns, toolUses: toolUses}
}

// toolUseRow is what the assertions read back.
type toolUseRow struct {
	id        int64
	turnID    string
	sessionID string
	toolName  string
	toolUseID sql.NullString
	inputJSON sql.NullString
	ordinal   int
}

func readToolUses(t *testing.T, database *DB) []toolUseRow {
	t.Helper()
	rows, err := database.Query(`SELECT id, turn_id, session_id, tool_name, tool_use_id, input_json, turn_ordinal
		FROM tool_uses ORDER BY turn_id, turn_ordinal`)
	if err != nil {
		t.Fatalf("read tool_uses: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var out []toolUseRow
	for rows.Next() {
		var r toolUseRow
		if err := rows.Scan(&r.id, &r.turnID, &r.sessionID, &r.toolName, &r.toolUseID, &r.inputJSON, &r.ordinal); err != nil {
			t.Fatalf("scan tool_uses: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate tool_uses: %v", err)
	}
	return out
}

func toolUsesForTurn(rows []toolUseRow, turnID string) []toolUseRow {
	var out []toolUseRow
	for _, r := range rows {
		if r.turnID == turnID {
			out = append(out, r)
		}
	}
	return out
}

// TestMigration011CleansUpToolUses is the cleanup proof: one pass that removes
// the orphans of #87 and the duplicates of #82 and leaves everything else
// alone.
func TestMigration011CleansUpToolUses(t *testing.T) {
	dir := seedPre011(t, integrityFixture())

	database, err := Open(dir)
	if err != nil {
		t.Fatalf("open after migration: %v", err)
	}
	defer func() { _ = database.Close() }()

	rows := readToolUses(t, database)
	if len(rows) != 5 {
		t.Errorf("tool_uses holds %d rows after the cleanup, want 5 (1 + 1 + 3)", len(rows))
		for _, r := range rows {
			t.Logf("  id=%d turn=%s session=%s tool=%s tool_use_id=%v ordinal=%d",
				r.id, r.turnID, r.sessionID, r.toolName, r.toolUseID.String, r.ordinal)
		}
	}

	// The orphans are gone outright.
	if got := toolUsesForTurn(rows, "turn-gone"); len(got) != 0 {
		t.Errorf("%d orphaned rows survived; they reference a turn that does not exist", len(got))
	}

	// The two-row turn keeps one row, and keeps the one carrying the payload
	// rather than the empty extra.
	dup2 := toolUsesForTurn(rows, "turn-dup2")
	if len(dup2) != 1 {
		t.Fatalf("turn-dup2 holds %d rows, want 1 — its message records one tool_use block", len(dup2))
	}
	if dup2[0].toolUseID.String != "toolu_dup2" {
		t.Errorf("turn-dup2 kept the row with tool_use_id %q, want the one carrying toolu_dup2",
			dup2[0].toolUseID.String)
	}
	if dup2[0].inputJSON.String != `{"n":0}` {
		t.Errorf("turn-dup2 kept a row with input_json %q; the cleanup dropped the payload",
			dup2[0].inputJSON.String)
	}
	// And its session_id now agrees with its turn's, which is what makes the
	// row reachable from the session it actually belongs to.
	if dup2[0].sessionID != "sess-new" {
		t.Errorf("turn-dup2's row names session %q, but its turn belongs to sess-new", dup2[0].sessionID)
	}

	// Four rows for one call collapse to one.
	if got := toolUsesForTurn(rows, "turn-dup4"); len(got) != 1 {
		t.Errorf("turn-dup4 holds %d rows, want 1 — four rows repeated one provider id", len(got))
	}

	// Three real calls of one tool are not duplicates. A dedup keyed on
	// (turn_id, tool_name) would have left one.
	parallel := toolUsesForTurn(rows, "turn-parallel")
	if len(parallel) != 3 {
		t.Fatalf("turn-parallel holds %d rows, want 3 — the turn really made three Read calls", len(parallel))
	}
	for i, want := range []string{"toolu_p1", "toolu_p2", "toolu_p3"} {
		if parallel[i].toolUseID.String != want {
			t.Errorf("turn-parallel row %d carries %q, want %q", i, parallel[i].toolUseID.String, want)
		}
	}

	assertFTSSound(t, database, "after migration 011")
}

// TestMigration011NumbersSurvivingCallsWithinTheirTurn covers the other half
// of the migration: the key. The ordinals have to come out the way a fresh
// parse of the same transcript would write them, or the next sync's
// delete-then-insert would not land on the rows already there.
func TestMigration011NumbersSurvivingCallsWithinTheirTurn(t *testing.T) {
	dir := seedPre011(t, integrityFixture())

	database, err := Open(dir)
	if err != nil {
		t.Fatalf("open after migration: %v", err)
	}
	defer func() { _ = database.Close() }()

	rows := readToolUses(t, database)
	byTurn := map[string][]int{}
	for _, r := range rows {
		byTurn[r.turnID] = append(byTurn[r.turnID], r.ordinal)
	}
	for turnID, ordinals := range byTurn {
		for i, got := range ordinals {
			if got != i {
				t.Errorf("turn %s's calls are numbered %v, want 0..%d gapless", turnID, ordinals, len(ordinals)-1)
				break
			}
		}
	}

	// The key itself: a second row at the same position in the same turn is
	// refused rather than accepted or silently swapped in.
	_, err = database.Exec(
		`INSERT INTO tool_uses (turn_id, session_id, tool_name, timestamp, turn_ordinal)
		 VALUES ('turn-parallel', 'sess-new', 'Read', ?, 0)`, time.Now())
	if err == nil {
		t.Fatal("a second call at ordinal 0 of turn-parallel was accepted; tool_uses has no key")
	}
}

// TestMigration011NumbersTheWayTheWritePathDoes is the invariant the whole
// design rests on: the positions the migration assigns have to be the positions
// the write path would assign for the same transcript. If they disagree by even
// one, the next sync's delete-then-insert misses the rows already there and the
// turn ends up with both sets — which is #82 again.
//
// Proved by re-writing the surviving calls through InsertToolUses in transcript
// order and asserting the table does not grow. Verified end to end as well:
// `sync --full` over the fork-shaped fixture hands the writer four tool uses,
// two from each transcript holding the shared turn, and the table stays at two
// rows at ordinals 0 and 1.
func TestMigration011NumbersTheWayTheWritePathDoes(t *testing.T) {
	dir := seedPre011(t, integrityFixture())

	database, err := Open(dir)
	if err != nil {
		t.Fatalf("open after migration: %v", err)
	}
	defer func() { _ = database.Close() }()

	before := readToolUses(t, database)

	// The calls the transcripts record, in the order a parser reads them —
	// which is what sync hands to InsertToolUses.
	ts := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	reparsed := []models.ToolUse{
		{TurnID: "turn-dup2", SessionID: "sess-new", ToolName: "Bash", Timestamp: ts,
			ToolUseID: "toolu_dup2", InputJSON: `{"n":0}`},
		{TurnID: "turn-dup4", SessionID: "sess-new", ToolName: "Grep", Timestamp: ts,
			ToolUseID: "toolu_dup4", InputJSON: `{"n":0}`},
		{TurnID: "turn-parallel", SessionID: "sess-new", ToolName: "Read", Timestamp: ts,
			ToolUseID: "toolu_p1", InputJSON: `{"n":0}`},
		{TurnID: "turn-parallel", SessionID: "sess-new", ToolName: "Read", Timestamp: ts,
			ToolUseID: "toolu_p2", InputJSON: `{"n":1}`},
		{TurnID: "turn-parallel", SessionID: "sess-new", ToolName: "Read", Timestamp: ts,
			ToolUseID: "toolu_p3", InputJSON: `{"n":2}`},
	}
	if err := database.InsertToolUses(reparsed); err != nil {
		t.Fatalf("re-write the migrated calls: %v", err)
	}

	after := readToolUses(t, database)
	if len(after) != len(before) {
		t.Fatalf("re-writing the same calls took tool_uses from %d rows to %d; the migration and "+
			"the write path do not agree on where a call belongs", len(before), len(after))
	}
	for i := range after {
		if after[i].turnID != before[i].turnID || after[i].ordinal != before[i].ordinal ||
			after[i].toolUseID != before[i].toolUseID {
			t.Errorf("row %d moved: %s@%d %q -> %s@%d %q", i,
				before[i].turnID, before[i].ordinal, before[i].toolUseID.String,
				after[i].turnID, after[i].ordinal, after[i].toolUseID.String)
		}
	}
	assertFTSSound(t, database, "after re-writing the migrated calls")
}

// TestMigration011Replays guards the migrator's replay path: anything that
// rewinds schema_version re-runs 011, and a second pass must not delete a row
// or renumber one.
func TestMigration011Replays(t *testing.T) {
	dir := seedPre011(t, integrityFixture())

	first, err := Open(dir)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	before := readToolUses(t, first)
	if _, err := first.Exec("DELETE FROM schema_version WHERE version >= 11"); err != nil {
		t.Fatalf("rewind schema_version: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first: %v", err)
	}

	second, err := Open(dir)
	if err != nil {
		t.Fatalf("replay must be a no-op, got: %v", err)
	}
	defer func() { _ = second.Close() }()

	after := readToolUses(t, second)
	if len(after) != len(before) {
		t.Fatalf("replay changed the row count from %d to %d", len(before), len(after))
	}
	for i := range after {
		if after[i] != before[i] {
			t.Errorf("replay changed row %d: %+v -> %+v", i, before[i], after[i])
		}
	}
	assertFTSSound(t, second, "after replaying migration 011")
}

// TestInsertToolUsesNumbersCallsWithinTheirTurn is the write path's half of the
// key: the ordinal is a position in the turn, so a turn's calls get 0, 1, 2…
// and two turns in one batch each start at 0.
func TestInsertToolUsesNumbersCallsWithinTheirTurn(t *testing.T) {
	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = database.Close() }()

	seedTurnsForToolUses(t, database, "sess-w", "turn-a", "turn-b")

	if err := database.InsertToolUses(twoCallsEach("sess-w", "turn-a", "turn-b")); err != nil {
		t.Fatalf("insert tool uses: %v", err)
	}

	for _, turnID := range []string{"turn-a", "turn-b"} {
		got := toolUsesForTurn(readToolUses(t, database), turnID)
		if len(got) != 2 {
			t.Fatalf("%s holds %d rows, want 2", turnID, len(got))
		}
		if got[0].ordinal != 0 || got[1].ordinal != 1 {
			t.Errorf("%s's calls are numbered %d and %d, want 0 and 1", turnID, got[0].ordinal, got[1].ordinal)
		}
	}
}

// TestInsertToolUsesReplacesRatherThanDuplicating is the recurrence proof for
// #82. Re-ingesting the same transcript is what produced the duplicates, and
// the second write must land on the rows the first one wrote.
func TestInsertToolUsesReplacesRatherThanDuplicating(t *testing.T) {
	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = database.Close() }()

	seedTurnsForToolUses(t, database, "sess-w", "turn-a", "turn-b")

	batch := twoCallsEach("sess-w", "turn-a", "turn-b")
	for pass := 1; pass <= 3; pass++ {
		if err := database.InsertToolUses(batch); err != nil {
			t.Fatalf("pass %d: insert tool uses: %v", pass, err)
		}
		if got := len(readToolUses(t, database)); got != 4 {
			t.Fatalf("after pass %d tool_uses holds %d rows, want 4", pass, got)
		}
		assertFTSSound(t, database, fmt.Sprintf("after pass %d", pass))
	}
}

// TestInsertTurnsDeletesToolUsesOfTheTurnsItReplaces is the recurrence proof
// for #87, and the reason the fix is not ON DELETE CASCADE.
//
// InsertTurns resolves a position collision by deleting whatever holds the
// position, so a transcript that was rewritten rather than appended to — the
// case internal/sync reports as "rewritten upstream" — removes turns outright.
// Those turns' tool uses must go with them. They do not go by themselves:
// tool_uses.turn_id declares ON DELETE CASCADE and that declaration is inert,
// because PRAGMA foreign_keys is off on every connection ccvault opens. It is
// asserted below so that enabling it makes this test say so rather than
// quietly start proving something else.
//
// The tool use is filed under the session its turn belongs to, because since
// migration 012 that is the only state the write path can produce: a turn is
// identified by (session_id, id), so a row naming another session is that
// session's own call on its own copy of the turn rather than a mis-filed copy
// of this one. The mis-filed state this test used to seed is what migration
// 011 repaired, 1,281 rows of it — and reaching into another session to delete
// it is exactly the loss #92 is about, so the write path no longer does.
//
// What still has to hold, and is what this test is for: sync clears a
// session's tool uses by session_id, and that cannot see a row whose turn is
// being deleted by position rather than by name. #87's 6,378 orphans came from
// that gap.
func TestInsertTurnsDeletesToolUsesOfTheTurnsItReplaces(t *testing.T) {
	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = database.Close() }()

	var foreignKeys int
	if err := database.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatalf("read foreign_keys: %v", err)
	}
	if foreignKeys != 0 {
		t.Fatalf("PRAGMA foreign_keys is %d; this test exists because it is 0 and "+
			"tool_uses.turn_id's ON DELETE CASCADE therefore never fires", foreignKeys)
	}

	seedTurnsForToolUses(t, database, "sess-a", "turn-vanishing")
	if err := database.InsertToolUses([]models.ToolUse{{
		TurnID: "turn-vanishing", SessionID: "sess-a", ToolName: "Bash",
		Timestamp: time.Now(), ToolUseID: "toolu_vanishing", InputJSON: `{"cmd":"ls"}`,
	}}); err != nil {
		t.Fatalf("insert tool use: %v", err)
	}

	// The rewritten transcript: a different turn now occupies ordinal 0 of
	// sess-a, so turn-vanishing is deleted rather than replaced.
	if err := database.InsertTurns([]models.Turn{{
		ID: "turn-replacement", SessionID: "sess-a", Type: "assistant",
		Timestamp: time.Now(), Content: "different history", Ordinal: 0,
	}}); err != nil {
		t.Fatalf("insert replacement turn: %v", err)
	}

	// Orphaned against the turn's full identity, (session_id, id), not its
	// uuid alone — a row naming a uuid some other session holds is still a row
	// pointing at a turn that does not exist.
	var orphans int
	if err := database.QueryRow(`SELECT COUNT(*) FROM tool_uses tu
		WHERE tu.turn_id IS NOT NULL
		  AND NOT EXISTS (SELECT 1 FROM turns t
		                  WHERE t.id = tu.turn_id AND t.session_id = tu.session_id)`).Scan(&orphans); err != nil {
		t.Fatalf("count orphans: %v", err)
	}
	if orphans != 0 {
		t.Errorf("%d tool_uses rows reference a turn that no longer exists", orphans)
	}
	assertFTSSound(t, database, "after the transcript was rewritten")
}

// TestDeleteTurnsForSessionTakesTheirToolUses covers the other turn-deleting
// path: a session's turns go, so the calls those turns issued go with them.
//
// Another session's copy of the same turn uuid is seeded alongside, and has to
// survive. A turn's identity is (session_id, id) since migration 012 — a
// resumed transcript repeats the earlier session's uuids verbatim — so a row
// naming this uuid under another session is that session's own call, and
// taking it would be #92 again one table over. This test used to seed exactly
// that row as a *mis-filed* copy of sess-a's call, which is the pre-011 state
// migration 011 repaired and the write path can no longer create.
func TestDeleteTurnsForSessionTakesTheirToolUses(t *testing.T) {
	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = database.Close() }()

	seedSessionForToolUses(t, database, "sess-a", "turn-a")
	seedSessionForToolUses(t, database, "sess-b", "turn-a")
	if err := database.InsertToolUses([]models.ToolUse{{
		TurnID: "turn-a", SessionID: "sess-a", ToolName: "Bash",
		Timestamp: time.Now(), ToolUseID: "toolu_x",
	}}); err != nil {
		t.Fatalf("insert tool use: %v", err)
	}
	if err := database.InsertToolUses([]models.ToolUse{{
		TurnID: "turn-a", SessionID: "sess-b", ToolName: "Bash",
		Timestamp: time.Now(), ToolUseID: "toolu_y",
	}}); err != nil {
		t.Fatalf("insert the other session's tool use: %v", err)
	}

	if err := database.DeleteTurnsForSession("sess-a"); err != nil {
		t.Fatalf("delete turns: %v", err)
	}

	var left int
	if err := database.QueryRow(
		"SELECT COUNT(*) FROM tool_uses WHERE session_id = 'sess-a'").Scan(&left); err != nil {
		t.Fatalf("count tool_uses: %v", err)
	}
	if left != 0 {
		t.Errorf("%d tool_uses rows outlived the turns they belong to", left)
	}
	var elsewhere int
	if err := database.QueryRow(
		"SELECT COUNT(*) FROM tool_uses WHERE session_id = 'sess-b'").Scan(&elsewhere); err != nil {
		t.Fatalf("count the other session's tool_uses: %v", err)
	}
	if elsewhere != 1 {
		t.Errorf("the other session's copy of the turn lost %d of its calls", 1-elsewhere)
	}
}

// TestMergeFromPre011SourceNumbersAndDeduplicatesToolUses covers the archive
// `ccvault import` is actually pointed at: a copy off another machine, or a
// backup, written before tool_uses had a key — which is the shape that made
// the duplicates in the first place.
//
// Two things have to happen. sharedColumns drops a column the incoming
// database lacks, so without special handling every imported call would land
// on turn_ordinal's DEFAULT 0 and the second call of a turn would violate the
// unique index, turning a recovery import into a hard failure. And the
// incoming rows have to replace the destination's rows for the same turns
// rather than be added to them: merge clears a picked session's tool uses by
// session_id, which cannot reach a row filed under a session the merge did not
// pick.
func TestMergeFromPre011SourceNumbersAndDeduplicatesToolUses(t *testing.T) {
	dest, cleanup := setupTestDB(t)
	defer cleanup()

	srcDir := seedPre011(t, integrityFixture())

	// The destination already holds a row for one of the incoming turns, filed
	// under a session the merge will not pick. This is the pre-011 archive
	// state, reproduced in the destination.
	if _, err := dest.Exec(`INSERT INTO sessions (id, started_at, source_file, source)
		VALUES ('sess-unrelated', ?, '/fake/sess-unrelated.jsonl', 'claude-code')`, time.Now()); err != nil {
		t.Fatalf("seed unrelated session: %v", err)
	}
	if _, err := dest.Exec(`INSERT INTO tool_uses (turn_id, session_id, tool_name, timestamp, turn_ordinal)
		VALUES ('turn-parallel', 'sess-unrelated', 'Read', ?, 0)`, time.Now()); err != nil {
		t.Fatalf("seed stale destination row: %v", err)
	}

	if _, err := dest.MergeFrom(filepath.Join(srcDir, dbFileName)); err != nil {
		t.Fatalf("MergeFrom a pre-011 archive: %v", err)
	}

	rows := readToolUses(t, dest)
	if len(rows) != 5 {
		t.Errorf("destination holds %d tool_uses rows after the import, want 5", len(rows))
		for _, r := range rows {
			t.Logf("  id=%d turn=%s session=%s tool=%s ordinal=%d", r.id, r.turnID, r.sessionID, r.toolName, r.ordinal)
		}
	}
	parallel := toolUsesForTurn(rows, "turn-parallel")
	if len(parallel) != 3 {
		t.Fatalf("turn-parallel holds %d rows, want its 3 real calls", len(parallel))
	}
	for i, r := range parallel {
		if r.ordinal != i {
			t.Errorf("turn-parallel's calls are numbered %d at position %d, want %d", r.ordinal, i, i)
		}
		if r.sessionID == "sess-unrelated" {
			t.Errorf("the stale destination row survived the import at ordinal %d", r.ordinal)
		}
	}
	assertFTSSound(t, dest, "after importing a pre-011 archive")
}

// seedTurnsForToolUses creates one session holding the named turns, each at
// its own ordinal. seedSessionForToolUses makes one turn per session, which the
// write-path tests cannot use — they need two turns in one batch to show that
// each turn's calls are numbered from zero independently.
func seedTurnsForToolUses(t *testing.T, database *DB, sessionID string, turnIDs ...string) {
	t.Helper()
	ts := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if _, err := database.Exec(
		`INSERT INTO sessions (id, started_at, source_file, source) VALUES (?, ?, ?, 'claude-code')`,
		sessionID, ts, "/fake/"+sessionID+".jsonl"); err != nil {
		t.Fatalf("seed session %s: %v", sessionID, err)
	}
	for i, turnID := range turnIDs {
		if _, err := database.Exec(
			`INSERT INTO turns (id, session_id, type, timestamp, content, ordinal)
			 VALUES (?, ?, 'assistant', ?, '', ?)`,
			turnID, sessionID, ts.Add(time.Duration(i)*time.Second), i); err != nil {
			t.Fatalf("seed turn %s: %v", turnID, err)
		}
	}
}

// twoCallsEach builds a batch holding two calls for each of the named turns,
// in the order a transcript records them.
func twoCallsEach(sessionID string, turnIDs ...string) []models.ToolUse {
	base := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	var out []models.ToolUse
	for _, turnID := range turnIDs {
		for i := 0; i < 2; i++ {
			out = append(out, models.ToolUse{
				TurnID:    turnID,
				SessionID: sessionID,
				ToolName:  "Bash",
				Timestamp: base,
				ToolUseID: fmt.Sprintf("toolu_%s_%d", turnID, i),
				InputJSON: fmt.Sprintf(`{"command":"echo %d"}`, i),
			})
		}
	}
	return out
}
