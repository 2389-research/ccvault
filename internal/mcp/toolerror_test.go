// ABOUTME: Tests that the MCP surfaces report which tool calls failed, read from the stored is_error column (#83).
// ABOUTME: Real SQLite in a temp dir; the fixtures separate a failed call from a successful one and an unanswered one.

package mcp

import (
	"slices"
	"testing"
	"time"

	"github.com/2389-research/ccvault/pkg/models"
)

// seedFailedCalls gives a session two assistant turns: one that made a
// successful call and a failing one, and one whose single call nothing
// answered. The raw_json carries the tool_use blocks so tools_used and the
// enrichment path still work, which is what makes the failed-tool fields
// additive rather than a replacement.
func seedFailedCalls(t *testing.T, s *Server) {
	t.Helper()
	database := s.db

	p := seedProject(t, database, "/test/errors")
	seedSession(t, database, "session-e", p.ID)

	ts := time.Date(2026, 1, 1, 0, 0, 2, 0, time.UTC)
	turns := []models.Turn{
		{
			ID: "turn-mixed", SessionID: "session-e", Type: "assistant", Timestamp: ts, Ordinal: 1,
			Content: "[Tool: Bash]",
			RawJSON: []byte(`{"uuid":"turn-mixed","type":"assistant","message":{"role":"assistant","content":[` +
				`{"type":"tool_use","id":"toolu_OK","name":"Bash","input":{"command":"true"}},` +
				`{"type":"tool_use","id":"toolu_FAIL","name":"Edit","input":{"file_path":"/x"}}]}}`),
		},
		{
			ID: "turn-unanswered", SessionID: "session-e", Type: "assistant", Timestamp: ts.Add(time.Second), Ordinal: 2,
			Content: "[Tool: Bash]",
			RawJSON: []byte(`{"uuid":"turn-unanswered","type":"assistant","message":{"role":"assistant","content":[` +
				`{"type":"tool_use","id":"toolu_U","name":"Bash","input":{"command":"sleep 600"}}]}}`),
		},
	}
	if err := database.InsertTurns(turns); err != nil {
		t.Fatalf("insert turns: %v", err)
	}

	yes, no := true, false
	uses := []models.ToolUse{
		{TurnID: "turn-mixed", SessionID: "session-e", ToolName: "Bash", Timestamp: ts,
			ToolUseID: "toolu_OK", HasResult: true, ResultContent: "", ResultLength: 0, IsError: &no},
		{TurnID: "turn-mixed", SessionID: "session-e", ToolName: "Edit", Timestamp: ts,
			ToolUseID: "toolu_FAIL", HasResult: true,
			ResultContent: "String to replace not found in file", ResultLength: 34, IsError: &yes},
		{TurnID: "turn-unanswered", SessionID: "session-e", ToolName: "Bash", Timestamp: ts.Add(time.Second),
			ToolUseID: "toolu_U", HasResult: false},
	}
	if err := database.InsertToolUses(uses); err != nil {
		t.Fatalf("insert tool uses: %v", err)
	}
}

// TestGetTurns_ReportsFailedTools is the MCP half of issue #83. An agent
// paging a session has to be able to see which of its calls failed without
// fetching every result and reading it for words that look like an error.
func TestGetTurns_ReportsFailedTools(t *testing.T) {
	s, _ := newTestServer(t)
	seedFailedCalls(t, s)

	result, err := s.getTurns(map[string]interface{}{"session_id": "session-e", "limit": float64(50)})
	if err != nil {
		t.Fatalf("getTurns: %v", err)
	}
	resp := result.(map[string]interface{})
	turns := resp["turns"].([]map[string]interface{})

	byID := map[string]map[string]interface{}{}
	for _, turn := range turns {
		byID[turn["id"].(string)] = turn
	}

	mixed, ok := byID["turn-mixed"]
	if !ok {
		t.Fatalf("turn-mixed missing from the page: %+v", turns)
	}
	failed, ok := mixed["failed_tools"].([]string)
	if !ok {
		t.Fatalf("turn-mixed has no failed_tools: %+v", mixed)
	}
	if want := []string{"Edit"}; !slices.Equal(failed, want) {
		t.Errorf("failed_tools = %v, want %v — only the Edit call reported a failure", failed, want)
	}

	// The turn whose only call succeeded, and the one nothing answered, must
	// carry no failure field at all. An empty array present on every turn
	// would make "no failures" and "this turn reported none" the same shape.
	unanswered := byID["turn-unanswered"]
	if _, present := unanswered["failed_tools"]; present {
		t.Errorf("turn-unanswered carries failed_tools: %+v — nothing answered its call, so nothing failed", unanswered)
	}
}

// TestGetTurns_FailedToolsSurvivesUnreadableRawJSON is why the field reads the
// stored column rather than re-parsing raw_json. 216,978 turns in the author's
// archive hold raw_json that does not parse (#101); those turns already lose
// `tools` and say so with raw_unavailable, and a failure flag the backfill or
// the write path did manage to store must not be lost with them.
func TestGetTurns_FailedToolsSurvivesUnreadableRawJSON(t *testing.T) {
	s, database := newTestServer(t)
	p := seedProject(t, database, "/test/corrupt")
	seedSession(t, database, "session-c", p.ID)

	ts := time.Date(2026, 1, 1, 0, 0, 2, 0, time.UTC)
	if err := database.InsertTurns([]models.Turn{{
		ID: "turn-corrupt", SessionID: "session-c", Type: "assistant", Timestamp: ts, Ordinal: 1,
		Content: "[Tool: Bash]",
		// Truncated mid-document, the shape #101's rows have.
		RawJSON: []byte(`{"uuid":"turn-corrupt","type":"assistant","message":{"role":"assistant","content":[{"type":"tool_u`),
	}}); err != nil {
		t.Fatalf("insert turns: %v", err)
	}

	yes := true
	if err := database.InsertToolUses([]models.ToolUse{{
		TurnID: "turn-corrupt", SessionID: "session-c", ToolName: "Bash", Timestamp: ts,
		ToolUseID: "toolu_C", HasResult: true, ResultContent: "boom", ResultLength: 4, IsError: &yes,
	}}); err != nil {
		t.Fatalf("insert tool uses: %v", err)
	}

	result, err := s.getTurns(map[string]interface{}{"session_id": "session-c"})
	if err != nil {
		t.Fatalf("getTurns: %v", err)
	}
	resp := result.(map[string]interface{})
	turns := resp["turns"].([]map[string]interface{})

	var corrupt map[string]interface{}
	for _, turn := range turns {
		if turn["id"] == "turn-corrupt" {
			corrupt = turn
		}
	}
	if corrupt == nil {
		t.Fatalf("turn-corrupt missing from the page: %+v", turns)
	}
	if corrupt["raw_unavailable"] != true {
		t.Fatalf("expected turn-corrupt to be flagged raw_unavailable, got %+v", corrupt)
	}
	failed, ok := corrupt["failed_tools"].([]string)
	if want := []string{"Bash"}; !ok || !slices.Equal(failed, want) {
		t.Errorf("failed_tools = %v (present=%t), want %v — the stored flag does not depend on raw_json parsing",
			failed, ok, want)
	}
}

// TestGetSessionSummary_CountsFailedCalls gives the summary the same answer at
// session scope. A caller deciding whether a session is worth reading wants to
// know how much of it broke, and tools_used alone cannot say.
func TestGetSessionSummary_CountsFailedCalls(t *testing.T) {
	s, _ := newTestServer(t)
	seedFailedCalls(t, s)

	result, err := s.getSessionSummary(map[string]interface{}{"session_id": "session-e"})
	if err != nil {
		t.Fatalf("getSessionSummary: %v", err)
	}
	resp := result.(map[string]interface{})

	count, ok := resp["failed_tool_calls"].(int)
	if !ok {
		t.Fatalf("summary has no failed_tool_calls: %+v", resp)
	}
	if count != 1 {
		t.Errorf("failed_tool_calls = %d, want 1", count)
	}

	failed, ok := resp["failed_tools"].([]map[string]interface{})
	if !ok {
		t.Fatalf("summary has no failed_tools breakdown: %+v", resp)
	}
	if len(failed) != 1 || failed[0]["tool"] != "Edit" || failed[0]["count"] != 1 {
		t.Errorf("failed_tools = %+v, want one Edit at count 1", failed)
	}
}

// TestGetSessionSummary_OmitsFailureFieldsWhenNothingFailed keeps a clean
// session clean. A zero count present on every summary trains a caller to
// ignore the field; absent means "nothing to report", which is the same
// discipline unreadable_turns and warnings already follow.
func TestGetSessionSummary_OmitsFailureFieldsWhenNothingFailed(t *testing.T) {
	s, database := newTestServer(t)
	p := seedProject(t, database, "/test/clean")
	seedSession(t, database, "session-clean", p.ID)

	no := false
	if err := database.InsertToolUses([]models.ToolUse{{
		TurnID: "session-clean-turn-1", SessionID: "session-clean", ToolName: "Bash",
		Timestamp: time.Date(2026, 1, 1, 0, 0, 2, 0, time.UTC),
		ToolUseID: "toolu_CLEAN", HasResult: true, ResultContent: "fine", ResultLength: 4, IsError: &no,
	}}); err != nil {
		t.Fatalf("insert tool uses: %v", err)
	}

	result, err := s.getSessionSummary(map[string]interface{}{"session_id": "session-clean"})
	if err != nil {
		t.Fatalf("getSessionSummary: %v", err)
	}
	resp := result.(map[string]interface{})

	if _, present := resp["failed_tool_calls"]; present {
		t.Errorf("failed_tool_calls present on a session that broke nothing: %v", resp["failed_tool_calls"])
	}
	if _, present := resp["failed_tools"]; present {
		t.Errorf("failed_tools present on a session that broke nothing: %v", resp["failed_tools"])
	}
}
