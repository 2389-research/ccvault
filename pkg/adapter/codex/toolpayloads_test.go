// ABOUTME: Tests that the codex adapter carries call_id, arguments, and output onto each tool use.
// ABOUTME: Covers both of codex's tool shapes — function_call/function_call_output and custom_tool_call/custom_tool_call_output.

package codex

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/2389-research/ccvault/pkg/adapter"
)

// parseLines writes lines to a temp session file and parses it.
func parseLines(t *testing.T, lines []map[string]any) *adapter.ParsedSession {
	t.Helper()
	fpath := filepath.Join(t.TempDir(), "rollout-test.jsonl")
	writeJSONLFile(t, fpath, lines)
	parsed, err := New().Parse(fpath)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return parsed
}

// allToolUses flattens the tool uses across a session's turns.
func allToolUses(s *adapter.ParsedSession) []adapter.ParsedToolUse {
	var out []adapter.ParsedToolUse
	for _, turn := range s.Turns {
		out = append(out, turn.ToolUses...)
	}
	return out
}

func codexBaseLines() []map[string]any {
	return []map[string]any{
		{
			"timestamp": "2026-03-11T16:25:01.231Z",
			"type":      "session_meta",
			"payload":   map[string]any{"id": "sess-1", "cwd": "/Users/someone/project"},
		},
		{
			"timestamp": "2026-03-11T16:25:07.982Z",
			"type":      "response_item",
			"payload": map[string]any{
				"type": "message", "role": "assistant",
				"content": []map[string]any{{"type": "output_text", "text": "Looking."}},
			},
		},
	}
}

// TestParse_FunctionCallCarriesIDArgumentsAndOutput covers codex's main tool
// shape. call_id is the id on both halves, so it is both the stored id and the
// key that links the output back to the call.
func TestParse_FunctionCallCarriesIDArgumentsAndOutput(t *testing.T) {
	lines := append(codexBaseLines(),
		map[string]any{
			"timestamp": "2026-03-11T16:25:07.985Z",
			"type":      "response_item",
			"payload": map[string]any{
				"type":      "function_call",
				"name":      "exec_command",
				"arguments": `{"cmd":"ls -la","workdir":"/tmp"}`,
				"call_id":   "call_tQRetcPjbp",
			},
		},
		map[string]any{
			"timestamp": "2026-03-11T16:25:08.000Z",
			"type":      "response_item",
			"payload": map[string]any{
				"type":    "function_call_output",
				"call_id": "call_tQRetcPjbp",
				"output":  "Process exited with code 0\ntotal 8",
			},
		},
	)

	tus := allToolUses(parseLines(t, lines))
	if len(tus) != 1 {
		t.Fatalf("got %d tool uses, want 1: %+v", len(tus), tus)
	}
	tu := tus[0]

	if tu.ToolUseID != "call_tQRetcPjbp" {
		t.Errorf("ToolUseID = %q, want %q", tu.ToolUseID, "call_tQRetcPjbp")
	}
	if !strings.Contains(tu.InputJSON, "ls -la") {
		t.Errorf("InputJSON = %q, want the arguments", tu.InputJSON)
	}
	if tu.InputLength != len(tu.InputJSON) {
		t.Errorf("InputLength = %d, len(InputJSON) = %d", tu.InputLength, len(tu.InputJSON))
	}
	if !tu.HasResult {
		t.Error("HasResult = false, want true")
	}
	if want := "Process exited with code 0\ntotal 8"; tu.ResultContent != want {
		t.Errorf("ResultContent = %q, want %q", tu.ResultContent, want)
	}
	if tu.ResultLength != len("Process exited with code 0\ntotal 8") {
		t.Errorf("ResultLength = %d, want %d", tu.ResultLength, len("Process exited with code 0\ntotal 8"))
	}
}

// TestParse_CustomToolCallIsRecorded covers apply_patch, codex's edit tool,
// which arrives as custom_tool_call with its body in `input` rather than
// `arguments`. The adapter skipped the whole payload kind before this change,
// so every codex file edit was invisible to the archive.
func TestParse_CustomToolCallIsRecorded(t *testing.T) {
	lines := append(codexBaseLines(),
		map[string]any{
			"timestamp": "2026-03-11T16:44:01.327Z",
			"type":      "response_item",
			"payload": map[string]any{
				"type":    "custom_tool_call",
				"status":  "completed",
				"call_id": "call_vpkPlrbeFG",
				"name":    "apply_patch",
				"input":   "*** Begin Patch\n*** Add File: /tmp/plan.md\n+# Plan\n",
			},
		},
		map[string]any{
			"timestamp": "2026-03-11T16:44:01.357Z",
			"type":      "response_item",
			"payload": map[string]any{
				"type":    "custom_tool_call_output",
				"call_id": "call_vpkPlrbeFG",
				"output":  `{"output":"Success. Updated the following files:\nA /tmp/plan.md\n"}`,
			},
		},
	)

	tus := allToolUses(parseLines(t, lines))
	if len(tus) != 1 {
		t.Fatalf("got %d tool uses, want 1: %+v", len(tus), tus)
	}
	tu := tus[0]

	if tu.ToolName != "apply_patch" {
		t.Errorf("ToolName = %q, want %q", tu.ToolName, "apply_patch")
	}
	if tu.ToolUseID != "call_vpkPlrbeFG" {
		t.Errorf("ToolUseID = %q, want %q", tu.ToolUseID, "call_vpkPlrbeFG")
	}
	if !strings.Contains(tu.InputJSON, "*** Begin Patch") {
		t.Errorf("InputJSON = %q, want the patch body", tu.InputJSON)
	}
	if !strings.Contains(tu.ResultContent, "Success.") {
		t.Errorf("ResultContent = %q, want the output", tu.ResultContent)
	}
}

// TestParse_OutputArrivingBeforeItsCallStillLinks covers ordering. codex writes
// the output after the call today, but the link is call_id, not adjacency, and
// the adapter buffers rather than assuming.
func TestParse_OutputArrivingBeforeItsCallStillLinks(t *testing.T) {
	lines := append(codexBaseLines(),
		map[string]any{
			"timestamp": "2026-03-11T16:25:08.000Z",
			"type":      "response_item",
			"payload": map[string]any{
				"type": "function_call_output", "call_id": "call_late", "output": "early output",
			},
		},
		map[string]any{
			"timestamp": "2026-03-11T16:25:07.985Z",
			"type":      "response_item",
			"payload": map[string]any{
				"type": "function_call", "name": "exec_command", "arguments": "{}", "call_id": "call_late",
			},
		},
	)

	tus := allToolUses(parseLines(t, lines))
	if len(tus) != 1 {
		t.Fatalf("got %d tool uses, want 1", len(tus))
	}
	if !tus[0].HasResult || tus[0].ResultContent != "early output" {
		t.Errorf("got %+v, want the buffered output attached", tus[0])
	}
}

// TestParse_CallWithNoOutputHasNoResult keeps "the tool never answered"
// distinct from "it answered with nothing".
func TestParse_CallWithNoOutputHasNoResult(t *testing.T) {
	lines := append(codexBaseLines(),
		map[string]any{
			"timestamp": "2026-03-11T16:25:07.985Z",
			"type":      "response_item",
			"payload": map[string]any{
				"type": "function_call", "name": "exec_command", "arguments": "{}", "call_id": "call_orphan",
			},
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
