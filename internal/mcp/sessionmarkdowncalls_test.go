// ABOUTME: Tests that get_session's markdown carries a codex turn's prose and its calls, which means the exporter is wired to tool_uses.
// ABOUTME: End-to-end over real SQLite, because the defect was in the wiring and not in the formatter.

package mcp

import (
	"strings"
	"testing"
	"time"

	"github.com/2389-research/ccvault/pkg/models"
)

// TestGetSession_CodexMarkdownCarriesProseAndCalls is the wiring half of the
// export fix. The formatter can read calls out of a grouping it was handed and
// still produce an empty transcript if nobody hands it one, so this goes
// through getSession with a real database: the prose comes from the `content`
// column because codex's raw_json recognises no block, and the calls come from
// tool_uses because a codex turn's calls are on transcript lines of their own.
//
// Asserted on the markdown get_session actually returns, not on the exporter,
// for exactly that reason.
func TestGetSession_CodexMarkdownCarriesProseAndCalls(t *testing.T) {
	s, database := newTestServer(t)

	const prose = "I'm fetching the install instructions from that URL first."
	ts := time.Date(2026, 3, 11, 16, 15, 54, 0, time.UTC)

	if err := database.UpsertSession(&models.Session{
		ID:         "sess-codex-mcp",
		StartedAt:  ts,
		SourceFile: "/Users/x/.codex/sessions/2026/03/11/rollout-x.jsonl",
		Source:     "codex",
	}); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	if err := database.InsertTurns([]models.Turn{{
		ID: "sess-codex-mcp-turn-0", SessionID: "sess-codex-mcp", Type: "assistant",
		Timestamp: ts, Ordinal: 0,
		Content: prose,
		RawJSON: []byte(`{"timestamp":"2026-03-11T16:15:54.880Z","type":"response_item","payload":{` +
			`"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + prose + `"}]}}`),
	}}); err != nil {
		t.Fatalf("insert turns: %v", err)
	}
	if err := database.InsertToolUses([]models.ToolUse{{
		TurnID: "sess-codex-mcp-turn-0", SessionID: "sess-codex-mcp", ToolName: "exec_command",
		Timestamp: ts, ToolUseID: "call_abc123",
		InputJSON: `{"cmd":"ls -la /Users/x/.agents/skills","yield_time_ms":1000}`,
		HasResult: true, ResultContent: "total 0", ResultLength: 7,
	}}); err != nil {
		t.Fatalf("insert tool uses: %v", err)
	}

	result, err := s.getSession(map[string]interface{}{"session_id": "sess-codex-mcp"})
	if err != nil {
		t.Fatalf("getSession: %v", err)
	}
	markdown := mustField[string](t, resultMap(t, result), "markdown")

	if !strings.Contains(markdown, "fetching the install instructions") {
		t.Errorf("the assistant's prose is not in the markdown:\n%s", markdown)
	}
	if !strings.Contains(markdown, "exec_command") {
		t.Errorf("the turn's call is not in the markdown:\n%s", markdown)
	}
	// The arguments, not just the name — a call exported without them tells a
	// reader a command ran and not which one.
	if !strings.Contains(markdown, "ls -la /Users/x/.agents/skills") {
		t.Errorf("the call's arguments are not in the markdown:\n%s", markdown)
	}
}
