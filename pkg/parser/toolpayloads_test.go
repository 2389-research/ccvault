// ABOUTME: Tests for ExtractToolUses — the provider tool-use id, the input, and the result linked back to its call.
// ABOUTME: Fixtures are real Claude Code transcript shapes: tool_use in an assistant turn, tool_result in the next user turn.

package parser

import (
	"strings"
	"testing"

	"github.com/2389-research/ccvault/pkg/toolpayload"
)

// twoTurnTranscript is a tool call and its result in the shape Claude Code
// writes them: the tool_use in an assistant message, the tool_result in the
// user message that follows, joined by tool_use_id.
const twoTurnTranscript = `{"uuid":"a-1","sessionId":"s-1","type":"assistant","timestamp":"2026-09-01T10:00:00.000Z","message":{"model":"claude","role":"assistant","content":[{"type":"tool_use","id":"toolu_01ABC","name":"Bash","input":{"command":"git status --short","description":"check tree"}}]}}
{"uuid":"u-1","sessionId":"s-1","type":"user","timestamp":"2026-09-01T10:00:01.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01ABC","content":"?? docs/multiplayer.md"}]}}`

func extractFrom(t *testing.T, transcript string) []toolUseForTest {
	t.Helper()
	turns, _, _, err := ParseSessionReader(strings.NewReader(transcript), "/test/payloads.jsonl")
	if err != nil {
		t.Fatalf("ParseSessionReader: %v", err)
	}
	out := make([]toolUseForTest, 0)
	for _, tu := range ExtractToolUses(turns) {
		out = append(out, toolUseForTest{
			ToolUseID:  tu.ToolUseID,
			ToolName:   tu.ToolName,
			InputJSON:  tu.InputJSON,
			HasResult:  tu.HasResult,
			Result:     tu.ResultContent,
			ResultLen:  tu.ResultLength,
			OmitReason: tu.ResultOmittedReason,
		})
	}
	return out
}

type toolUseForTest struct {
	ToolUseID  string
	ToolName   string
	InputJSON  string
	HasResult  bool
	Result     string
	ResultLen  int
	OmitReason string
}

// TestExtractToolUses_CarriesIDInputAndResult is the whole point of issue #28:
// the call's provider id, its input, and the result it produced all land on
// the row instead of being parsed and thrown away.
func TestExtractToolUses_CarriesIDInputAndResult(t *testing.T) {
	got := extractFrom(t, twoTurnTranscript)

	if len(got) != 1 {
		t.Fatalf("got %d tool uses, want 1: %+v", len(got), got)
	}
	tu := got[0]

	if tu.ToolUseID != "toolu_01ABC" {
		t.Errorf("ToolUseID = %q, want %q", tu.ToolUseID, "toolu_01ABC")
	}
	if !strings.Contains(tu.InputJSON, "git status --short") {
		t.Errorf("InputJSON = %q, want it to carry the command", tu.InputJSON)
	}
	if !tu.HasResult {
		t.Error("HasResult = false, want true — the transcript carries a tool_result for this call")
	}
	if tu.Result != "?? docs/multiplayer.md" {
		t.Errorf("ResultContent = %q, want %q", tu.Result, "?? docs/multiplayer.md")
	}
	if tu.ResultLen != len("?? docs/multiplayer.md") {
		t.Errorf("ResultLength = %d, want %d", tu.ResultLen, len("?? docs/multiplayer.md"))
	}
	if tu.OmitReason != "" {
		t.Errorf("ResultOmittedReason = %q, want empty", tu.OmitReason)
	}
}

// TestExtractToolUses_InputLengthMatchesStoredInput pins that the recorded
// length describes the stored bytes. Inputs are never omitted, so these must
// not drift apart.
func TestExtractToolUses_InputLengthMatchesStoredInput(t *testing.T) {
	turns, _, _, err := ParseSessionReader(strings.NewReader(twoTurnTranscript), "/test/payloads.jsonl")
	if err != nil {
		t.Fatalf("ParseSessionReader: %v", err)
	}
	tus := ExtractToolUses(turns)
	if len(tus) != 1 {
		t.Fatalf("got %d tool uses, want 1", len(tus))
	}
	if tus[0].InputLength != len(tus[0].InputJSON) {
		t.Errorf("InputLength = %d, len(InputJSON) = %d", tus[0].InputLength, len(tus[0].InputJSON))
	}
}

// TestExtractToolUses_ResultLinkedAcrossDistantTurns covers the realistic
// interleaving: two calls in one assistant turn, their results arriving in
// separate later turns and out of call order. Matching on tool_use_id is what
// keeps them straight; pairing by position would cross them.
func TestExtractToolUses_ResultLinkedAcrossDistantTurns(t *testing.T) {
	transcript := `{"uuid":"a-1","sessionId":"s-2","type":"assistant","timestamp":"2026-09-01T10:00:00.000Z","message":{"model":"claude","role":"assistant","content":[{"type":"tool_use","id":"toolu_FIRST","name":"Bash","input":{"command":"one"}},{"type":"tool_use","id":"toolu_SECOND","name":"Grep","input":{"pattern":"two"}}]}}
{"uuid":"u-1","sessionId":"s-2","type":"user","timestamp":"2026-09-01T10:00:01.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_SECOND","content":"grep output"}]}}
{"uuid":"u-2","sessionId":"s-2","type":"user","timestamp":"2026-09-01T10:00:02.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_FIRST","content":"bash output"}]}}`

	got := extractFrom(t, transcript)
	if len(got) != 2 {
		t.Fatalf("got %d tool uses, want 2: %+v", len(got), got)
	}
	if got[0].ToolUseID != "toolu_FIRST" || got[0].Result != "bash output" {
		t.Errorf("first call got %+v, want toolu_FIRST / %q", got[0], "bash output")
	}
	if got[1].ToolUseID != "toolu_SECOND" || got[1].Result != "grep output" {
		t.Errorf("second call got %+v, want toolu_SECOND / %q", got[1], "grep output")
	}
}

// TestExtractToolUses_NoResultIsNotAnEmptyResult covers an interrupted call —
// 24 of them in the author's archive. HasResult false is a different fact from
// a result that came back empty, and the row has to be able to say which.
func TestExtractToolUses_NoResultIsNotAnEmptyResult(t *testing.T) {
	transcript := `{"uuid":"a-1","sessionId":"s-3","type":"assistant","timestamp":"2026-09-01T10:00:00.000Z","message":{"model":"claude","role":"assistant","content":[{"type":"tool_use","id":"toolu_ORPHAN","name":"Bash","input":{"command":"sleep 600"}}]}}`

	got := extractFrom(t, transcript)
	if len(got) != 1 {
		t.Fatalf("got %d tool uses, want 1", len(got))
	}
	if got[0].HasResult {
		t.Error("HasResult = true, want false for a call with no tool_result in the transcript")
	}

	withEmpty := `{"uuid":"a-1","sessionId":"s-4","type":"assistant","timestamp":"2026-09-01T10:00:00.000Z","message":{"model":"claude","role":"assistant","content":[{"type":"tool_use","id":"toolu_EMPTY","name":"Bash","input":{"command":"true"}}]}}
{"uuid":"u-1","sessionId":"s-4","type":"user","timestamp":"2026-09-01T10:00:01.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_EMPTY","content":""}]}}`

	got = extractFrom(t, withEmpty)
	if len(got) != 1 {
		t.Fatalf("got %d tool uses, want 1", len(got))
	}
	if !got[0].HasResult {
		t.Error("HasResult = false, want true — the tool answered, it just answered with nothing")
	}
	if got[0].ResultLen != 0 || got[0].OmitReason != "" {
		t.Errorf("empty result got len=%d reason=%q, want 0 and empty", got[0].ResultLen, got[0].OmitReason)
	}
}

// TestExtractToolUses_BulkReadKeepsLengthOnly covers the policy arriving
// through the parser: a Read's result is 65% of nothing useful in an FTS index,
// so the row keeps file_path and a length.
func TestExtractToolUses_BulkReadKeepsLengthOnly(t *testing.T) {
	body := strings.Repeat("a line of a file\\n", 500)
	transcript := `{"uuid":"a-1","sessionId":"s-5","type":"assistant","timestamp":"2026-09-01T10:00:00.000Z","message":{"model":"claude","role":"assistant","content":[{"type":"tool_use","id":"toolu_READ","name":"Read","input":{"file_path":"/tmp/big.txt"}}]}}
{"uuid":"u-1","sessionId":"s-5","type":"user","timestamp":"2026-09-01T10:00:01.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_READ","content":"` + body + `"}]}}`

	got := extractFrom(t, transcript)
	if len(got) != 1 {
		t.Fatalf("got %d tool uses, want 1", len(got))
	}
	if got[0].Result != "" {
		t.Errorf("stored %d bytes of a bulk read, want none", len(got[0].Result))
	}
	if got[0].OmitReason != toolpayload.OmitBulkRead {
		t.Errorf("OmitReason = %q, want %q", got[0].OmitReason, toolpayload.OmitBulkRead)
	}
	if got[0].ResultLen == 0 {
		t.Error("ResultLength = 0, want the original size")
	}
	if !got[0].HasResult {
		t.Error("HasResult = false — an omitted result is still a result")
	}
}

// TestExtractToolUses_ImagePayloadNeverStored is the base64 canary at the
// parser level. An image-shaped result must leave no trace of its payload on
// the row, whatever the tool is called.
func TestExtractToolUses_ImagePayloadNeverStored(t *testing.T) {
	needle := "BASE64PAYLOADCANARYAAAAAAAAAAAAAAAAAA"
	transcript := `{"uuid":"a-1","sessionId":"s-6","type":"assistant","timestamp":"2026-09-01T10:00:00.000Z","message":{"model":"claude","role":"assistant","content":[{"type":"tool_use","id":"toolu_IMG","name":"mcp__arbitrary__name","input":{"url":"http://x"}}]}}
{"uuid":"u-1","sessionId":"s-6","type":"user","timestamp":"2026-09-01T10:00:01.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_IMG","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + needle + `"}}]}]}}`

	got := extractFrom(t, transcript)
	if len(got) != 1 {
		t.Fatalf("got %d tool uses, want 1", len(got))
	}
	if strings.Contains(got[0].Result, needle) {
		t.Fatalf("base64 payload reached the stored result: %q", got[0].Result)
	}
	if got[0].OmitReason != toolpayload.OmitImage {
		t.Errorf("OmitReason = %q, want %q", got[0].OmitReason, toolpayload.OmitImage)
	}
	if got[0].ResultLen == 0 {
		t.Error("ResultLength = 0, want the payload's original size")
	}
}

// TestExtractToolUses_TextArrayResultConcatenated covers the content shape
// half the archive's results use: an array holding one text block.
func TestExtractToolUses_TextArrayResultConcatenated(t *testing.T) {
	transcript := `{"uuid":"a-1","sessionId":"s-7","type":"assistant","timestamp":"2026-09-01T10:00:00.000Z","message":{"model":"claude","role":"assistant","content":[{"type":"tool_use","id":"toolu_ARR","name":"mcp__nanoclaw__ssh_localhost","input":{"command":"uptime"}}]}}
{"uuid":"u-1","sessionId":"s-7","type":"user","timestamp":"2026-09-01T10:00:01.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_ARR","content":[{"type":"text","text":"load average: 2.10"}]}]}}`

	got := extractFrom(t, transcript)
	if len(got) != 1 {
		t.Fatalf("got %d tool uses, want 1", len(got))
	}
	if got[0].Result != "load average: 2.10" {
		t.Errorf("ResultContent = %q, want %q", got[0].Result, "load average: 2.10")
	}
}

// TestExtractToolUses_TurnContentIsUnchanged pins the boundary of this change.
// turns.content feeds turns_fts, which holds a million documents; the tool
// payloads become searchable through their own index, and nothing here is
// allowed to start rewriting what the existing one contains.
func TestExtractToolUses_TurnContentIsUnchanged(t *testing.T) {
	turns, _, _, err := ParseSessionReader(strings.NewReader(twoTurnTranscript), "/test/payloads.jsonl")
	if err != nil {
		t.Fatalf("ParseSessionReader: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("got %d turns, want 2", len(turns))
	}
	// The assistant turn still summarises its tool call the way it always has.
	if want := "[Tool: Bash] $ git status --short"; turns[0].Content != want {
		t.Errorf("assistant turn content = %q, want %q", turns[0].Content, want)
	}
	// And the tool_result turn still gets the 200-char preview form.
	if want := "[Tool Result: ?? docs/multiplayer.md]"; turns[1].Content != want {
		t.Errorf("tool_result turn content = %q, want %q", turns[1].Content, want)
	}
}
