// ABOUTME: Tests the reader that hands a view a session's tool calls grouped by the turn that issued them (#179).
// ABOUTME: Covers the within-turn order, the three states of is_error, and that an orphan call is left out.

package db

import (
	"testing"
	"time"

	"github.com/2389-research/ccvault/pkg/models"
)

// seedCallsForRendering builds one assistant turn that made three calls — one
// that failed, one that did not, one nothing is known about — plus a second
// turn that made one, so the grouping has something to get wrong.
func seedCallsForRendering(t *testing.T, database *DB) {
	t.Helper()

	ts := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if err := database.UpsertSession(&models.Session{
		ID: "sess-render", StartedAt: ts, SourceFile: "/tmp/sess-render.jsonl", Source: "codex",
	}); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	if err := database.InsertTurns([]models.Turn{
		{ID: "turn-a", SessionID: "sess-render", Type: "assistant", Timestamp: ts, Ordinal: 0, Content: "first"},
		{ID: "turn-b", SessionID: "sess-render", Type: "assistant", Timestamp: ts, Ordinal: 1, Content: "second"},
	}); err != nil {
		t.Fatalf("insert turns: %v", err)
	}

	yes, no := true, false
	if err := database.InsertToolUses([]models.ToolUse{
		// turn_ordinal is assigned by insertion order, so these three are
		// calls 0, 1 and 2 of turn-a.
		{TurnID: "turn-a", SessionID: "sess-render", ToolName: "first_call", Timestamp: ts,
			InputJSON: `{"n":0}`, HasResult: true, ResultContent: "ok", ResultLength: 2, IsError: &no},
		{TurnID: "turn-a", SessionID: "sess-render", ToolName: "second_call", Timestamp: ts,
			InputJSON: `{"n":1}`, HasResult: true, ResultContent: "boom", ResultLength: 4, IsError: &yes},
		{TurnID: "turn-a", SessionID: "sess-render", ToolName: "third_call", Timestamp: ts,
			InputJSON: `{"n":2}`},
		{TurnID: "turn-b", SessionID: "sess-render", ToolName: "other_turns_call", Timestamp: ts,
			InputJSON: `{"n":0}`},
	}); err != nil {
		t.Fatalf("insert tool uses: %v", err)
	}
}

// TestSessionToolCallsByTurn_GroupsAndOrders is what the conversation view
// needs: each turn's own calls, in the order it made them.
func TestSessionToolCallsByTurn_GroupsAndOrders(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()
	seedCallsForRendering(t, database)

	byTurn, err := database.SessionToolCallsByTurn("sess-render")
	if err != nil {
		t.Fatalf("read calls: %v", err)
	}

	if len(byTurn) != 2 {
		t.Fatalf("grouped into %d turns, want 2: %v", len(byTurn), byTurn)
	}
	want := []string{"first_call", "second_call", "third_call"}
	got := byTurn["turn-a"]
	if len(got) != len(want) {
		t.Fatalf("turn-a has %d calls, want %d: %v", len(got), len(want), got)
	}
	for i, name := range want {
		if got[i].ToolName != name {
			t.Errorf("call %d of turn-a is %q, want %q", i, got[i].ToolName, name)
		}
	}
	if n := len(byTurn["turn-b"]); n != 1 {
		t.Errorf("turn-b has %d calls, want 1", n)
	}
}

// TestSessionToolCallsByTurn_CarriesArgumentsAndFailure pins the two fields a
// view renders beyond the name. Failure is the stored flag, not a guess from
// the result text.
func TestSessionToolCallsByTurn_CarriesArgumentsAndFailure(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()
	seedCallsForRendering(t, database)

	byTurn, err := database.SessionToolCallsByTurn("sess-render")
	if err != nil {
		t.Fatalf("read calls: %v", err)
	}
	calls := byTurn["turn-a"]
	if len(calls) != 3 {
		t.Fatalf("turn-a has %d calls, want 3", len(calls))
	}

	if calls[0].InputJSON != `{"n":0}` {
		t.Errorf("arguments of call 0 are %q, want %q", calls[0].InputJSON, `{"n":0}`)
	}

	// is_error's three states collapse to two here, because a view marks or
	// does not mark. Only `true` marks: an unanswered call did not fail, and
	// marking it sends a reader looking for an error nobody reported.
	if calls[0].Failed {
		t.Error("a call whose result reported no failure is marked failed")
	}
	if !calls[1].Failed {
		t.Error("a call whose result reported a failure is not marked failed")
	}
	if calls[2].Failed {
		t.Error("a call nothing is known about is marked failed")
	}
}

// TestSessionToolCallsByTurn_SkipsCallsWithNoTurn keeps a call that names no
// turn out of the grouping. There is no turn to render it under, and a NULL
// turn_id would otherwise collect every such call of a session into one
// phantom group.
func TestSessionToolCallsByTurn_SkipsCallsWithNoTurn(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()
	seedCallsForRendering(t, database)
	if _, err := database.Exec(
		`INSERT INTO tool_uses (turn_id, session_id, tool_name, timestamp, turn_ordinal)
		 VALUES (NULL, 'sess-render', 'orphan_call', '2026-10-01 12:00:00', 0)`); err != nil {
		t.Fatalf("insert orphan call: %v", err)
	}

	byTurn, err := database.SessionToolCallsByTurn("sess-render")
	if err != nil {
		t.Fatalf("read calls: %v", err)
	}
	for turnID, calls := range byTurn {
		for _, c := range calls {
			if c.ToolName == "orphan_call" {
				t.Errorf("a call with no turn_id was grouped under %q", turnID)
			}
		}
	}
}

// TestSessionToolCallsByTurn_EmptyForASessionThatCalledNothing returns an empty
// map rather than an error, the same convention GetFailedToolCalls holds: the
// caller branches on length, and an error for an uneventful session would make
// it indistinguishable from a failed read.
func TestSessionToolCallsByTurn_EmptyForASessionThatCalledNothing(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	byTurn, err := database.SessionToolCallsByTurn("sess-nothing")
	if err != nil {
		t.Fatalf("read calls for a session with none: %v", err)
	}
	if len(byTurn) != 0 {
		t.Errorf("a session that called nothing grouped into %d turns", len(byTurn))
	}
}
