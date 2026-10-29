// ABOUTME: Tests for how the pi adapter records tool calls and the results answering them.
// ABOUTME: Covers argument capture, matching on toolCallId, and pi's isError flag.

package pi

import (
	"testing"

	"github.com/2389-research/ccvault/pkg/adapter"
)

// onlyToolUse returns the single tool use of the session's first turn, failing
// the test rather than panicking when the adapter recorded none.
func onlyToolUse(t *testing.T, ps *adapter.ParsedSession) adapter.ParsedToolUse {
	t.Helper()
	if len(ps.Turns) == 0 {
		t.Fatal("got no turns")
	}
	if len(ps.Turns[0].ToolUses) != 1 {
		t.Fatalf("got %d tool uses on turn 0, want 1", len(ps.Turns[0].ToolUses))
	}
	return ps.Turns[0].ToolUses[0]
}

// toolCallTurn is an assistant message making one call to the read tool, the
// shape that accounts for 454 of the content blocks in pi's own fixtures.
const toolCallTurn = `{"type":"message","id":"e1","parentId":null,"timestamp":"2025-12-09T00:54:05.000Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"toolu_012yui","name":"read","arguments":{"path":"/Users/dyl/work/alpha/main.ts"}}],"api":"anthropic-messages","provider":"anthropic","model":"claude-opus-4-5","stopReason":"toolUse","timestamp":1765233665294}}`

func TestParse_AssistantToolCallBecomesAToolUse(t *testing.T) {
	got := parseLines(t, alpha, toolCallTurn)

	if len(got.Turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(got.Turns))
	}
	uses := got.Turns[0].ToolUses
	if len(uses) != 1 {
		t.Fatalf("got %d tool uses, want 1", len(uses))
	}
	if uses[0].ToolName != "read" {
		t.Errorf("ToolName = %q, want %q", uses[0].ToolName, "read")
	}
	if uses[0].ToolUseID != "toolu_012yui" {
		t.Errorf("ToolUseID = %q, want %q", uses[0].ToolUseID, "toolu_012yui")
	}
	if uses[0].InputJSON != `{"path":"/Users/dyl/work/alpha/main.ts"}` {
		t.Errorf("InputJSON = %q, want the arguments object", uses[0].InputJSON)
	}
	if uses[0].InputLength != len(uses[0].InputJSON) {
		t.Errorf("InputLength = %d, want %d", uses[0].InputLength, len(uses[0].InputJSON))
	}
	if uses[0].HasResult {
		t.Error("HasResult = true before any result arrived")
	}
}

// TestParse_OneAssistantTurnCanMakeSeveralCalls because pi emits parallel
// calls as sibling blocks of one message, which is how both of its fixture
// sessions open.
func TestParse_OneAssistantTurnCanMakeSeveralCalls(t *testing.T) {
	got := parseLines(t, alpha,
		`{"type":"message","id":"e1","parentId":null,"timestamp":"2025-12-09T00:54:05.000Z","message":{"role":"assistant","content":[{"type":"text","text":"reading both"},{"type":"toolCall","id":"c1","name":"read","arguments":{"path":"a.ts"}},{"type":"toolCall","id":"c2","name":"read","arguments":{"path":"b.ts"}}],"timestamp":1765233665294}}`,
	)
	if len(got.Turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(got.Turns))
	}
	if len(got.Turns[0].ToolUses) != 2 {
		t.Fatalf("got %d tool uses, want 2", len(got.Turns[0].ToolUses))
	}
	if got.Turns[0].Content != "reading both" {
		t.Errorf("Content = %q, want the text block only", got.Turns[0].Content)
	}
}

// TestParse_ToolResultAttachesToItsCallAndIsNotATurn is the one role that does
// not become a turn: it answers a call an assistant turn made, and it belongs
// on that call's row.
func TestParse_ToolResultAttachesToItsCallAndIsNotATurn(t *testing.T) {
	got := parseLines(t, alpha, toolCallTurn,
		`{"type":"message","id":"e2","parentId":"e1","timestamp":"2025-12-09T00:54:09.000Z","message":{"role":"toolResult","toolCallId":"toolu_012yui","toolName":"read","content":[{"type":"text","text":"export const x = 1"}],"isError":false,"timestamp":1765233669393}}`,
	)

	if len(got.Turns) != 1 {
		t.Fatalf("got %d turns, want 1 — a toolResult is not a turn", len(got.Turns))
	}
	use := onlyToolUse(t, got)
	if !use.HasResult {
		t.Fatal("HasResult = false, want true")
	}
	if use.ResultContent != "export const x = 1" {
		t.Errorf("ResultContent = %q, want %q", use.ResultContent, "export const x = 1")
	}
	if use.ResultLength != len("export const x = 1") {
		t.Errorf("ResultLength = %d, want %d", use.ResultLength, len("export const x = 1"))
	}
	if use.IsError == nil || *use.IsError {
		t.Errorf("IsError = %v, want a non-nil false — pi always says whether a result failed", use.IsError)
	}
}

// TestParse_AFailedToolResultMarksTheCallingTurn puts the failure where a
// reader would look for it, the way the Claude Code adapter does: pi records
// it on the result message, and the turn that shows up in a session is the one
// that made the call.
func TestParse_AFailedToolResultMarksTheCallingTurn(t *testing.T) {
	got := parseLines(t, alpha, toolCallTurn,
		`{"type":"message","id":"e2","parentId":"e1","timestamp":"2025-12-09T00:54:09.000Z","message":{"role":"toolResult","toolCallId":"toolu_012yui","toolName":"read","content":[{"type":"text","text":"ENOENT: no such file"}],"isError":true,"timestamp":1765233669393}}`,
	)

	use := onlyToolUse(t, got)
	if use.IsError == nil || !*use.IsError {
		t.Errorf("IsError = %v, want a non-nil true", use.IsError)
	}
	if !got.Turns[0].HasError {
		t.Error("the calling turn's HasError = false, want true")
	}
	if got.Metadata["has_error"] != true {
		t.Errorf("Metadata[has_error] = %v, want true", got.Metadata["has_error"])
	}
}

// TestParse_ResultsAreMatchedOnIDNotOnOrder keeps one tool's output off
// another tool's row. pi writes a toolCallId on both halves of every pair, so
// unlike jeff there is never a reason to fall back to order here.
func TestParse_ResultsAreMatchedOnIDNotOnOrder(t *testing.T) {
	got := parseLines(t, alpha,
		`{"type":"message","id":"e1","parentId":null,"timestamp":"2025-12-09T00:54:05.000Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"c1","name":"read","arguments":{"path":"a.ts"}},{"type":"toolCall","id":"c2","name":"bash","arguments":{"cmd":"ls"}}],"timestamp":1765233665294}}`,
		// Answered in the opposite order to the calls.
		`{"type":"message","id":"e2","parentId":"e1","timestamp":"2025-12-09T00:54:06.000Z","message":{"role":"toolResult","toolCallId":"c2","toolName":"bash","content":[{"type":"text","text":"a.ts b.ts"}],"isError":false,"timestamp":1765233665300}}`,
		`{"type":"message","id":"e3","parentId":"e2","timestamp":"2025-12-09T00:54:07.000Z","message":{"role":"toolResult","toolCallId":"c1","toolName":"read","content":[{"type":"text","text":"export const x = 1"}],"isError":false,"timestamp":1765233665301}}`,
	)

	uses := got.Turns[0].ToolUses
	if len(uses) != 2 {
		t.Fatalf("got %d tool uses, want 2", len(uses))
	}
	if uses[0].ToolName != "read" || uses[0].ResultContent != "export const x = 1" {
		t.Errorf("call c1 got result %q on tool %q, want the read output", uses[0].ResultContent, uses[0].ToolName)
	}
	if uses[1].ToolName != "bash" || uses[1].ResultContent != "a.ts b.ts" {
		t.Errorf("call c2 got result %q on tool %q, want the bash output", uses[1].ResultContent, uses[1].ToolName)
	}
}

// TestParse_APureToolCallTurnHasNoContent pins a decision rather than an
// accident. 217 of the 544 turns in pi's own before-compaction fixture are
// assistant messages made entirely of toolCall blocks, and the model said
// nothing in any of them.
//
// The alternative was to synthesise a label — "[Tool: read]", which is what
// the Claude Code adapter falls back to — and it was rejected on search
// grounds: the tool name, its arguments and its result are already stored on
// the tool use itself, where they are indexed and queryable, so repeating them
// in the turn's text would make a full-text search for "read" or "bash" match
// several hundred turns per session that are not about reading or bashing.
func TestParse_APureToolCallTurnHasNoContent(t *testing.T) {
	got := parseLines(t, alpha, toolCallTurn)

	if len(got.Turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(got.Turns))
	}
	if got.Turns[0].Content != "" {
		t.Errorf("Content = %q, want empty — the model said nothing in this turn", got.Turns[0].Content)
	}
	if len(got.Turns[0].ToolUses) != 1 {
		t.Errorf("got %d tool uses, want 1 — the turn is empty of text, not of substance",
			len(got.Turns[0].ToolUses))
	}
}

// TestParse_AnUnansweredCallHasNoResult covers a session that was interrupted
// while a tool was still running. The call happened and is recorded; claiming
// an empty result for it would be claiming the tool returned nothing.
func TestParse_AnUnansweredCallHasNoResult(t *testing.T) {
	got := parseLines(t, alpha, toolCallTurn)

	use := onlyToolUse(t, got)
	if use.HasResult {
		t.Error("HasResult = true for a call nothing answered")
	}
	if use.IsError != nil {
		t.Errorf("IsError = %v, want nil — nothing said whether it failed", use.IsError)
	}
}
