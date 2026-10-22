// ABOUTME: Tests the paged turn reader (#169) — that a page reads the page, and that the query plan carries no sort.
// ABOUTME: Red first: GetTurnsPage and CountTurns did not exist, so MCP's get_turns read every turn of the session to return fifty.

package db

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/2389-research/ccvault/pkg/models"
)

// seedSessionTurns writes n turns of a session, alternating user and
// assistant, at ordinals 0..n-1. The content is per-turn so a test can tell
// which rows came back.
func seedSessionTurns(t *testing.T, database *DB, sessionID string, n int) []models.Turn {
	t.Helper()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	turns := make([]models.Turn, n)
	for i := range turns {
		kind := "user"
		if i%2 == 1 {
			kind = "assistant"
		}
		turns[i] = models.Turn{
			ID:        fmt.Sprintf("%s-turn-%d", sessionID, i),
			SessionID: sessionID,
			Type:      kind,
			Timestamp: base.Add(time.Duration(i) * time.Second),
			Ordinal:   i,
			Content:   fmt.Sprintf("turn %d", i),
		}
	}
	if err := database.InsertTurns(turns); err != nil {
		t.Fatalf("insert %d turns: %v", n, err)
	}
	return turns
}

// shadowTurnsBeyondOrdinal makes every turn past `lastOrdinal` unreadable: its
// input_tokens comes back NULL, which fails the scan in any reader that
// returns the row. A reader bounded by the page never returns one and never
// notices; a whole-session read fails outright.
//
// Same mechanism as internal/mcp's shadowToolUsesOutsideThePage (#141): a TEMP
// view shadows a main-schema object of the same name for unqualified
// references, so the real table, its indexes and the FTS content table behind
// it are all left alone.
//
// input_tokens rather than any column the queries filter or order on.
// Poisoning type would change which rows a type filter matches and poisoning
// ordinal would change the page itself — either would make the test pass for
// the wrong reason. It cannot be timestamp either, for a subtler reason: a
// view column defined by a CASE expression carries no declared type, and
// go-sqlite3 decides whether to hand back a time.Time from the declared type
// of the column — so poisoning it would break the scan of the on-page rows
// too. An integer scans the same either way.
func shadowTurnsBeyondOrdinal(t *testing.T, database *DB, lastOrdinal int) {
	t.Helper()

	// The TEMP view lives on one connection. Open already pins the pool to a
	// single connection; saying so here keeps the test from depending on that
	// silently.
	database.SetMaxOpenConns(1)

	columns := turnsSelectList(t, database, lastOrdinal)

	view := "CREATE TEMP VIEW turns AS SELECT " + strings.Join(columns, ", ") + " FROM main.turns"
	if _, err := database.Exec(view); err != nil {
		t.Fatalf("shadow turns: %v", err)
	}

	// Self-check: a shadow that did not take effect would make every
	// assertion below pass for the wrong reason.
	var poisoned int
	if err := database.QueryRow(`SELECT count(*) FROM turns WHERE input_tokens IS NULL`).Scan(&poisoned); err != nil {
		t.Fatalf("probe the shadow: %v", err)
	}
	if poisoned == 0 {
		t.Fatal("the TEMP view did not shadow turns, so nothing here would detect an off-page read")
	}
}

// turnsSelectList reads turns' columns back from the schema and returns them
// as a select list with input_tokens replaced by the expression that hides it
// past lastOrdinal. Reading the columns rather than listing them means adding
// one to turns cannot quietly break the shadow.
//
// Generated columns are dropped: search_period is computed from timestamp and
// a view cannot declare one, and nothing here reads it.
//
// Its own function so the cursor is closed before the caller runs anything
// else: the pool is a single connection, and a statement left open across an
// Exec would wait for itself.
func turnsSelectList(t *testing.T, database *DB, lastOrdinal int) []string {
	t.Helper()

	rows, err := database.Query(`SELECT name, hidden FROM pragma_table_xinfo('turns')`)
	if err != nil {
		t.Fatalf("read turns columns: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var columns []string
	for rows.Next() {
		var name string
		var hidden int
		if err := rows.Scan(&name, &hidden); err != nil {
			t.Fatalf("scan column name: %v", err)
		}
		// hidden 2 and 3 are FTS5-style hidden and generated columns; only a
		// plain column (0) belongs in the shadow.
		if hidden != 0 {
			continue
		}
		if name == "input_tokens" {
			columns = append(columns, fmt.Sprintf(
				"CASE WHEN ordinal <= %d THEN input_tokens END AS input_tokens", lastOrdinal))
			continue
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read column names: %v", err)
	}
	if len(columns) == 0 {
		t.Fatal("turns reported no columns")
	}
	return columns
}

// TestGetTurnsPage_ReadsOnlyThePage covers issue #169: the only reader of a
// session's turns loaded all of them, so a 50-turn page of the archive's
// largest session cost 56,030 rows, and its heaviest cost 56.8 MB of content
// and raw_json — per call, to return fifty rows.
//
// Every turn past the page is unreadable here, so the reader returning the
// page at all is the proof that it stayed inside it.
func TestGetTurnsPage_ReadsOnlyThePage(t *testing.T) {
	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = database.Close() }()

	seedSessionTurns(t, database, "session-1", 40)
	shadowTurnsBeyondOrdinal(t, database, 9)

	page, err := database.GetTurnsPage("session-1", TurnPage{Limit: 10})
	if err != nil {
		t.Fatalf("GetTurnsPage read turns outside the page it was asked for (#169): %v", err)
	}
	if len(page) != 10 {
		t.Fatalf("got %d turns, want 10", len(page))
	}
	for i, turn := range page {
		if turn.Ordinal != i {
			t.Errorf("page position %d holds ordinal %d, want %d", i, turn.Ordinal, i)
		}
	}
}

// TestCountTurns_DoesNotReadTheRows holds the other half of the bound: the
// total a page reports has to come from a count, not from materialising every
// row to take its length. The shadow makes the rows unreadable, so a count
// that scans them fails.
func TestCountTurns_DoesNotReadTheRows(t *testing.T) {
	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = database.Close() }()

	seedSessionTurns(t, database, "session-1", 40)
	shadowTurnsBeyondOrdinal(t, database, 9)

	total, err := database.CountTurns("session-1", "")
	if err != nil {
		t.Fatalf("CountTurns: %v", err)
	}
	if total != 40 {
		t.Errorf("CountTurns reported %d, want 40", total)
	}

	assistants, err := database.CountTurns("session-1", "assistant")
	if err != nil {
		t.Fatalf("CountTurns(assistant): %v", err)
	}
	if assistants != 20 {
		t.Errorf("CountTurns(assistant) reported %d, want 20", assistants)
	}
}

// TestGetTurnsPage_HonoursEveryBound checks the page the handler asks for is
// the page it gets, across the three ways a caller can name one.
func TestGetTurnsPage_HonoursEveryBound(t *testing.T) {
	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = database.Close() }()

	seedSessionTurns(t, database, "session-1", 20)

	cases := []struct {
		name string
		page TurnPage
		want []int
	}{
		{"first page", TurnPage{Limit: 3}, []int{0, 1, 2}},
		{"offset page", TurnPage{Offset: 5, Limit: 3}, []int{5, 6, 7}},
		{"offset past the end", TurnPage{Offset: 50, Limit: 3}, nil},
		{"resume from an ordinal", TurnPage{AfterOrdinal: 11, FromOrdinal: true, Limit: 3}, []int{12, 13, 14}},
		{"resume from before the start", TurnPage{AfterOrdinal: -1, FromOrdinal: true, Limit: 2}, []int{0, 1}},
		{"type filter", TurnPage{Type: "assistant", Limit: 3}, []int{1, 3, 5}},
		{"type filter and resume", TurnPage{Type: "user", AfterOrdinal: 5, FromOrdinal: true, Limit: 2}, []int{6, 8}},
		{"last page is short", TurnPage{Offset: 18, Limit: 5}, []int{18, 19}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := database.GetTurnsPage("session-1", tc.page)
			if err != nil {
				t.Fatalf("GetTurnsPage: %v", err)
			}
			ordinals := make([]int, len(got))
			for i, turn := range got {
				ordinals[i] = turn.Ordinal
			}
			if fmt.Sprint(ordinals) != fmt.Sprint(tc.want) {
				t.Errorf("got ordinals %v, want %v", ordinals, tc.want)
			}
		})
	}
}

// TestGetTurnsPage_PlanCarriesNoSort is the check #141 skipped and #157 paid
// for: a query can seek the right index and still sort what it found in a temp
// B-tree, which on #157's numbers was about half the cost. A page read is only
// bounded if SQLite can stop at the limit, and it cannot stop early if it has
// to sort the whole seek first.
//
// Asserted over the SQL the package actually runs rather than a copy of it, so
// a later edit to the query cannot leave this passing on stale text.
func TestGetTurnsPage_PlanCarriesNoSort(t *testing.T) {
	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = database.Close() }()

	seedSessionTurns(t, database, "session-1", 40)

	cases := []struct {
		name string
		page TurnPage
	}{
		{"plain page", TurnPage{Limit: 10}},
		{"offset page", TurnPage{Offset: 10, Limit: 10}},
		{"resume from an ordinal", TurnPage{AfterOrdinal: 9, FromOrdinal: true, Limit: 10}},
		{"type filter", TurnPage{Type: "assistant", Limit: 10}},
		{"type filter and resume", TurnPage{Type: "assistant", AfterOrdinal: 9, FromOrdinal: true, Limit: 10}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sql, args := turnsPageQuery("session-1", tc.page)
			plan := queryPlan(t, database, sql, args...)

			if strings.Contains(plan, "TEMP B-TREE") {
				t.Errorf("the page query sorts in a temp B-tree, so it cannot stop at the limit:\n%s", plan)
			}
			if !strings.Contains(plan, "idx_turns_session_ordinal") {
				t.Errorf("the page query does not seek the (session_id, ordinal) index:\n%s", plan)
			}
			if strings.Contains(plan, "SCAN turns") {
				t.Errorf("the page query scans turns:\n%s", plan)
			}
		})
	}
}

// TestCountTurns_PlanCarriesNoSort holds the same property for the count: a
// page's total_turns is read once per call and must not sort either.
func TestCountTurns_PlanCarriesNoSort(t *testing.T) {
	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = database.Close() }()

	seedSessionTurns(t, database, "session-1", 40)

	for _, typeFilter := range []string{"", "assistant"} {
		sql, args := turnCountQuery("session-1", typeFilter)
		plan := queryPlan(t, database, sql, args...)
		if strings.Contains(plan, "TEMP B-TREE") {
			t.Errorf("the count query for type %q sorts in a temp B-tree:\n%s", typeFilter, plan)
		}
		if strings.Contains(plan, "SCAN turns") {
			t.Errorf("the count query for type %q scans turns:\n%s", typeFilter, plan)
		}
	}
}

// queryPlan returns EXPLAIN QUERY PLAN's detail lines for one query, joined.
func queryPlan(t *testing.T, database *DB, query string, args ...interface{}) string {
	t.Helper()

	rows, err := database.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var plan strings.Builder
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan.WriteString(detail)
		plan.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read plan: %v", err)
	}
	return plan.String()
}
