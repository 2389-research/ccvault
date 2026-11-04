// ABOUTME: Tests for retagging transcript `user` lines that carry only tool_result blocks as type tool_result
// ABOUTME: Fixtures are verbatim lines from ~/.claude/projects and shapes measured in the author's archive

package parser

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// The fixtures below are real lines, copied verbatim out of
// ~/.claude/projects/*/*.jsonl, except where a base64 payload was shortened —
// noted at the site. A survey of 334 live transcripts found these shapes and
// no others for a `user` line whose blocks are all tool_result:
//
//	11,000  content is a JSON string
//	   332  content is an array of text blocks
//	    61  content is an array of tool_reference blocks
//	    15  content is an array containing an image block
//
// The author's 52,375-session archive agrees, and adds the two shapes the live
// transcripts no longer hold: an empty-string content (150 rows) and a
// tool_result beside no prose at all. Every one of the archive's 315,985
// all-tool_result user turns carries exactly one content block.
const (
	// A string-valued tool_result. The common case: 11,000 of 11,408 in the
	// live transcripts.
	realStringResultLine = `{"parentUuid":"58a90e59-4beb-4c74-ae64-f4162834e22c","isSidechain":false,"promptId":"fed2d768-9711-4026-be34-83be94aaf712","type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01PVw7s4BZn2inWM92iqezJb","content":"Launching skill: terminal-title"}]},"uuid":"b31a72c7-bc09-48a0-be90-127808042c81","timestamp":"2026-10-21T19:51:59.142Z","toolUseResult":{"success":true,"commandName":"terminal-title"},"sourceToolAssistantUUID":"58a90e59-4beb-4c74-ae64-f4162834e22c","userType":"external","entrypoint":"sdk-cli","cwd":"/Users/dylanr/work/2389/kindred-instance/personas/janus","sessionId":"e1a3e1a3-cddb-45df-87d0-a0dce6720cd9","version":"2.1.226","gitBranch":"main"}`

	// An array-valued tool_result holding one text block. This is the shape
	// behind the archive's 145,648 contentless turns: UserContentBlock.Content
	// is typed as a string, so the whole block array failed to unmarshal and
	// the turn was stored with no content at all.
	realArrayResultLine = `{"parentUuid":"15db3d3b-705a-452a-97e7-4ce9d2ed5aa4","isSidechain":false,"promptId":"5eadd415-dbec-41b8-a3d5-9af2eed2c65a","type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_017GM1tmyfaN9gZUwQRwfLg6","type":"tool_result","content":[{"type":"text","text":"Navigated to https://www.grainger.com"}]}]},"uuid":"84517671-4d85-45ec-97fd-a2e8176a0d46","timestamp":"2026-10-07T18:29:14.572Z","toolUseResult":[{"type":"text","text":"Navigated to https://www.grainger.com"}],"sourceToolAssistantUUID":"15db3d3b-705a-452a-97e7-4ce9d2ed5aa4","session_id":"7e1a1285-b69f-424e-aa80-8a17211c9cec","userType":"external","entrypoint":"cli","cwd":"/private/tmp/claude-501/-Users-dylanr-work-2389-skills-dev-design/873d77ef-1542-4f8e-aa2b-34867187e1db/scratchpad/capture/grainger-live","sessionId":"7e1a1285-b69f-424e-aa80-8a17211c9cec","version":"2.1.226","gitBranch":"main"}`

	// An array of tool_reference blocks, which carry a tool_name and no text.
	realToolReferenceResultLine = `{"parentUuid":"347f034d-4c39-4c45-961f-613ce974c00f","isSidechain":false,"promptId":"45c4bd82-c687-4acd-b3df-86c85584a84d","type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_0156PaJs1qt3Q1CvJUXaw8g3","content":[{"type":"tool_reference","tool_name":"TaskCreate"},{"type":"tool_reference","tool_name":"TaskList"}]}]},"uuid":"246eee6e-4d90-43c8-ab29-cfb313adc075","timestamp":"2026-10-06T15:05:49.349Z","toolUseResult":{"matches":["TaskCreate","TaskList"],"query":"select:TaskCreate,TaskList","total_deferred_tools":42},"sourceToolAssistantUUID":"347f034d-4c39-4c45-961f-613ce974c00f","userType":"external","entrypoint":"sdk-cli","cwd":"/Users/dylanr/work/2389/skills-dev/repo-maintenance/evals/runs/dispatch-baseline/red/3/repo","sessionId":"7d7e2870-48aa-4e0b-ba3a-c8a777007e75","version":"2.1.226","gitBranch":"main"}`
)

// TestParseTurn_StringToolResultRetaggedAndTextExtracted covers the shape
// behind 170,187 of the archive's mislabeled rows: a string-valued
// tool_result, which the old code already summarised into content but filed
// under `user`. The type is the defect here, not the text.
func TestParseTurn_StringToolResultRetaggedAndTextExtracted(t *testing.T) {
	turn, err := ParseTurn([]byte(realStringResultLine))
	if err != nil {
		t.Fatalf("ParseTurn: %v", err)
	}

	if turn.Type != "tool_result" {
		t.Errorf("Type = %q, want %q", turn.Type, "tool_result")
	}
	if !strings.Contains(turn.Content, "Launching skill: terminal-title") {
		t.Errorf("Content = %q, want it to carry the result text", turn.Content)
	}
}

// TestParseTurn_ArrayToolResultRetaggedAndTextExtracted covers the shape
// behind the archive's 145,648 contentless turns. Both halves have to change:
// the type, and the text that the string-typed block struct could never read.
func TestParseTurn_ArrayToolResultRetaggedAndTextExtracted(t *testing.T) {
	turn, err := ParseTurn([]byte(realArrayResultLine))
	if err != nil {
		t.Fatalf("ParseTurn: %v", err)
	}

	if turn.Type != "tool_result" {
		t.Errorf("Type = %q, want %q", turn.Type, "tool_result")
	}
	if !strings.Contains(turn.Content, "Navigated to https://www.grainger.com") {
		t.Errorf("Content = %q, want it to carry the nested text block", turn.Content)
	}
}

// TestParseTurn_MixedProseAndToolResultStaysUser is the guard that keeps the
// retag from hiding anything. A line carrying both prose and a result is a
// user message with a result attached; calling it a tool result would file the
// person's words under a machine's answer.
//
// No such line exists in the author's 52,375-session archive — Claude Code
// writes results into a message of their own — so this pins a rule rather than
// a measurement. It is the rule that makes the retag safe to state as "all
// blocks", and a future producer that batches them would walk straight into it.
func TestParseTurn_MixedProseAndToolResultStaysUser(t *testing.T) {
	line := `{"uuid":"mixed-1","sessionId":"sess-mixed","type":"user","timestamp":"2026-10-07T18:29:14.572Z","message":{"role":"user","content":[` +
		`{"type":"text","text":"stop and read the failing test first"},` +
		`{"type":"tool_result","tool_use_id":"toolu_mixed","content":[{"type":"text","text":"exit status 1"}]}]}}`

	turn, err := ParseTurn([]byte(line))
	if err != nil {
		t.Fatalf("ParseTurn: %v", err)
	}

	if turn.Type != "user" {
		t.Errorf("Type = %q, want %q — prose beside a result is still a user message", turn.Type, "user")
	}
	if !strings.Contains(turn.Content, "stop and read the failing test first") {
		t.Errorf("Content = %q, want the prose kept", turn.Content)
	}
	if !strings.Contains(turn.Content, "exit status 1") {
		t.Errorf("Content = %q, want the attached result kept too", turn.Content)
	}
}

// TestParseTurn_ImageToolResultRetaggedWithNoContent pins the half of issue
// #28's policy that still applies here: an image payload is recognised by
// shape and its base64 never reaches turns.content, which feeds the FTS index.
// 642 of the archive's affected rows carry one, 191 of them beside text — and
// a stored partial that keeps the caption while dropping the screenshot is
// more misleading than no text at all.
//
// The base64 here is shortened to a canary; the surrounding shape is the one
// the archive carries.
func TestParseTurn_ImageToolResultRetaggedWithNoContent(t *testing.T) {
	needle := "BASE64PAYLOADCANARYAAAAAAAAAAAAAAAAAA"
	line := `{"uuid":"img-1","sessionId":"sess-img","type":"user","timestamp":"2026-10-07T18:29:14.572Z","message":{"role":"user","content":[` +
		`{"tool_use_id":"toolu_01EET9Fq7bw6Zy6iY7EFFoFy","type":"tool_result","content":[` +
		`{"type":"text","text":"Screenshot 'es-home-supporters' taken at 1440x700"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + needle + `"}}]}]}}`

	turn, err := ParseTurn([]byte(line))
	if err != nil {
		t.Fatalf("ParseTurn: %v", err)
	}

	if turn.Type != "tool_result" {
		t.Errorf("Type = %q, want %q", turn.Type, "tool_result")
	}
	if strings.Contains(turn.Content, needle) {
		t.Fatalf("base64 payload reached indexable content: %q", turn.Content)
	}
	if turn.Content != "" {
		t.Errorf("Content = %q, want empty for an image-bearing result", turn.Content)
	}
}

// TestParseTurn_ToolReferenceToolResultRetaggedWithNoContent covers the 287
// rows whose result is a list of tool references: real blocks, no text in
// them. The type must still be right; there is simply nothing to index.
func TestParseTurn_ToolReferenceToolResultRetaggedWithNoContent(t *testing.T) {
	turn, err := ParseTurn([]byte(realToolReferenceResultLine))
	if err != nil {
		t.Fatalf("ParseTurn: %v", err)
	}

	if turn.Type != "tool_result" {
		t.Errorf("Type = %q, want %q", turn.Type, "tool_result")
	}
	if turn.Content != "" {
		t.Errorf("Content = %q, want empty — tool_reference blocks carry no text", turn.Content)
	}
}

// TestParseTurn_EmptyStringToolResultRetaggedWithNoContent covers the 150 rows
// whose result content is the empty string, verbatim from the archive. The
// tool answered with nothing; that is still a tool result.
func TestParseTurn_EmptyStringToolResultRetaggedWithNoContent(t *testing.T) {
	line := `{"uuid":"empty-1","sessionId":"sess-empty","type":"user","timestamp":"2026-08-19T20:45:19.702Z","message":{"role":"user","content":[` +
		`{"tool_use_id":"toolu_01FJB7n5bKmJ1RSdCrG6Yx6E","type":"tool_result","content":"","is_error":false}]}}`

	turn, err := ParseTurn([]byte(line))
	if err != nil {
		t.Fatalf("ParseTurn: %v", err)
	}

	if turn.Type != "tool_result" {
		t.Errorf("Type = %q, want %q", turn.Type, "tool_result")
	}
	if turn.Content != "" {
		t.Errorf("Content = %q, want empty", turn.Content)
	}
}

// TestParseTurn_PlainUserMessagesStayUser is the negative control. Each of
// these is a real `user` line from the live transcripts whose content holds no
// tool_result block, and each must keep its type. The image and document cases
// are the archive's nine contentless user turns that are NOT tool results —
// pasted screenshots and PDFs, which carry no text to extract and never did.
func TestParseTurn_PlainUserMessagesStayUser(t *testing.T) {
	cases := []struct {
		name string
		line string
	}{
		{
			name: "string content",
			line: `{"uuid":"u-1","sessionId":"s","type":"user","timestamp":"2026-10-21T19:51:59.142Z","message":{"role":"user","content":"run the tests"}}`,
		},
		{
			name: "text block",
			line: `{"uuid":"u-2","sessionId":"s","type":"user","timestamp":"2026-10-21T19:51:59.142Z","message":{"role":"user","content":[{"type":"text","text":"run the tests"}]}}`,
		},
		{
			name: "image block only",
			line: `{"uuid":"u-3","sessionId":"s","type":"user","timestamp":"2026-10-21T19:51:59.142Z","message":{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"SHORTENED"}}]}}`,
		},
		{
			name: "document block only",
			line: `{"uuid":"u-4","sessionId":"s","type":"user","timestamp":"2026-10-21T19:51:59.142Z","message":{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"SHORTENED"}}]}}`,
		},
		{
			name: "empty block array",
			line: `{"uuid":"u-5","sessionId":"s","type":"user","timestamp":"2026-10-21T19:51:59.142Z","message":{"role":"user","content":[]}}`,
		},
		{
			name: "no message at all",
			line: `{"uuid":"u-6","sessionId":"s","type":"user","timestamp":"2026-10-21T19:51:59.142Z"}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			turn, err := ParseTurn([]byte(tc.line))
			if err != nil {
				t.Fatalf("ParseTurn: %v", err)
			}
			if turn.Type != "user" {
				t.Errorf("Type = %q, want %q", turn.Type, "user")
			}
		})
	}
}

// TestParseTurn_OtherTypesUnaffected pins that nothing outside `user` is
// reclassified. An assistant message holds tool_use blocks, never
// tool_result ones, and the retag must not reach a type it was not written
// for.
func TestParseTurn_OtherTypesUnaffected(t *testing.T) {
	line := `{"uuid":"a-1","sessionId":"s","type":"assistant","timestamp":"2026-10-21T19:51:59.142Z","message":{"model":"claude","role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls"}}]}}`

	turn, err := ParseTurn([]byte(line))
	if err != nil {
		t.Fatalf("ParseTurn: %v", err)
	}
	if turn.Type != "assistant" {
		t.Errorf("Type = %q, want %q", turn.Type, "assistant")
	}
}

// TestToolResultPreview_TruncatesOnRunes pins the cut as a rune count, not a
// byte count.
//
// Two reasons, and both of them bite. A byte cut can land inside a multi-byte
// rune and put invalid UTF-8 into turns.content and the FTS index behind it.
// And migration 017 has to reproduce this same string in SQL, where
// substr()'s unit is the character: 67,845 of the 145,006 rows the migration
// fills carry a non-ASCII character inside the first 200, so a byte cut here
// would disagree with the migration on 47% of them.
func TestToolResultPreview_TruncatesOnRunes(t *testing.T) {
	// 300 three-byte runes: 900 bytes, 300 characters.
	body := strings.Repeat("あ", 300)
	content := json.RawMessage(`[{"type":"tool_result","tool_use_id":"t","content":"` + body + `"}]`)

	got := extractUserContent(content)

	if !utf8.ValidString(got) {
		t.Fatalf("preview is not valid UTF-8: %q", got)
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(got, "[Tool Result: "), "]")
	if n := utf8.RuneCountInString(inner); n != 200 {
		t.Errorf("preview body = %d runes, want 200 (got %q)", n, inner)
	}
	if !strings.HasSuffix(inner, "...") {
		t.Errorf("preview body = %q, want it to end in the truncation marker", inner)
	}
}
