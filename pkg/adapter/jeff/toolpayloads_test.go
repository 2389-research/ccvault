// ABOUTME: Tests that the jeff adapter carries tool_id, params, and output_preview onto each tool use.
// ABOUTME: Covers the case real jeff data is mostly made of — an empty tool_id, where linking has to fall back to order.

package jeff

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/2389-research/ccvault/pkg/adapter"
)

const convID = "d41f67af-1234-5678-9abc-def012345678"

func parseLines(t *testing.T, lines []map[string]any) *adapter.ParsedSession {
	t.Helper()
	fpath := filepath.Join(t.TempDir(), "20260224_195605.jsonl")
	writeJSONLFile(t, fpath, lines)
	parsed, err := New().Parse(fpath)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return parsed
}

func allToolUses(s *adapter.ParsedSession) []adapter.ParsedToolUse {
	var out []adapter.ParsedToolUse
	for _, turn := range s.Turns {
		out = append(out, turn.ToolUses...)
	}
	return out
}

func jeffBaseLines() []map[string]any {
	return []map[string]any{
		{
			"timestamp": "2026-02-24T19:56:05.125559Z", "entry_type": "session_start",
			"conversation_id": convID,
			"data":            map[string]any{"model": "claude-sonnet-4-5"},
		},
		{
			"timestamp": "2026-02-24T19:56:15.000000Z", "entry_type": "assistant_message",
			"conversation_id": convID,
			"data":            map[string]any{"content": "Searching."},
		},
	}
}

// TestParse_ToolRequestCarriesIDParamsAndResult covers a jeff tool call where
// tool_id is set, which is the minority (109 of 742 real requests) but the
// shape the link is designed around.
func TestParse_ToolRequestCarriesIDParamsAndResult(t *testing.T) {
	lines := append(jeffBaseLines(),
		map[string]any{
			"timestamp": "2026-02-24T19:56:16.000000Z", "entry_type": "tool_request",
			"conversation_id": convID,
			"data": map[string]any{
				"tool_name": "search_drive",
				"params":    map[string]any{"query": "Q4 report"},
				"tool_id":   "tool-001",
			},
		},
		map[string]any{
			"timestamp": "2026-02-24T19:56:18.000000Z", "entry_type": "tool_result",
			"conversation_id": convID,
			"data": map[string]any{
				"success": true, "output_preview": "Found 3 results", "tool_id": "tool-001",
			},
		},
	)

	tus := allToolUses(parseLines(t, lines))
	if len(tus) != 1 {
		t.Fatalf("got %d tool uses, want 1: %+v", len(tus), tus)
	}
	tu := tus[0]

	if tu.ToolUseID != "tool-001" {
		t.Errorf("ToolUseID = %q, want %q", tu.ToolUseID, "tool-001")
	}
	if !strings.Contains(tu.InputJSON, "Q4 report") {
		t.Errorf("InputJSON = %q, want the params", tu.InputJSON)
	}
	if !tu.HasResult || tu.ResultContent != "Found 3 results" {
		t.Errorf("got HasResult=%v ResultContent=%q, want true / %q", tu.HasResult, tu.ResultContent, "Found 3 results")
	}
	if tu.ResultLength != len("Found 3 results") {
		t.Errorf("ResultLength = %d, want %d", tu.ResultLength, len("Found 3 results"))
	}
}

// TestParse_EmptyToolIDLinksByOrder covers what jeff actually writes: tool_id
// is the empty string on 633 of 742 real tool_requests. Keying the link on an
// id would collapse every one of those onto each other, so the fallback is the
// file's order — each result belongs to the oldest request still waiting.
func TestParse_EmptyToolIDLinksByOrder(t *testing.T) {
	lines := append(jeffBaseLines(),
		map[string]any{
			"timestamp": "2026-02-24T19:56:16.000000Z", "entry_type": "tool_request",
			"conversation_id": convID,
			"data": map[string]any{
				"tool_name": "email", "params": map[string]any{"operation": "list"}, "tool_id": "",
			},
		},
		map[string]any{
			"timestamp": "2026-02-24T19:56:17.000000Z", "entry_type": "tool_result",
			"conversation_id": convID,
			"data":            map[string]any{"success": true, "output_preview": "Found 2 message(s)", "tool_id": ""},
		},
		map[string]any{
			"timestamp": "2026-02-24T19:56:18.000000Z", "entry_type": "tool_request",
			"conversation_id": convID,
			"data": map[string]any{
				"tool_name": "calendar", "params": map[string]any{"operation": "list"}, "tool_id": "",
			},
		},
		map[string]any{
			"timestamp": "2026-02-24T19:56:19.000000Z", "entry_type": "tool_result",
			"conversation_id": convID,
			"data":            map[string]any{"success": true, "output_preview": "No events today", "tool_id": ""},
		},
	)

	tus := allToolUses(parseLines(t, lines))
	if len(tus) != 2 {
		t.Fatalf("got %d tool uses, want 2: %+v", len(tus), tus)
	}
	if tus[0].ToolName != "email" || tus[0].ResultContent != "Found 2 message(s)" {
		t.Errorf("first = %+v, want email / %q", tus[0], "Found 2 message(s)")
	}
	if tus[1].ToolName != "calendar" || tus[1].ResultContent != "No events today" {
		t.Errorf("second = %+v, want calendar / %q", tus[1], "No events today")
	}
	if tus[0].ToolUseID != "" || tus[1].ToolUseID != "" {
		t.Errorf("ToolUseIDs = %q / %q, want both empty — jeff recorded none, and inventing one would make it unjoinable", tus[0].ToolUseID, tus[1].ToolUseID)
	}
}

// TestParse_UnmatchedIDBearingResultIsDropped covers a tool_result that names
// a tool_id no pending call has. It happens when the request preceded any
// assistant turn (so no row was created for it) or when the call was already
// answered.
//
// Guessing the oldest pending call there attaches one tool's output to a
// different tool's row, which stores the wrong text and then makes it
// searchable under the wrong tool name. An id that does not match is a result
// ccvault cannot place, and dropping it is the only honest option.
func TestParse_UnmatchedIDBearingResultIsDropped(t *testing.T) {
	lines := append(jeffBaseLines(),
		map[string]any{
			"timestamp": "2026-02-24T19:56:16.000000Z", "entry_type": "tool_request",
			"conversation_id": convID,
			"data": map[string]any{
				"tool_name": "email", "params": map[string]any{"operation": "list"}, "tool_id": "tool-001",
			},
		},
		map[string]any{
			"timestamp": "2026-02-24T19:56:18.000000Z", "entry_type": "tool_result",
			"conversation_id": convID,
			"data": map[string]any{
				"success": true, "output_preview": "belongs to some other call", "tool_id": "tool-999",
			},
		},
	)

	tus := allToolUses(parseLines(t, lines))
	if len(tus) != 1 {
		t.Fatalf("got %d tool uses, want 1: %+v", len(tus), tus)
	}
	if tus[0].HasResult {
		t.Errorf("the email call was given a result it does not own: %q", tus[0].ResultContent)
	}
	if tus[0].ResultContent != "" {
		t.Errorf("ResultContent = %q, want empty", tus[0].ResultContent)
	}
}

// TestParse_IDLessResultDoesNotConsumeAnIDBearingCall covers the other side of
// the same mistake. A result with no id has to fall back to order, but it must
// not swallow a call that jeff did identify — that call's own result is still
// coming, and taking its slot mislabels both.
func TestParse_IDLessResultDoesNotConsumeAnIDBearingCall(t *testing.T) {
	lines := append(jeffBaseLines(),
		// Identified call, waiting for its own result.
		map[string]any{
			"timestamp": "2026-02-24T19:56:16.000000Z", "entry_type": "tool_request",
			"conversation_id": convID,
			"data": map[string]any{
				"tool_name": "search_drive", "params": map[string]any{"query": "Q4"}, "tool_id": "tool-001",
			},
		},
		// Unidentified call, which is what the id-less result below belongs to.
		map[string]any{
			"timestamp": "2026-02-24T19:56:17.000000Z", "entry_type": "tool_request",
			"conversation_id": convID,
			"data": map[string]any{
				"tool_name": "calendar", "params": map[string]any{"operation": "list"}, "tool_id": "",
			},
		},
		map[string]any{
			"timestamp": "2026-02-24T19:56:18.000000Z", "entry_type": "tool_result",
			"conversation_id": convID,
			"data":            map[string]any{"success": true, "output_preview": "No events today", "tool_id": ""},
		},
		map[string]any{
			"timestamp": "2026-02-24T19:56:19.000000Z", "entry_type": "tool_result",
			"conversation_id": convID,
			"data":            map[string]any{"success": true, "output_preview": "Found 3 results", "tool_id": "tool-001"},
		},
	)

	tus := allToolUses(parseLines(t, lines))
	if len(tus) != 2 {
		t.Fatalf("got %d tool uses, want 2: %+v", len(tus), tus)
	}
	if tus[0].ToolName != "search_drive" || tus[0].ResultContent != "Found 3 results" {
		t.Errorf("identified call got %+v, want search_drive / %q", tus[0], "Found 3 results")
	}
	if tus[1].ToolName != "calendar" || tus[1].ResultContent != "No events today" {
		t.Errorf("unidentified call got %+v, want calendar / %q", tus[1], "No events today")
	}
}

// TestParse_UnansweredToolRequestHasNoResult keeps the two absences apart in
// jeff too.
func TestParse_UnansweredToolRequestHasNoResult(t *testing.T) {
	lines := append(jeffBaseLines(),
		map[string]any{
			"timestamp": "2026-02-24T19:56:16.000000Z", "entry_type": "tool_request",
			"conversation_id": convID,
			"data":            map[string]any{"tool_name": "email", "params": map[string]any{}, "tool_id": ""},
		},
	)

	tus := allToolUses(parseLines(t, lines))
	if len(tus) != 1 {
		t.Fatalf("got %d tool uses, want 1", len(tus))
	}
	if tus[0].HasResult {
		t.Error("HasResult = true, want false")
	}
}
