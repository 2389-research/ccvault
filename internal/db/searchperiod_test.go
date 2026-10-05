// ABOUTME: Tests migration 010's period column and the FTS indexes rebuilt around it
// ABOUTME: Covers the token the column derives, the indexes' integrity, and what the triggers maintain

package db

import (
	"fmt"
	"testing"
	"time"

	"github.com/2389-research/ccvault/pkg/models"
)

// ftsIntegrityCheck runs FTS5's own integrity-check on an index.
//
// The strict form, with rank 1, which compares every indexed document against
// the content table rather than only checking the index's internal structure.
// It is the only check that sees an orphaned entry: #42 established that
// COUNT(*) on an external-content table resolves through the content table and
// agrees with itself however far the index has drifted, and a plain
// PRAGMA integrity_check returns clean as well.
func ftsIntegrityCheck(t *testing.T, db *DB, table string) error {
	t.Helper()

	_, err := db.Exec(fmt.Sprintf("INSERT INTO %s(%s, rank) VALUES('integrity-check', 1)", table, table))
	return err
}

func ftsDocsizeCount(t *testing.T, db *DB, table string) int64 {
	t.Helper()

	var n int64
	if err := db.QueryRow("SELECT COUNT(*) FROM " + table + "_docsize").Scan(&n); err != nil {
		t.Fatalf("count %s_docsize: %v", table, err)
	}
	return n
}

// turnIDsMatching joins an FTS index back to turns and returns the ids it
// matched, in id order.
func turnIDsMatching(t *testing.T, db *DB, table, match string) []string {
	t.Helper()

	rows, err := db.Query(
		"SELECT t.id FROM turns t JOIN "+table+" f ON f.rowid = t.rowid WHERE "+
			table+" MATCH ? ORDER BY t.id", match)
	if err != nil {
		t.Fatalf("match %q on %s: %v", match, table, err)
	}
	defer func() { _ = rows.Close() }()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate matches: %v", err)
	}
	return ids
}

// seedPeriodTurn writes one turn, and one tool use on it, at a given instant.
func seedPeriodTurn(t *testing.T, db *DB, sessionID, turnID string, ts time.Time, ordinal int) {
	t.Helper()

	if err := db.InsertTurns([]models.Turn{{
		ID: turnID, SessionID: sessionID, Type: "assistant", Timestamp: ts,
		Ordinal: ordinal, Content: "turn content " + turnID,
	}}); err != nil {
		t.Fatalf("insert turn %s: %v", turnID, err)
	}
	if err := db.InsertToolUses([]models.ToolUse{{
		TurnID: turnID, SessionID: sessionID, ToolName: "Bash", Timestamp: ts,
		ToolUseID: "toolu_" + turnID, InputJSON: `{"command":"echo ` + turnID + `"}`, InputLength: 20,
		HasResult: true, ResultContent: "output " + turnID, ResultLength: 12,
	}}); err != nil {
		t.Fatalf("insert tool use for %s: %v", turnID, err)
	}
}

// TestSearchPeriod_ColumnHoldsTheTokens is the SQL half of the tie between
// migration 010 and internal/search. The Go side builds terms by formatting a
// time; the column builds them by slicing the stored text. They have to agree
// exactly, and a disagreement would not fail to build or to run — it would
// return no rows for a date-filtered search.
func TestSearchPeriod_ColumnHoldsTheTokens(t *testing.T) {
	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = database.Close() }()

	tests := []struct {
		name string
		ts   string
		want string
	}{
		{
			name: "a stored timestamp yields a day and a month token",
			ts:   "2026-10-05 13:34:02.179 +0000 UTC",
			want: "ccvd20261005 ccvym202610",
		},
		{
			name: "the RFC3339 spelling of the same instant yields the same tokens",
			ts:   "2026-10-05T13:34:02.179Z",
			want: "ccvd20261005 ccvym202610",
		},
		{
			name: "a date with no time is still a date",
			ts:   "2026-01-14",
			want: "ccvd20260114 ccvym202601",
		},
		{
			// An unpadded day would slice into "5 " and tokenize as a term in
			// no compiled set, so the row would vanish from a date-filtered
			// search. The sentinel keeps it a candidate instead.
			name: "an unpadded day falls back to the sentinel",
			ts:   "2026-10-5 13:34:02",
			want: "ccvymx",
		},
		{
			name: "text that is not a date at all falls back to the sentinel",
			ts:   "sometime last tuesday",
			want: "ccvymx",
		},
		{
			name: "an empty timestamp falls back to the sentinel",
			ts:   "",
			want: "ccvymx",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Written with raw SQL rather than InsertTurns, because the point
			// is what the column derives from text the Go layer could not
			// produce.
			if _, err := database.Exec(
				`INSERT OR REPLACE INTO turns (id, session_id, type, timestamp, content, ordinal)
				 VALUES ('period-probe', NULL, 'user', ?, 'probe', 0)`, tc.ts); err != nil {
				t.Fatalf("insert probe turn: %v", err)
			}

			var got string
			if err := database.QueryRow(
				"SELECT search_period FROM turns WHERE id = 'period-probe'").Scan(&got); err != nil {
				t.Fatalf("read search_period: %v", err)
			}
			if got != tc.want {
				t.Errorf("search_period for %q = %q, want %q", tc.ts, got, tc.want)
			}
		})
	}
}

// TestSearchPeriod_IndexesCarryThePeriodColumn checks that both indexes can be
// matched on the period column, which is the whole point of the migration. A
// test that only searched for content would pass against the old two-column
// index.
func TestSearchPeriod_IndexesCarryThePeriodColumn(t *testing.T) {
	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = database.Close() }()

	seedTurnSession(t, database, "session-period")
	seedPeriodTurn(t, database, "session-period", "turn-sep",
		time.Date(2026, 9, 30, 23, 59, 59, 0, time.UTC), 0)
	seedPeriodTurn(t, database, "session-period", "turn-oct",
		time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), 1)

	cases := []struct {
		table   string
		match   string
		wantIDs []string
	}{
		{"turns_fts", "{search_period} : (ccvd20260930)", []string{"turn-sep"}},
		{"turns_fts", "{search_period} : (ccvym202610)", []string{"turn-oct"}},
		{"turns_fts", "{search_period} : (ccvd20260930 OR ccvd20261001)", []string{"turn-oct", "turn-sep"}},
	}

	for _, tc := range cases {
		ids := turnIDsMatching(t, database, tc.table, tc.match)
		if fmt.Sprint(ids) != fmt.Sprint(tc.wantIDs) {
			t.Errorf("%s MATCH %q returned %v, want %v", tc.table, tc.match, ids, tc.wantIDs)
		}
	}

	// The tool payload index carries it too, keyed on the tool use's own
	// timestamp — which is its turn's, stamped on by adapter.ToolUseFromParsed.
	var n int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM tool_uses_fts WHERE tool_uses_fts MATCH '{search_period} : (ccvym202610)'`,
	).Scan(&n); err != nil {
		t.Fatalf("match period on tool_uses_fts: %v", err)
	}
	if n != 1 {
		t.Errorf("tool_uses_fts holds %d October documents, want 1", n)
	}
}

// TestSearchPeriod_ContentDoesNotReachThePeriodColumn is the collision
// guarantee. A turn whose text holds a period token must be findable by
// searching for that text, and must not answer a period filter for a month it
// does not belong to.
func TestSearchPeriod_ContentDoesNotReachThePeriodColumn(t *testing.T) {
	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = database.Close() }()

	seedTurnSession(t, database, "session-collide")
	if err := database.InsertTurns([]models.Turn{{
		ID: "turn-collide", SessionID: "session-collide", Type: "user",
		Timestamp: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
		Content:   "the token for last month is ccvym202610",
	}}); err != nil {
		t.Fatalf("insert turn: %v", err)
	}

	if n := ftsMatchCount(t, database, "{search_period} : (ccvym202610)"); n != 0 {
		t.Errorf("a March turn answered an October period filter %d times — content leaked into the period column", n)
	}
	if n := ftsMatchCount(t, database, "{content} : (ccvym202610)"); n != 1 {
		t.Errorf("searching the literal token found %d turns, want the 1 whose content holds it", n)
	}
}

// TestSearchPeriod_IndexesAreIntegral runs FTS5's strict integrity-check over
// both rebuilt indexes and compares the document counts against the rows
// behind them. The migration drops and recreates both indexes, which is the
// operation most likely to leave an index that answers queries while holding
// the wrong documents.
func TestSearchPeriod_IndexesAreIntegral(t *testing.T) {
	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = database.Close() }()

	seedTurnSession(t, database, "session-integral")
	for i := 0; i < 5; i++ {
		seedPeriodTurn(t, database, "session-integral", fmt.Sprintf("turn-%d", i),
			time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i), i)
	}
	// A re-sync replaces a session's rows. Doing it here means the integrity
	// check below covers what the triggers maintain, not only what the
	// migration's rebuild produced.
	seedPeriodTurn(t, database, "session-integral", "turn-2",
		time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC), 2)

	for _, table := range []string{"turns_fts", "tool_uses_fts"} {
		if err := ftsIntegrityCheck(t, database, table); err != nil {
			t.Errorf("%s integrity-check: %v", table, err)
		}
	}

	var turns, toolUses int64
	if err := database.QueryRow("SELECT (SELECT COUNT(*) FROM turns), (SELECT COUNT(*) FROM tool_uses)").
		Scan(&turns, &toolUses); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if got := ftsDocsizeCount(t, database, "turns_fts"); got != turns {
		t.Errorf("turns_fts holds %d documents for %d turns", got, turns)
	}
	if got := ftsDocsizeCount(t, database, "tool_uses_fts"); got != toolUses {
		t.Errorf("tool_uses_fts holds %d documents for %d tool uses", got, toolUses)
	}
}

// TestSearchPeriod_RebuildRepairsOrphans covers the side effect the migration
// has on an archive that already drifted — issue #81, where the author's
// archive carried 29,268 turns_fts entries with no turns row behind them, from
// before the recursive_triggers dependency was understood. The migration's
// rebuild discards the index and derives it from the content table, so those
// entries go.
func TestSearchPeriod_RebuildRepairsOrphans(t *testing.T) {
	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = database.Close() }()

	seedTurnSession(t, database, "session-orphan")
	seedPeriodTurn(t, database, "session-orphan", "turn-live",
		time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), 0)

	// Forge an orphan the way the pre-recursive_triggers era produced them: an
	// index entry for a rowid the turns table does not have.
	if _, err := database.Exec(
		`INSERT INTO turns_fts(rowid, content, search_period) VALUES (99999, 'ghostcontent', 'ccvym202610')`,
	); err != nil {
		t.Fatalf("forge orphan: %v", err)
	}
	if n := ftsMatchCount(t, database, "{content} : (ghostcontent)"); n != 1 {
		t.Fatalf("forged orphan is not in the index (%d hits), so the repair below would prove nothing", n)
	}

	if _, err := database.Exec("INSERT INTO turns_fts(turns_fts) VALUES('rebuild')"); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	if n := ftsMatchCount(t, database, "{content} : (ghostcontent)"); n != 0 {
		t.Errorf("rebuild left %d orphaned entries", n)
	}
	integrity, err := database.CheckFTSIntegrity()
	if err != nil {
		t.Fatalf("integrity report: %v", err)
	}
	if !integrity.Consistent() {
		t.Errorf("after rebuild: %+v, want consistent", integrity)
	}
	if err := ftsIntegrityCheck(t, database, "turns_fts"); err != nil {
		t.Errorf("turns_fts integrity-check after rebuild: %v", err)
	}
}

// TestSearchPeriod_TriggerFollowsATimestampChange covers the trigger scope.
// The period token derives from the timestamp, so an UPDATE that moves a turn
// in time has to re-index it. The old trigger fired on content alone, which
// was right when content was the only indexed material and would now leave the
// index claiming the wrong month.
func TestSearchPeriod_TriggerFollowsATimestampChange(t *testing.T) {
	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = database.Close() }()

	seedTurnSession(t, database, "session-move")
	seedPeriodTurn(t, database, "session-move", "turn-move",
		time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC), 0)

	if _, err := database.Exec(
		`UPDATE turns SET timestamp = '2026-10-20 08:00:00.000 +0000 UTC' WHERE id = 'turn-move'`); err != nil {
		t.Fatalf("move the turn in time: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE tool_uses SET timestamp = '2026-10-20 08:00:00.000 +0000 UTC' WHERE turn_id = 'turn-move'`); err != nil {
		t.Fatalf("move the tool use in time: %v", err)
	}

	if n := ftsMatchCount(t, database, "{search_period} : (ccvym202609)"); n != 0 {
		t.Errorf("turns_fts still answers the old month %d times", n)
	}
	if n := ftsMatchCount(t, database, "{search_period} : (ccvym202610)"); n != 1 {
		t.Errorf("turns_fts answers the new month %d times, want 1", n)
	}
	for _, table := range []string{"turns_fts", "tool_uses_fts"} {
		if err := ftsIntegrityCheck(t, database, table); err != nil {
			t.Errorf("%s integrity-check after the move: %v", table, err)
		}
	}
}
