// ABOUTME: Tests that get_turns reads one page of turns rather than the whole session (#169), and that paging still returns every turn exactly once.
// ABOUTME: Red first: the handler called db.GetTurns and sliced in Go, so a page of the archive's largest session cost 56,030 rows.

package mcp

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/2389-research/ccvault/internal/db"
	"github.com/2389-research/ccvault/pkg/models"
)

// seedAlternatingTurns writes n turns of a session at ordinals 0..n-1,
// alternating user and assistant, each with content naming its ordinal.
func seedAlternatingTurns(t *testing.T, database *db.DB, sessionID string, n int) {
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
			RawJSON:   []byte(`{"type":"` + kind + `"}`),
		}
	}
	if err := database.InsertTurns(turns); err != nil {
		t.Fatalf("insert %d turns: %v", n, err)
	}
}

// shadowTurnsOutsideTheFirstPage makes every turn past `lastOrdinal`
// unreadable: its input_tokens comes back NULL, which fails the scan in any
// reader that returns the row. A handler whose read is bounded by the page it
// is rendering never returns one; one that reads the session fails outright.
//
// Same mechanism and the same reasoning as shadowToolUsesOutsideThePage above
// (#141): a TEMP view shadows a main-schema object of the same name for
// unqualified references, leaving the real table, its indexes and the FTS
// content table behind it alone.
//
// input_tokens rather than a column the page query filters or orders on —
// poisoning type or ordinal would change which rows the page is — and not
// timestamp, because a view column defined by a CASE carries no declared type
// and go-sqlite3 decides whether to return a time.Time from that type, which
// would break the on-page rows too.
func shadowTurnsOutsideTheFirstPage(t *testing.T, database *db.DB, lastOrdinal int) {
	t.Helper()

	database.SetMaxOpenConns(1)

	columns := turnsSelectListMCP(t, database, lastOrdinal)

	view := "CREATE TEMP VIEW turns AS SELECT " + strings.Join(columns, ", ") + " FROM main.turns"
	if _, err := database.Exec(view); err != nil {
		t.Fatalf("shadow turns: %v", err)
	}

	var poisoned int
	if err := database.QueryRow(`SELECT count(*) FROM turns WHERE input_tokens IS NULL`).Scan(&poisoned); err != nil {
		t.Fatalf("probe the shadow: %v", err)
	}
	if poisoned == 0 {
		t.Fatal("the TEMP view did not shadow turns, so nothing here would detect a whole-session read")
	}
}

// turnsSelectListMCP reads turns' columns back from the schema, hiding
// input_tokens past lastOrdinal. Its own function so the cursor is closed
// before the caller's next Exec: the pool is one connection, and a statement
// left open across an Exec would wait for itself.
func turnsSelectListMCP(t *testing.T, database *db.DB, lastOrdinal int) []string {
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
		// Generated columns (search_period) cannot be declared on a view.
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

// TestGetTurns_TurnReadIsBoundedByThePage covers issue #169: get_turns loaded
// every turn of the session to return one page, so the cost of a page followed
// the session rather than the page — up to 56,030 rows, or 56.8 MB of content
// and raw_json, on the author's archive.
//
// Every turn past the page is unreadable here, so the handler completing at
// all is the proof that it stayed inside the page.
//
// The bound is the page's ten turns plus one: the handler asks for limit+1
// rows and renders limit of them, and the presence of that extra row is how
// has_more is answered without a second query. So ordinal 10 is readable and
// ordinals 11 and up are not — eleven rows of a sixty-turn session.
func TestGetTurns_TurnReadIsBoundedByThePage(t *testing.T) {
	s, database := newTestServer(t)
	p := seedProject(t, database, "/test/proj")
	seedSession(t, database, "session-1", p.ID)
	seedAlternatingTurns(t, database, "session-1", 60)
	shadowTurnsOutsideTheFirstPage(t, database, 10)

	result, err := s.getTurns(map[string]interface{}{
		"session_id": "session-1",
		"limit":      float64(10),
	})
	if err != nil {
		t.Fatalf("get_turns read turns outside the page it was rendering (#169): %v", err)
	}

	m := resultMap(t, result)
	turns := mustField[[]map[string]interface{}](t, m, "turns")
	if len(turns) != 10 {
		t.Fatalf("got %d turns, want 10", len(turns))
	}

	// total_turns still names the whole session even though only the page was
	// read, because that is what a caller walking it needs.
	if total, _ := m["total_turns"].(int); total != 60 {
		t.Errorf("total_turns is %d, want 60", total)
	}
	if _, more := m["has_more"]; !more {
		t.Error("a 10-turn page of a 60-turn session reported no has_more")
	}
}

// TestGetTurns_ResumeReadIsBoundedByThePage holds the same bound for the
// resumable path #79 added, which is where the defect cost the most: each
// after_ordinal resume re-read the whole session to hand back the next page.
//
// Here only the fourth page's turns and its one has_more lookahead row are
// readable, so a resume that re-reads the session fails while one that seeks
// to the ordinal succeeds.
func TestGetTurns_ResumeReadIsBoundedByThePage(t *testing.T) {
	s, database := newTestServer(t)
	p := seedProject(t, database, "/test/proj")
	seedSession(t, database, "session-1", p.ID)
	seedAlternatingTurns(t, database, "session-1", 60)
	shadowTurnsOnlyWithin(t, database, 30, 40)

	result, err := s.getTurns(map[string]interface{}{
		"session_id":    "session-1",
		"after_ordinal": float64(29),
		"limit":         float64(10),
	})
	if err != nil {
		t.Fatalf("resuming from an ordinal re-read the whole session (#169): %v", err)
	}

	m := resultMap(t, result)
	turns := mustField[[]map[string]interface{}](t, m, "turns")
	if len(turns) != 10 {
		t.Fatalf("got %d turns, want 10", len(turns))
	}
	if ordinal, _ := turns[0]["ordinal"].(int); ordinal != 30 {
		t.Errorf("the page starts at ordinal %d, want 30", ordinal)
	}
	if next, _ := m["next_after_ordinal"].(int); next != 39 {
		t.Errorf("next_after_ordinal is %d, want 39", next)
	}
}

// shadowTurnsOnlyWithin is shadowTurnsOutsideTheFirstPage for a page in the
// middle of a session: every turn outside [first, last] is unreadable.
func shadowTurnsOnlyWithin(t *testing.T, database *db.DB, first, last int) {
	t.Helper()

	database.SetMaxOpenConns(1)

	columns := turnsSelectListMCP(t, database, -1)
	for i, c := range columns {
		if strings.HasSuffix(c, "AS input_tokens") {
			columns[i] = fmt.Sprintf(
				"CASE WHEN ordinal BETWEEN %d AND %d THEN input_tokens END AS input_tokens", first, last)
		}
	}

	view := "CREATE TEMP VIEW turns AS SELECT " + strings.Join(columns, ", ") + " FROM main.turns"
	if _, err := database.Exec(view); err != nil {
		t.Fatalf("shadow turns: %v", err)
	}

	var poisoned int
	if err := database.QueryRow(`SELECT count(*) FROM turns WHERE input_tokens IS NULL`).Scan(&poisoned); err != nil {
		t.Fatalf("probe the shadow: %v", err)
	}
	if poisoned == 0 {
		t.Fatal("the TEMP view did not shadow turns, so nothing here would detect a whole-session read")
	}
}

// TestGetTurns_PagingCoversEveryTurnExactlyOnce is the correctness half of
// #169: a page read in SQL has to return the same sequence the Go slicing did.
// A LIMIT/OFFSET off by one, or an ordinal bound that is inclusive where the
// old filter was exclusive, shows up here as a skipped or repeated turn.
func TestGetTurns_PagingCoversEveryTurnExactlyOnce(t *testing.T) {
	s, database := newTestServer(t)
	p := seedProject(t, database, "/test/proj")
	seedSession(t, database, "session-1", p.ID)
	seedAlternatingTurns(t, database, "session-1", 23)

	for _, typeFilter := range []string{"", "user", "assistant"} {
		t.Run("type="+typeFilter, func(t *testing.T) {
			// Both ways of walking a session, because they page differently:
			// offset counts rows of the result, after_ordinal names a turn.
			byOffset := walkWithOffset(t, s, typeFilter, 4)
			byOrdinal := walkWithOrdinal(t, s, typeFilter, 4)

			want := expectedOrdinals(23, typeFilter)
			if fmt.Sprint(byOffset) != fmt.Sprint(want) {
				t.Errorf("walking by offset gave %v, want %v", byOffset, want)
			}
			if fmt.Sprint(byOrdinal) != fmt.Sprint(want) {
				t.Errorf("walking by after_ordinal gave %v, want %v", byOrdinal, want)
			}
		})
	}
}

func expectedOrdinals(n int, typeFilter string) []int {
	var want []int
	for i := 0; i < n; i++ {
		kind := "user"
		if i%2 == 1 {
			kind = "assistant"
		}
		if typeFilter == "" || typeFilter == kind {
			want = append(want, i)
		}
	}
	return want
}

func walkWithOffset(t *testing.T, s *Server, typeFilter string, limit int) []int {
	t.Helper()

	var seen []int
	offset := 0
	for range 100 {
		args := map[string]interface{}{
			"session_id": "session-1",
			"limit":      float64(limit),
			"offset":     float64(offset),
		}
		if typeFilter != "" {
			args["type"] = typeFilter
		}
		result, err := s.getTurns(args)
		if err != nil {
			t.Fatalf("offset %d: %v", offset, err)
		}
		m := resultMap(t, result)
		turns := mustField[[]map[string]interface{}](t, m, "turns")
		for _, turn := range turns {
			ordinal, _ := turn["ordinal"].(int)
			seen = append(seen, ordinal)
		}
		next, more := m["next_offset"].(int)
		if !more {
			return seen
		}
		offset = next
	}
	t.Fatal("walking by offset did not terminate")
	return nil
}

func walkWithOrdinal(t *testing.T, s *Server, typeFilter string, limit int) []int {
	t.Helper()

	var seen []int
	after := -1
	for range 100 {
		args := map[string]interface{}{
			"session_id":    "session-1",
			"limit":         float64(limit),
			"after_ordinal": float64(after),
		}
		if typeFilter != "" {
			args["type"] = typeFilter
		}
		result, err := s.getTurns(args)
		if err != nil {
			t.Fatalf("after_ordinal %d: %v", after, err)
		}
		m := resultMap(t, result)
		turns := mustField[[]map[string]interface{}](t, m, "turns")
		for _, turn := range turns {
			ordinal, _ := turn["ordinal"].(int)
			seen = append(seen, ordinal)
		}
		if _, more := m["has_more"]; !more {
			return seen
		}
		next, ok := m["next_after_ordinal"].(int)
		if !ok {
			t.Fatal("a page reporting has_more reported no next_after_ordinal")
		}
		after = next
	}
	t.Fatal("walking by after_ordinal did not terminate")
	return nil
}
