// ABOUTME: Tests that the conversation view renders a tool_result turn from the shape the transcripts write
// ABOUTME: Fixtures are real Claude Code tool_result lines; the old renderer read a shape no source produces

package tui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/2389-research/ccvault/pkg/models"
)

// toolResultModel is a conversation view holding one tool_result turn, as
// pkg/parser now produces it: type tool_result, raw_json verbatim, content a
// bounded preview.
func toolResultModel(rawJSON, content string) (*ConversationModel, models.Turn) {
	turn := models.Turn{
		ID:        "turn-tr",
		SessionID: "sess-tr",
		Type:      "tool_result",
		Timestamp: time.Date(2026, 10, 21, 19, 51, 59, 0, time.UTC),
		Content:   content,
		RawJSON:   json.RawMessage(rawJSON),
	}
	m := &ConversationModel{width: 100, turns: []models.Turn{turn}}
	return m, turn
}

// TestRenderToolResult_RendersStringBody covers the common shape, verbatim
// from ~/.claude/projects. Before #185 this renderer had no rows to run on,
// and it would have written nothing if it had: it read $.toolUseId and $.data,
// which no source in the repo produces.
func TestRenderToolResult_RendersStringBody(t *testing.T) {
	raw := `{"parentUuid":"58a90e59-4beb-4c74-ae64-f4162834e22c","type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01PVw7s4BZn2inWM92iqezJb","content":"Launching skill: terminal-title"}]},"uuid":"b31a72c7-bc09-48a0-be90-127808042c81","timestamp":"2026-10-21T19:51:59.142Z","sessionId":"e1a3e1a3-cddb-45df-87d0-a0dce6720cd9"}`

	m, turn := toolResultModel(raw, "[Tool Result: Launching skill: terminal-title]")

	if got := m.renderToolResult(turn); !strings.Contains(got, "Launching skill: terminal-title") {
		t.Errorf("renderToolResult = %q, want the result body", got)
	}
}

// TestRenderToolResult_RendersNestedTextBody covers the array shape — the one
// behind the archive's 145,648 turns that stored no content at all, so the
// preview cannot stand in for the body here.
func TestRenderToolResult_RendersNestedTextBody(t *testing.T) {
	raw := `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_017GM1tmyfaN9gZUwQRwfLg6","type":"tool_result","content":[{"type":"text","text":"Navigated to https://www.grainger.com"}]}]},"uuid":"84517671-4d85-45ec-97fd-a2e8176a0d46","timestamp":"2026-10-07T18:29:14.572Z","sessionId":"7e1a1285-b69f-424e-aa80-8a17211c9cec"}`

	m, turn := toolResultModel(raw, "")

	if got := m.renderToolResult(turn); !strings.Contains(got, "Navigated to https://www.grainger.com") {
		t.Errorf("renderToolResult = %q, want the nested text block", got)
	}
}

// TestRenderToolResult_OmitsImagePayload keeps base64 out of the terminal. A
// screenshot result is 1-2 MB of it and would take the view with it.
func TestRenderToolResult_OmitsImagePayload(t *testing.T) {
	needle := "BASE64PAYLOADCANARYAAAAAAAAAAAAAAAAAA"
	raw := `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_01EET9Fq7bw6Zy6iY7EFFoFy","type":"tool_result","content":[` +
		`{"type":"text","text":"Screenshot 'es-home-supporters' taken at 1440x700"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + needle + `"}}]}]},"uuid":"img-1","timestamp":"2026-10-07T18:29:14.572Z","sessionId":"s"}`

	m, turn := toolResultModel(raw, "")

	got := m.renderToolResult(turn)
	if strings.Contains(got, needle) {
		t.Fatalf("base64 payload reached the view: %q", got[:minInt(len(got), 300)])
	}
	if !strings.Contains(got, "Screenshot 'es-home-supporters' taken at 1440x700") {
		t.Errorf("the caption beside the image should still render: %q", got)
	}
}

// TestRenderToolResult_FallsBackToContent covers the turns whose raw_json can
// no longer answer — the oversized-line placeholder keeps a turn's identity
// and drops its message. The old code returned the empty string for these,
// and renderConversation skips a tool_result that renders empty, so the turn
// would have vanished from the view entirely.
func TestRenderToolResult_FallsBackToContent(t *testing.T) {
	raw := `{"_ccvault_stripped":true,"_ccvault_original_size":1353901,"uuid":"ff8aa5b4-f653-469b-953a-3c264b33e610","sessionId":"8dcd6505-68fa-4d3a-9ff0-6f2033224e55","type":"user","timestamp":"2026-08-24T20:27:49.263Z"}`

	m, turn := toolResultModel(raw, "[Tool Result: 2 files changed]")

	if got := m.renderToolResult(turn); !strings.Contains(got, "2 files changed") {
		t.Errorf("renderToolResult = %q, want the stored content as a fallback", got)
	}
}

// TestRenderConversation_ShowsRetaggedToolResult is the end-to-end half: the
// turn has to appear in the rendered conversation under its own label, which
// is what the retag was for.
func TestRenderConversation_ShowsRetaggedToolResult(t *testing.T) {
	raw := `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_017GM1tmyfaN9gZUwQRwfLg6","type":"tool_result","content":[{"type":"text","text":"NAVIGATEDCANARY"}]}]},"uuid":"u","timestamp":"2026-10-07T18:29:14.572Z","sessionId":"s"}`

	m, _ := toolResultModel(raw, "")

	got := m.renderConversation()
	if !strings.Contains(got, "TOOL RESULT") {
		t.Errorf("conversation is missing the tool-result label:\n%s", got)
	}
	if !strings.Contains(got, "NAVIGATEDCANARY") {
		t.Errorf("conversation is missing the result body:\n%s", got)
	}
}

// TestRenderToolResult_TruncatesLongBody pins the cap. Tool output runs to
// tens of thousands of characters and the conversation view is a terminal.
//
// ToValidUTF8 matters here for the same reason it does in formatToolUseBlock:
// a byte cut can land inside a multi-byte rune and leave the terminal a
// fragment to draw.
func TestRenderToolResult_TruncatesLongBody(t *testing.T) {
	body := strings.Repeat("あ", 2000)
	raw := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"` + body + `"}]},"uuid":"u","timestamp":"2026-10-21T19:51:59.142Z","sessionId":"s"}`

	m, turn := toolResultModel(raw, "")

	got := m.renderToolResult(turn)
	if !strings.Contains(got, "truncated") {
		t.Errorf("a 2,000-character result was not truncated: %q", got[:minInt(len(got), 200)])
	}
	if strings.ContainsRune(got, '�') {
		t.Errorf("truncation split a rune and left a replacement character: %q", got)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
