// ABOUTME: Tests that MCP reports a turn's tools for every source, not just the message.content[] shape.
// ABOUTME: codex, hex and jeff store raw entries with no top-level message key and used to read as "no tools".

package mcp

import (
	"testing"
	"time"

	"github.com/2389-research/ccvault/internal/db"
	"github.com/2389-research/ccvault/pkg/models"
)

// Real assistant-turn raw_json from the three sources whose shape the
// message.content[] enrichment cannot read. Each parses as JSON, and Go's
// json.Unmarshal ignores unknown keys, so the old enrichment "succeeded"
// against all three and reported no tools (#100).
const (
	// codex stores one response_item line per turn. The tool calls are
	// separate function_call / custom_tool_call lines, so they are not in
	// this blob at all and no amount of re-parsing it can recover them.
	rawCodexAssistant = `{"timestamp":"2026-01-01T00:00:01Z","type":"response_item",` +
		`"payload":{"type":"message","role":"assistant",` +
		`"content":[{"type":"output_text","text":"patching"}]}}`

	// jeff is the same story under different key names: assistant_message
	// lines and tool_request lines are separate entries.
	rawJeffAssistant = `{"conv_id":"c1","timestamp":"2026-01-01T00:00:01Z",` +
		`"entry_type":"assistant_message","data":{"content":"patching"}}`

	// hex stores the message object itself, with content as a plain string.
	rawHexAssistant = `{"role":"assistant","content":"patching","timestamp":"2026-01-01T00:00:01Z"}`
)

// seedForeignShapeSession writes one assistant turn carrying raw_json in a
// shape with no top-level `message` key, plus the tool_uses rows the
// adapter recorded for it — which is how every source stores its calls,
// and the only place codex's and jeff's exist.
func seedForeignShapeSession(t *testing.T, database *db.DB, sessionID, source, rawJSON string, toolNames []string) {
	t.Helper()

	ts := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)
	session := &models.Session{
		ID:         sessionID,
		StartedAt:  ts,
		SourceFile: "/tmp/" + sessionID + ".jsonl",
		Source:     source,
	}
	if err := database.UpsertSession(session); err != nil {
		t.Fatalf("upsert session %s: %v", sessionID, err)
	}

	turnID := sessionID + "-turn-1"
	if _, err := database.Exec(
		`INSERT INTO turns (id, session_id, type, timestamp, ordinal, content, raw_json)
		 VALUES (?, ?, 'assistant', ?, 0, ?, ?)`,
		turnID, sessionID, ts, "patching", rawJSON,
	); err != nil {
		t.Fatalf("insert turn for %s: %v", sessionID, err)
	}

	uses := make([]models.ToolUse, len(toolNames))
	for i, name := range toolNames {
		uses[i] = models.ToolUse{
			TurnID:    turnID,
			SessionID: sessionID,
			ToolName:  name,
			Timestamp: ts,
		}
	}
	if len(uses) > 0 {
		if err := database.InsertToolUses(uses); err != nil {
			t.Fatalf("insert tool uses for %s: %v", sessionID, err)
		}
	}
}

// foreignShapeCases is the set the issue names, with the calls each source
// really makes: codex's apply_patch arrives as a custom_tool_call, jeff's
// as a tool_request.
var foreignShapeCases = []struct {
	source string
	raw    string
	tools  []string
}{
	{source: "codex", raw: rawCodexAssistant, tools: []string{"apply_patch", "shell"}},
	{source: "jeff", raw: rawJeffAssistant, tools: []string{"read_file"}},
	{source: "hex", raw: rawHexAssistant, tools: []string{"search"}},
}

// TestGetTurns_ReportsToolsForEveryRawShape is issue #100. The enrichment
// read message.content[], which is claude-code's and nanoclaw's shape; the
// other three sources store raw entries with no top-level message key, the
// parse succeeded against them anyway, and the response said "no tools"
// confidently and wrongly. Nothing failed, so no warning could fire.
func TestGetTurns_ReportsToolsForEveryRawShape(t *testing.T) {
	for _, tc := range foreignShapeCases {
		t.Run(tc.source, func(t *testing.T) {
			s, database := newTestServer(t)
			sessionID := "session-" + tc.source
			seedForeignShapeSession(t, database, sessionID, tc.source, tc.raw, tc.tools)

			result, err := s.getTurns(map[string]interface{}{"session_id": sessionID})
			if err != nil {
				t.Fatalf("getTurns: %v", err)
			}
			m := resultMap(t, result)
			turns := mustField[[]map[string]interface{}](t, m, "turns")
			if len(turns) != 1 {
				t.Fatalf("got %d turns, want 1", len(turns))
			}

			got, present := turns[0]["tools"]
			if !present {
				t.Fatalf("turn reports no tools, but the session recorded %v — "+
					"a %s turn's calls are not in its raw_json", tc.tools, tc.source)
			}
			names, ok := got.([]string)
			if !ok {
				t.Fatalf("tools = %#v, want a list of names", got)
			}
			if len(names) != len(tc.tools) {
				t.Fatalf("tools = %v, want %v", names, tc.tools)
			}
			for i, want := range tc.tools {
				if names[i] != want {
					t.Errorf("tools[%d] = %q, want %q — calls are reported in the order issued", i, names[i], want)
				}
			}
		})
	}
}

// TestGetSessionSummary_CountsToolsForEveryRawShape is the aggregate half.
// tools_used was empty for a codex session that demonstrably ran
// apply_patch, which is the indirect signal the issue says is the only way
// this defect shows itself.
func TestGetSessionSummary_CountsToolsForEveryRawShape(t *testing.T) {
	for _, tc := range foreignShapeCases {
		t.Run(tc.source, func(t *testing.T) {
			s, database := newTestServer(t)
			sessionID := "session-summary-" + tc.source
			seedForeignShapeSession(t, database, sessionID, tc.source, tc.raw, tc.tools)

			result, err := s.getSessionSummary(map[string]interface{}{"session_id": sessionID})
			if err != nil {
				t.Fatalf("getSessionSummary: %v", err)
			}
			m := resultMap(t, result)

			used := mustField[[]map[string]interface{}](t, m, "tools_used")
			if len(used) != len(tc.tools) {
				t.Fatalf("tools_used = %#v, want %d entries for %v", used, len(tc.tools), tc.tools)
			}
			seen := make(map[string]bool, len(used))
			for _, entry := range used {
				name, _ := entry["tool"].(string)
				seen[name] = true
			}
			for _, want := range tc.tools {
				if !seen[want] {
					t.Errorf("tools_used does not name %q; it has %#v", want, used)
				}
			}
		})
	}
}

// TestGetTurns_DoesNotClaimAForeignShapeHadNoThinking is the other half of
// the same lie. has_thinking can only be read from a raw shape that records
// thinking blocks; on a shape that records none, false is not an answer, it
// is an absence of one. Omitting the field says so — the same contract
// failed_tools already follows, where absent and empty are different facts.
func TestGetTurns_DoesNotClaimAForeignShapeHadNoThinking(t *testing.T) {
	for _, tc := range foreignShapeCases {
		t.Run(tc.source, func(t *testing.T) {
			s, database := newTestServer(t)
			sessionID := "session-thinking-" + tc.source
			seedForeignShapeSession(t, database, sessionID, tc.source, tc.raw, tc.tools)

			result, err := s.getTurns(map[string]interface{}{"session_id": sessionID})
			if err != nil {
				t.Fatalf("getTurns: %v", err)
			}
			turns := mustField[[]map[string]interface{}](t, resultMap(t, result), "turns")

			if v, present := turns[0]["has_thinking"]; present {
				t.Errorf("has_thinking = %#v; a %s turn's raw shape carries no thinking blocks, "+
					"so false would be a claim rather than a reading", v, tc.source)
			}
		})
	}
}

// TestGetTurns_ReportsToolsOnATurnWhoseRawJSONCannotBeRead is the win #100
// hands to #62's remaining surface. The stored calls do not depend on
// raw_json parsing, so a turn marked raw_unavailable still names what it
// called — the field that was most missed is the one that now survives.
func TestGetTurns_ReportsToolsOnATurnWhoseRawJSONCannotBeRead(t *testing.T) {
	s, database := newTestServer(t)
	seedForeignShapeSession(t, database, "session-corrupt-tools", "claude-code",
		rawTruncated, []string{"Bash"})

	result, err := s.getTurns(map[string]interface{}{"session_id": "session-corrupt-tools"})
	if err != nil {
		t.Fatalf("getTurns: %v", err)
	}
	turns := mustField[[]map[string]interface{}](t, resultMap(t, result), "turns")

	if !mustField[bool](t, turns[0], "raw_unavailable") {
		t.Error("raw_unavailable = false; the seeded raw_json is not valid JSON")
	}
	tools := mustField[[]string](t, turns[0], "tools")
	if len(tools) != 1 || tools[0] != "Bash" {
		t.Errorf("tools = %#v, want [Bash] — the stored calls do not need raw_json to be readable", tools)
	}
}
