// ABOUTME: Tests that a tool_result turn exports its body, read from the shape the transcripts actually write
// ABOUTME: Fixtures are real Claude Code tool_result lines; the old renderer read a shape no source produces

package export

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/2389-research/ccvault/pkg/models"
)

// claudeToolResultTurn builds the turn pkg/parser now produces for a
// tool_result line: type tool_result, raw_json verbatim, content a bounded
// preview. Named apart from richcontent_test.go's toolResultTurn, which builds
// the legacy $.toolUseId/$.data shape.
func claudeToolResultTurn(rawJSON, content string) models.Turn {
	return models.Turn{
		ID:        "turn-tr",
		SessionID: "sess-tr",
		Type:      "tool_result",
		Timestamp: time.Date(2026, 10, 21, 19, 51, 59, 0, time.UTC),
		Content:   content,
		RawJSON:   json.RawMessage(rawJSON),
	}
}

// TestExportToolResult_RendersStringBody covers the common shape: a
// tool_result whose content is a JSON string. The line is verbatim from
// ~/.claude/projects.
//
// Before #185 this renderer had never run on a single row — `tool_result` had
// none — and it would still have written nothing if it had: it read
// $.toolUseId and $.data, a shape no source in the repo produces. Claude Code
// puts the body at $.message.content[].content.
func TestExportToolResult_RendersStringBody(t *testing.T) {
	raw := `{"parentUuid":"58a90e59-4beb-4c74-ae64-f4162834e22c","type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01PVw7s4BZn2inWM92iqezJb","content":"Launching skill: terminal-title"}]},"uuid":"b31a72c7-bc09-48a0-be90-127808042c81","timestamp":"2026-10-21T19:51:59.142Z","sessionId":"e1a3e1a3-cddb-45df-87d0-a0dce6720cd9"}`

	var buf bytes.Buffer
	e := NewMarkdownExporter()
	if err := e.exportTurn(&buf, claudeToolResultTurn(raw, "[Tool Result: Launching skill: terminal-title]")); err != nil {
		t.Fatalf("exportTurn: %v", err)
	}

	got := buf.String()
	if !strings.Contains(got, "Tool Result") {
		t.Errorf("missing the tool-result heading:\n%s", got)
	}
	if !strings.Contains(got, "Launching skill: terminal-title") {
		t.Errorf("body not rendered:\n%s", got)
	}
}

// TestExportToolResult_RendersNestedTextBody covers the array shape — the one
// behind the archive's 145,648 turns that stored no content at all. The body
// has to come out of the nested blocks, not out of the preview.
func TestExportToolResult_RendersNestedTextBody(t *testing.T) {
	raw := `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_017GM1tmyfaN9gZUwQRwfLg6","type":"tool_result","content":[{"type":"text","text":"Navigated to https://www.grainger.com"}]}]},"uuid":"84517671-4d85-45ec-97fd-a2e8176a0d46","timestamp":"2026-10-07T18:29:14.572Z","sessionId":"7e1a1285-b69f-424e-aa80-8a17211c9cec"}`

	var buf bytes.Buffer
	e := NewMarkdownExporter()
	if err := e.exportTurn(&buf, claudeToolResultTurn(raw, "[Tool Result: Navigated to https://www.grainger.com]")); err != nil {
		t.Fatalf("exportTurn: %v", err)
	}

	if got := buf.String(); !strings.Contains(got, "Navigated to https://www.grainger.com") {
		t.Errorf("nested text block not rendered:\n%s", got)
	}
}

// TestExportToolResult_OmitsImagePayload keeps base64 out of the markdown. A
// screenshot result is 1-2 MB of it, and a reader who opened the export to
// read a conversation does not want a megabyte of it pasted inline.
func TestExportToolResult_OmitsImagePayload(t *testing.T) {
	needle := "BASE64PAYLOADCANARYAAAAAAAAAAAAAAAAAA"
	raw := `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_01EET9Fq7bw6Zy6iY7EFFoFy","type":"tool_result","content":[` +
		`{"type":"text","text":"Screenshot 'es-home-supporters' taken at 1440x700"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + needle + `"}}]}]},"uuid":"img-1","timestamp":"2026-10-07T18:29:14.572Z","sessionId":"s"}`

	var buf bytes.Buffer
	e := NewMarkdownExporter()
	if err := e.exportTurn(&buf, claudeToolResultTurn(raw, "")); err != nil {
		t.Fatalf("exportTurn: %v", err)
	}

	got := buf.String()
	if strings.Contains(got, needle) {
		t.Fatalf("base64 payload written into the export:\n%s", got[:min(len(got), 400)])
	}
	if !strings.Contains(got, "Screenshot 'es-home-supporters' taken at 1440x700") {
		t.Errorf("the caption beside the image should still render:\n%s", got)
	}
}

// TestExportToolResult_FallsBackToContent covers the rows whose raw_json can
// no longer answer: six of the archive's affected turns had their line
// replaced by the oversized-line placeholder, which keeps the turn's identity
// and drops its message. The stored preview is all those rows have left, and
// it is better than a heading with nothing under it.
func TestExportToolResult_FallsBackToContent(t *testing.T) {
	raw := `{"_ccvault_stripped":true,"_ccvault_original_size":1353901,"uuid":"ff8aa5b4-f653-469b-953a-3c264b33e610","sessionId":"8dcd6505-68fa-4d3a-9ff0-6f2033224e55","type":"user","timestamp":"2026-08-24T20:27:49.263Z"}`

	var buf bytes.Buffer
	e := NewMarkdownExporter()
	if err := e.exportTurn(&buf, claudeToolResultTurn(raw, "[Tool Result: 2 files changed]")); err != nil {
		t.Fatalf("exportTurn: %v", err)
	}

	if got := buf.String(); !strings.Contains(got, "2 files changed") {
		t.Errorf("stored content not used as a fallback:\n%s", got)
	}
}

// TestExportToolResult_SuppressedWhenDisabled pins the flag. --tool-results
// exists so a reader can ask for a transcript without the tool output; with
// it off, nothing of the turn is written — not even its heading.
func TestExportToolResult_SuppressedWhenDisabled(t *testing.T) {
	raw := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"CANARYRESULTBODY"}]},"uuid":"u","timestamp":"2026-10-21T19:51:59.142Z","sessionId":"s"}`

	var buf bytes.Buffer
	e := NewMarkdownExporter(WithToolResults(false))
	if err := e.exportTurn(&buf, claudeToolResultTurn(raw, "[Tool Result: CANARYRESULTBODY]")); err != nil {
		t.Fatalf("exportTurn: %v", err)
	}

	if got := buf.String(); got != "" {
		t.Errorf("wrote %q with tool results disabled, want nothing", got)
	}
}

// TestExportToolResult_PreviewsLongBody pins that a result is previewed and
// not dumped. Tool output runs to tens of thousands of lines — a `go test`
// run, a file read — and 100 lines is this file's convention for a body whose
// substance is the point.
func TestExportToolResult_PreviewsLongBody(t *testing.T) {
	body := strings.Repeat("line of output\\n", 500)
	raw := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"` + body + `"}]},"uuid":"u","timestamp":"2026-10-21T19:51:59.142Z","sessionId":"s"}`

	var buf bytes.Buffer
	e := NewMarkdownExporter()
	if err := e.exportTurn(&buf, claudeToolResultTurn(raw, "")); err != nil {
		t.Fatalf("exportTurn: %v", err)
	}

	got := buf.String()
	if !strings.Contains(got, "more lines") {
		t.Errorf("a 500-line result was not previewed:\n%s", got[:min(len(got), 300)])
	}
	if n := strings.Count(got, "line of output"); n > 101 {
		t.Errorf("rendered %d output lines, want at most 100", n)
	}
}

// TestExportToolResult_FenceOutlastsBackticksInTheBody is a defect this PR
// made reachable. The body was wrapped in a fixed three-backtick fence, and
// tool output carries triple-backtick fences of its own routinely — a Read of
// a markdown file, a Bash cat of a README. 10,514 of the archive's retagged
// tool results hold one, and 1,152 of its tool inputs do.
//
// An inner fence closes the block early: the rest of the result renders as
// markdown, and the real closing fence opens a new block that swallows every
// turn after it. The fence has to outlast the longest backtick run in the body.
func TestExportToolResult_FenceOutlastsBackticksInTheBody(t *testing.T) {
	body := "Here is a snippet:\\n```go\\nfunc main() {}\\n```\\nand that is all"
	raw := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"` + body + `"}]},"uuid":"u","timestamp":"2026-10-21T19:51:59.142Z","sessionId":"s"}`

	var buf bytes.Buffer
	e := NewMarkdownExporter()
	if err := e.exportTurn(&buf, claudeToolResultTurn(raw, "")); err != nil {
		t.Fatalf("exportTurn: %v", err)
	}

	got := buf.String()
	if !strings.Contains(got, "func main() {}") {
		t.Fatalf("body not rendered:\n%s", got)
	}
	// The fences that open and close the block must be longer than anything
	// inside it, so the block stays one block.
	if !strings.Contains(got, "````\n") {
		t.Errorf("the fence does not outlast the body's own fence:\n%s", got)
	}
	// The body's own fences survive verbatim inside the longer one.
	if !strings.Contains(got, "\n```go\n") {
		t.Errorf("the body's opening fence did not survive:\n%s", got)
	}
	// And the block is one block: exactly one opening and one closing fence of
	// four backticks, with everything else between them.
	if n := strings.Count(got, "````"); n != 2 {
		t.Errorf("found %d four-backtick fences, want exactly 2:\n%s", n, got)
	}
}

// TestExportToolUse_FenceOutlastsBackticksInTheBody is the same defect in the
// renderer beside it, reachable before this PR: a tool argument that is not a
// JSON object is written as the text it is, and 1,152 of the archive's inputs
// carry a fence.
func TestExportToolUse_FenceOutlastsBackticksInTheBody(t *testing.T) {
	var buf bytes.Buffer
	e := NewMarkdownExporter()
	e.exportToolUse(&buf, "apply_patch", []byte("```\nnot json, a diff\n```"))

	got := buf.String()
	if !strings.Contains(got, "not json, a diff") {
		t.Fatalf("body not rendered:\n%s", got)
	}
	if !strings.Contains(got, "````\n") {
		t.Errorf("the fence does not outlast the body's own fence:\n%s", got)
	}
}
