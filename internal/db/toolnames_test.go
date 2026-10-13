// ABOUTME: Tests the tool-name readers — the page-bounded one must read only the turns it was asked about (#141, #157).
// ABOUTME: Boundedness is asserted by which rows a query returns, never by how long it takes.

package db

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/2389-research/ccvault/pkg/models"
)

// seedToolCallTurns appends n assistant turns to a session, each recording
// callsPerTurn tool calls in tool_uses under a name of its own and naming none
// of them in raw_json. That is the shape #100 was about: codex, hex and jeff
// record a turn's calls on transcript lines of their own, so the stored rows
// are the only place the names exist. Returns the turn ids in session order.
//
// No call is marked is_error, which keeps the failed-call readers out of the
// way: they read the same table and would otherwise have to be reasoned about
// alongside the read under test.
func seedToolCallTurns(t *testing.T, database *DB, sessionID string, n, callsPerTurn int) []string {
	t.Helper()

	p := &models.Project{Path: "/test/proj", DisplayName: "proj"}
	if err := database.UpsertProject(p); err != nil {
		t.Fatalf("upsert project: %v", err)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := database.UpsertSession(&models.Session{
		ID:         sessionID,
		ProjectID:  p.ID,
		StartedAt:  base,
		SourceFile: "/fake/" + sessionID + ".jsonl",
	}); err != nil {
		t.Fatalf("upsert session: %v", err)
	}

	turns := make([]models.Turn, n)
	uses := make([]models.ToolUse, 0, n*callsPerTurn)
	ids := make([]string, n)
	for i := range turns {
		id := fmt.Sprintf("%s-assistant-%d", sessionID, i)
		ids[i] = id
		turns[i] = models.Turn{
			ID:        id,
			SessionID: sessionID,
			Type:      "assistant",
			Timestamp: base.Add(time.Duration(i) * time.Second),
			Ordinal:   i + 1,
			Content:   fmt.Sprintf("assistant turn %d", i),
			// Deliberately no `message` object: raw_json names no tools, so a
			// turn's tools can only come from the stored rows.
			RawJSON: []byte(`{"type":"assistant"}`),
		}
		for c := 0; c < callsPerTurn; c++ {
			uses = append(uses, models.ToolUse{
				TurnID:    id,
				SessionID: sessionID,
				ToolName:  fmt.Sprintf("tool_%d_%d", i, c),
				Timestamp: turns[i].Timestamp,
			})
		}
	}

	if err := database.InsertTurns(turns); err != nil {
		t.Fatalf("insert %d assistant turns: %v", n, err)
	}
	if err := database.InsertToolUses(uses); err != nil {
		t.Fatalf("insert %d tool uses: %v", len(uses), err)
	}
	return ids
}

// TestToolNamesForTurns_ReadsOnlyTheTurnsAsked is the row count behind the
// handler test in internal/mcp. Every row the query returns appends exactly
// one name, so the names in the result are the rows it read — a page of two
// turns with three calls each must come back with six, not the session's
// eighteen.
func TestToolNamesForTurns_ReadsOnlyTheTurnsAsked(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	ids := seedToolCallTurns(t, database, "session-1", 6, 3)
	page := ids[:2]
	onPage := map[string]bool{page[0]: true, page[1]: true}

	byTurn, err := database.ToolNamesForTurns("session-1", page)
	if err != nil {
		t.Fatalf("ToolNamesForTurns: %v", err)
	}

	read := 0
	for turnID, names := range byTurn {
		if !onPage[turnID] {
			t.Errorf("read %d tool calls for turn %s, which is not on the page", len(names), turnID)
		}
		read += len(names)
	}
	if want := len(page) * 3; read != want {
		t.Errorf("read %d tool_uses rows to label a %d-turn page, want %d (#141)", read, len(page), want)
	}
}

// TestToolNamesForTurns_NoTurnsReadsNothing pins the empty case. A page with
// no assistant turns has nothing to label, and `turn_id IN ()` is not valid
// SQL — so the query must not be built at all.
func TestToolNamesForTurns_NoTurnsReadsNothing(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	seedToolCallTurns(t, database, "session-1", 2, 2)

	byTurn, err := database.ToolNamesForTurns("session-1", nil)
	if err != nil {
		t.Fatalf("ToolNamesForTurns with no turns: %v", err)
	}
	if len(byTurn) != 0 {
		t.Errorf("an empty page read %d turns' tool calls", len(byTurn))
	}
}

// TestToolNamesForTurns_IssueOrderWithinATurn pins the ordering its callers
// render: a turn's names come back in the order the calls were issued, which
// the page-bounded query gets from idx_tool_uses_turn_ordinal rather than from
// a sort.
func TestToolNamesForTurns_IssueOrderWithinATurn(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	ids := seedToolCallTurns(t, database, "session-1", 1, 4)

	byTurn, err := database.ToolNamesForTurns("session-1", ids)
	if err != nil {
		t.Fatalf("ToolNamesForTurns: %v", err)
	}

	got := byTurn[ids[0]]
	want := []string{"tool_0_0", "tool_0_1", "tool_0_2", "tool_0_3"}
	if len(got) != len(want) {
		t.Fatalf("turn labelled %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("turn labelled %v, want %v — the issue order is gone", got, want)
		}
	}
}

// TestSessionToolNamesByTurn_PlanSortsNothing covers the half of #157 that
// #141 got wrong about its own query. The lookup is an index seek either way;
// what the session-wide form also paid for was a temp B-tree over every row
// it found, because idx_tool_uses_session carries no ordering. Its one caller
// — get_session_summary — only counts the names, so the sort bought nothing.
//
// "There is an index" is therefore not grounds to call a read cheap, and this
// test is the standing check that the plan is looked at for a sort and not
// just for a seek.
func TestSessionToolNamesByTurn_PlanSortsNothing(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	seedToolCallTurns(t, database, "session-1", 4, 2)

	plan := toolUsesQueryPlan(t, database, sessionToolNamesSQL, "session-1")
	if !strings.Contains(plan, "idx_tool_uses_session") {
		t.Errorf("session-wide tool-name read no longer seeks idx_tool_uses_session:\n%s", plan)
	}
	if strings.Contains(plan, "TEMP B-TREE") {
		t.Errorf("session-wide tool-name read sorts every row of the session in a temp B-tree "+
			"for an ORDER BY its only caller does not use (#157):\n%s", plan)
	}
}

// TestSessionToolNamesByTurn_ReadsEveryCall is the other side of dropping that
// ORDER BY: the summary counts these names, so losing rows would undercount
// tools_used silently. Unordered is fine; short is not.
func TestSessionToolNamesByTurn_ReadsEveryCall(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	ids := seedToolCallTurns(t, database, "session-1", 5, 3)

	byTurn, err := database.SessionToolNamesByTurn("session-1")
	if err != nil {
		t.Fatalf("SessionToolNamesByTurn: %v", err)
	}
	if len(byTurn) != len(ids) {
		t.Errorf("read %d turns' calls, want %d", len(byTurn), len(ids))
	}
	for i, id := range ids {
		got := byTurn[id]
		if len(got) != 3 {
			t.Errorf("turn %s has %d names, want 3: %v", id, len(got), got)
			continue
		}
		// Order within a turn is not promised here, so the assertion is on the
		// set — which is what counting needs.
		want := map[string]bool{
			fmt.Sprintf("tool_%d_0", i): true,
			fmt.Sprintf("tool_%d_1", i): true,
			fmt.Sprintf("tool_%d_2", i): true,
		}
		for _, name := range got {
			if !want[name] {
				t.Errorf("turn %s names %s, which it never called", id, name)
			}
			delete(want, name)
		}
		if len(want) > 0 {
			t.Errorf("turn %s is missing %v", id, want)
		}
	}
}

// TestToolNamesForTurns_PlanSeeksTheOrdinalIndexAndSortsNothing pins the claim
// #154 made for the page-bounded form: naming the turns moves the query onto
// idx_tool_uses_turn_ordinal, which leads on turn_id and already holds a
// turn's calls in issue order — so the ORDER BY this one does need costs no
// sort at all.
func TestToolNamesForTurns_PlanSeeksTheOrdinalIndexAndSortsNothing(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	ids := seedToolCallTurns(t, database, "session-1", 6, 3)
	page := ids[:2]

	plan := toolUsesQueryPlan(t, database, toolNamesForTurnsSQL(len(page)),
		turnIDArgs("session-1", page)...)
	if !strings.Contains(plan, toolUsesTurnOrdinalIndex) {
		t.Errorf("page-bounded tool-name read does not seek %s:\n%s", toolUsesTurnOrdinalIndex, plan)
	}
	if strings.Contains(plan, "TEMP B-TREE") {
		t.Errorf("page-bounded tool-name read sorts in a temp B-tree, so the index is not "+
			"carrying the ORDER BY any more (#141):\n%s", plan)
	}
}

// toolUsesQueryPlan returns EXPLAIN QUERY PLAN's detail column for a query,
// one line per step.
func toolUsesQueryPlan(t *testing.T, database *DB, query string, args ...interface{}) string {
	t.Helper()

	rows, err := database.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var plan strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatalf("scan plan row: %v", err)
		}
		plan.WriteString(detail)
		plan.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate plan: %v", err)
	}
	return plan.String()
}
