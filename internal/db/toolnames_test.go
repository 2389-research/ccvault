// ABOUTME: Tests the tool-name readers — the page-bounded one must read only the turns it was asked about (#141, #157).
// ABOUTME: Boundedness is asserted by which rows a query returns, never by how long it takes.

package db

import (
	"fmt"
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
