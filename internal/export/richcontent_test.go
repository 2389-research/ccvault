// ABOUTME: Tests for assistant rich-content rendering, tool_use blocks and tool results.
// ABOUTME: Covers the thinking gate, each tool's renderer, and every truncation boundary.

package export

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/2389-research/ccvault/pkg/models"
)

// assistantTurn builds an assistant turn whose RawJSON carries the given
// content blocks, which is the shape exportAssistantRichContent parses.
func assistantTurn(t *testing.T, blocks ...map[string]any) models.Turn {
	t.Helper()

	raw, err := json.Marshal(map[string]any{
		"message": map[string]any{"content": blocks},
	})
	if err != nil {
		t.Fatalf("marshal raw json: %v", err)
	}
	return models.Turn{
		ID:        "turn-1",
		Type:      "assistant",
		Timestamp: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		RawJSON:   raw,
	}
}

func renderTurn(t *testing.T, e *MarkdownExporter, turn models.Turn) string {
	t.Helper()

	var buf bytes.Buffer
	if err := e.exportTurn(&buf, turn); err != nil {
		t.Fatalf("exportTurn: %v", err)
	}
	return buf.String()
}

// --- rich content dispatch ----------------------------------------------

func TestRichContent_RendersTextBlocks(t *testing.T) {
	out := renderTurn(t, NewMarkdownExporter(),
		assistantTurn(t, map[string]any{"type": "text", "text": "the answer is 42"}))

	if !strings.Contains(out, "the answer is 42") {
		t.Errorf("text block missing from output:\n%s", out)
	}
}

// The thinking gate is the contract WithThinking exists for. Both states are
// asserted, because a test that only rendered with thinking enabled would
// pass just as happily against a gate wired backwards.
func TestRichContent_ThinkingIsGatedByTheOption(t *testing.T) {
	turn := assistantTurn(t, map[string]any{"type": "thinking", "thinking": "secret reasoning"})

	withThinking := renderTurn(t, NewMarkdownExporter(WithThinking(true)), turn)
	if !strings.Contains(withThinking, "secret reasoning") {
		t.Errorf("WithThinking(true) dropped the thinking block:\n%s", withThinking)
	}
	if !strings.Contains(withThinking, "💭 Thinking") {
		t.Errorf("WithThinking(true) rendered no thinking disclosure:\n%s", withThinking)
	}

	withoutThinking := renderTurn(t, NewMarkdownExporter(WithThinking(false)), turn)
	if strings.Contains(withoutThinking, "secret reasoning") {
		t.Errorf("WithThinking(false) leaked the thinking text:\n%s", withoutThinking)
	}
}

// An assistant turn whose RawJSON will not parse must fall back to the plain
// Content column rather than emitting nothing. Dropping the turn silently is
// the failure this guards -- the transcript would lose a message.
func TestRichContent_UnparseableRawJSONFallsBackToPlainContent(t *testing.T) {
	turn := models.Turn{
		ID:        "turn-1",
		Type:      "assistant",
		Timestamp: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		RawJSON:   json.RawMessage(`{not valid json`),
		Content:   "plain fallback text",
	}

	out := renderTurn(t, NewMarkdownExporter(), turn)
	if !strings.Contains(out, "plain fallback text") {
		t.Errorf("the plain Content was not used as a fallback:\n%s", out)
	}
}

// Empty text and thinking blocks must not produce stray blank sections.
func TestRichContent_EmptyBlocksRenderNothing(t *testing.T) {
	out := renderTurn(t, NewMarkdownExporter(),
		assistantTurn(t,
			map[string]any{"type": "text", "text": ""},
			map[string]any{"type": "thinking", "thinking": ""}))

	if strings.Contains(out, "💭 Thinking") {
		t.Errorf("an empty thinking block rendered a disclosure:\n%s", out)
	}
}

// An unrecognised block type is skipped rather than rendered raw, so a new
// upstream content type cannot dump JSON into a transcript.
func TestRichContent_UnknownBlockTypeIsSkipped(t *testing.T) {
	out := renderTurn(t, NewMarkdownExporter(),
		assistantTurn(t, map[string]any{"type": "image", "text": "should-not-appear"}))

	if strings.Contains(out, "should-not-appear") {
		t.Errorf("an unknown block type was rendered:\n%s", out)
	}
}

// --- tool_use renderers -------------------------------------------------

func toolUseTurn(t *testing.T, name string, input map[string]any) models.Turn {
	t.Helper()

	return assistantTurn(t, map[string]any{
		"type": "tool_use", "name": name, "input": input,
	})
}

// Each tool has its own renderer pulling named fields out of the input. The
// table asserts the field actually reaches the output, so a renderer reading
// the wrong key fails rather than silently emitting only the header.
func TestToolUse_RendersEachToolsSalientFields(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input map[string]any
		want  []string
	}{
		{"Bash", map[string]any{"command": "ls -la", "description": "list files"},
			[]string{"🔧 Tool: Bash", "ls -la", "list files"}},
		{"Read", map[string]any{"file_path": "/etc/hosts"},
			[]string{"📄 Reading:", "/etc/hosts"}},
		{"Write", map[string]any{"file_path": "/tmp/x", "content": "hello"},
			[]string{"📝 Writing:", "/tmp/x", "hello"}},
		{"Edit", map[string]any{"file_path": "/tmp/y", "old_string": "before", "new_string": "after"},
			[]string{"✏️ Editing:", "/tmp/y", "**Replace:**", "before", "**With:**", "after"}},
		{"Glob", map[string]any{"pattern": "**/*.go"},
			[]string{"🔍 Pattern:", "**/*.go"}},
		{"Grep", map[string]any{"pattern": "needle", "path": "/src"},
			[]string{"🔍 Search:", "needle", "In:", "/src"}},
		{"WebFetch", map[string]any{"url": "https://example.com"},
			[]string{"🌐 Fetching:", "https://example.com"}},
		{"WebSearch", map[string]any{"query": "how to go"},
			[]string{"🔍 Searching:", "how to go"}},
		{"Task", map[string]any{"description": "do a thing", "prompt": "the full prompt"},
			[]string{"📋 Task:", "do a thing", "Task Prompt", "the full prompt"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := renderTurn(t, NewMarkdownExporter(), toolUseTurn(t, tc.name, tc.input))
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("output is missing %q:\n%s", want, out)
				}
			}
		})
	}
}

// The default branch: an unrecognised tool still has to show its parameters,
// because the alternative is a transcript naming a tool and nothing about
// what it was asked to do.
func TestToolUse_UnknownToolFallsBackToListingStringParams(t *testing.T) {
	out := renderTurn(t, NewMarkdownExporter(),
		toolUseTurn(t, "SomeFutureTool", map[string]any{
			"target": "the-target-value",
			"count":  7, // non-string, deliberately dropped by the renderer
		}))

	if !strings.Contains(out, "🔧 Tool: SomeFutureTool") {
		t.Errorf("tool name missing:\n%s", out)
	}
	if !strings.Contains(out, "target") || !strings.Contains(out, "the-target-value") {
		t.Errorf("the generic renderer dropped a string param:\n%s", out)
	}
}

func TestToolUse_NoInputRendersOnlyTheHeader(t *testing.T) {
	out := renderTurn(t, NewMarkdownExporter(),
		assistantTurn(t, map[string]any{"type": "tool_use", "name": "Bash"}))

	if !strings.Contains(out, "🔧 Tool: Bash") {
		t.Errorf("tool header missing:\n%s", out)
	}
	if strings.Contains(out, "```bash") {
		t.Errorf("a tool with no input rendered an empty command block:\n%s", out)
	}
}

// --- truncation boundaries ----------------------------------------------

// Write previews the first 50 lines and says how many it dropped. Both sides
// of the boundary are tested: at exactly 50 nothing is truncated, at 51 the
// notice appears and names 1 line. An off-by-one here either hides a line or
// claims a truncation that did not happen.
func TestToolUse_WritePreviewTruncatesOnlyPast50Lines(t *testing.T) {
	lines50 := make([]string, 50)
	for i := range lines50 {
		lines50[i] = fmt.Sprintf("line %d", i)
	}

	at := renderTurn(t, NewMarkdownExporter(),
		toolUseTurn(t, "Write", map[string]any{
			"file_path": "/tmp/x", "content": strings.Join(lines50, "\n"),
		}))
	if strings.Contains(at, "more lines") {
		t.Errorf("exactly 50 lines was truncated:\n%s", at)
	}
	if !strings.Contains(at, "line 49") {
		t.Errorf("the 50th line is missing:\n%s", at)
	}

	over := renderTurn(t, NewMarkdownExporter(),
		toolUseTurn(t, "Write", map[string]any{
			"file_path": "/tmp/x", "content": strings.Join(append(lines50, "line 50"), "\n"),
		}))
	if !strings.Contains(over, "... (1 more lines)") {
		t.Errorf("51 lines did not report exactly 1 dropped line:\n%s", over)
	}
	if strings.Contains(over, "line 50\n```") {
		t.Errorf("the 51st line was rendered despite the truncation notice:\n%s", over)
	}
}

// Edit truncates old_string and new_string at 200 characters with an
// ellipsis. Asserting the kept prefix and the dropped tail, rather than just
// the ellipsis, is what makes a wrong cut point fail.
//
// The two fields use different fill characters on purpose. Filling both with
// the same string lets a correct truncation of one satisfy the assertion
// meant for the other, which is exactly what happened on the first draft of
// this test: a mutation cutting old_string at 100 passed, because
// new_string's untouched 200-char prefix was still somewhere in the output.
func TestToolUse_EditTruncatesStringsAt200Characters(t *testing.T) {
	oldFill := strings.Repeat("O", 200)
	newFill := strings.Repeat("N", 200)

	out := renderTurn(t, NewMarkdownExporter(),
		toolUseTurn(t, "Edit", map[string]any{
			"file_path":  "/tmp/y",
			"old_string": oldFill + "OLDTAIL",
			"new_string": newFill + "NEWTAIL",
		}))

	if strings.Contains(out, "OLDTAIL") {
		t.Errorf("old_string past 200 chars survived truncation:\n%s", out)
	}
	if strings.Contains(out, "NEWTAIL") {
		t.Errorf("new_string past 200 chars survived truncation:\n%s", out)
	}
	if !strings.Contains(out, oldFill+"...") {
		t.Error("old_string was not cut at exactly 200 chars plus an ellipsis")
	}
	if !strings.Contains(out, newFill+"...") {
		t.Error("new_string was not cut at exactly 200 chars plus an ellipsis")
	}
}

func TestToolUse_EditLeavesShortStringsIntact(t *testing.T) {
	out := renderTurn(t, NewMarkdownExporter(),
		toolUseTurn(t, "Edit", map[string]any{
			"file_path": "/tmp/y", "old_string": "short before", "new_string": "short after",
		}))

	if strings.Contains(out, "...") {
		t.Errorf("a short string was truncated:\n%s", out)
	}
}

// Task truncates its prompt at 500 characters.
func TestToolUse_TaskTruncatesThePromptAt500Characters(t *testing.T) {
	long := strings.Repeat("b", 500) + "TAIL"

	out := renderTurn(t, NewMarkdownExporter(),
		toolUseTurn(t, "Task", map[string]any{"description": "d", "prompt": long}))

	if strings.Contains(out, "TAIL") {
		t.Errorf("prompt text past 500 chars survived truncation:\n%s", out)
	}
	if !strings.Contains(out, strings.Repeat("b", 500)+"...") {
		t.Error("the first 500 chars plus an ellipsis is not what was emitted")
	}
}

// The generic renderer truncates each string param at 100 characters.
func TestToolUse_GenericRendererTruncatesParamsAt100Characters(t *testing.T) {
	out := renderTurn(t, NewMarkdownExporter(),
		toolUseTurn(t, "Unknown", map[string]any{"k": strings.Repeat("c", 100) + "TAIL"}))

	if strings.Contains(out, "TAIL") {
		t.Errorf("param text past 100 chars survived truncation:\n%s", out)
	}
	if !strings.Contains(out, strings.Repeat("c", 100)+"...") {
		t.Error("the first 100 chars plus an ellipsis is not what was emitted")
	}
}

// --- tool results -------------------------------------------------------

func toolResultTurn(t *testing.T, data map[string]any) models.Turn {
	t.Helper()

	raw, err := json.Marshal(map[string]any{"toolUseId": "tu-1", "data": data})
	if err != nil {
		t.Fatalf("marshal raw json: %v", err)
	}
	return models.Turn{
		ID:        "turn-2",
		Type:      "tool_result",
		Timestamp: time.Date(2026, 1, 1, 12, 0, 1, 0, time.UTC),
		RawJSON:   raw,
	}
}

// The output field is chosen from several candidates in a fixed order.
// Each case sets the winner alongside a lower-precedence field, so a
// reordered switch picks the wrong one and fails -- which a test setting one
// field at a time could not detect.
func TestToolResult_OutputPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name string
		data map[string]any
		want string
		deny string
	}{
		{"output beats stdout", map[string]any{"output": "WIN", "stdout": "lose"}, "WIN", "lose"},
		{"stdout beats content", map[string]any{"stdout": "WIN", "content": "lose"}, "WIN", "lose"},
		{"content beats result", map[string]any{"content": "WIN", "result": "lose"}, "WIN", "lose"},
		{"result is the last resort", map[string]any{"result": "WIN"}, "WIN", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := renderTurn(t, NewMarkdownExporter(), toolResultTurn(t, tc.data))
			if !strings.Contains(out, tc.want) {
				t.Errorf("expected %q in output:\n%s", tc.want, out)
			}
			if tc.deny != "" && strings.Contains(out, tc.deny) {
				t.Errorf("lower-precedence field %q was used instead:\n%s", tc.deny, out)
			}
		})
	}
}

// stderr is appended to stdout rather than replacing it: a command's error
// output is usually the interesting half, and losing either loses the story.
func TestToolResult_StderrIsAppendedToStdout(t *testing.T) {
	out := renderTurn(t, NewMarkdownExporter(),
		toolResultTurn(t, map[string]any{"stdout": "normal output", "stderr": "the error"}))

	if !strings.Contains(out, "normal output") {
		t.Errorf("stdout missing:\n%s", out)
	}
	if !strings.Contains(out, "the error") {
		t.Errorf("stderr missing; a failing command's reason would be dropped:\n%s", out)
	}
}

// A result carrying no usable output must render nothing at all, not an
// empty "Tool Result" heading followed by an empty code fence.
func TestToolResult_NoUsableOutputRendersNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		turn models.Turn
	}{
		{"all fields empty", toolResultTurn(t, map[string]any{"output": ""})},
		{"no data key", models.Turn{Type: "tool_result", RawJSON: json.RawMessage(`{"toolUseId":"tu-1"}`)}},
		{"no raw json", models.Turn{Type: "tool_result"}},
		{"unparseable raw json", models.Turn{Type: "tool_result", RawJSON: json.RawMessage(`{broken`)}},
		{"data is not an object", models.Turn{Type: "tool_result", RawJSON: json.RawMessage(`{"data":"a string"}`)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := renderTurn(t, NewMarkdownExporter(), tc.turn)
			if out != "" {
				t.Errorf("expected no output, got:\n%s", out)
			}
		})
	}
}

// WithToolResults(false) must suppress the whole turn.
func TestToolResult_SuppressedByTheOption(t *testing.T) {
	turn := toolResultTurn(t, map[string]any{"output": "should not appear"})

	out := renderTurn(t, NewMarkdownExporter(WithToolResults(false)), turn)
	if strings.Contains(out, "should not appear") {
		t.Errorf("WithToolResults(false) still rendered the result:\n%s", out)
	}

	on := renderTurn(t, NewMarkdownExporter(WithToolResults(true)), turn)
	if !strings.Contains(on, "should not appear") {
		t.Errorf("WithToolResults(true) dropped the result:\n%s", on)
	}
}

// Long results are capped at 100 lines. Both sides of the boundary again.
func TestToolResult_TruncatesOnlyPast100Lines(t *testing.T) {
	lines := make([]string, 100)
	for i := range lines {
		lines[i] = fmt.Sprintf("out %d", i)
	}

	at := renderTurn(t, NewMarkdownExporter(),
		toolResultTurn(t, map[string]any{"output": strings.Join(lines, "\n")}))
	if strings.Contains(at, "more lines") {
		t.Errorf("exactly 100 lines was truncated:\n%s", at)
	}
	if !strings.Contains(at, "out 99") {
		t.Errorf("the 100th line is missing:\n%s", at)
	}

	over := renderTurn(t, NewMarkdownExporter(),
		toolResultTurn(t, map[string]any{"output": strings.Join(append(lines, "out 100"), "\n")}))
	if !strings.Contains(over, "... (1 more lines)") {
		t.Errorf("101 lines did not report exactly 1 dropped line:\n%s", over)
	}
}

// --- turn routing -------------------------------------------------------

// exportTurn routes on Type, and a type it does not know must be skipped
// rather than rendered as one of the others. Note the type is "human", the
// value a vacuous fixture in this repo used where real transcripts say
// "user" (#107) -- so this also pins that "human" is NOT a user turn.
func TestExportTurn_UnknownTypeIsSkipped(t *testing.T) {
	for _, typ := range []string{"human", "system", ""} {
		t.Run(typ, func(t *testing.T) {
			out := renderTurn(t, NewMarkdownExporter(), models.Turn{
				ID:      "turn-x",
				Type:    typ,
				Content: "content-should-not-render",
			})
			if out != "" {
				t.Errorf("turn type %q produced output:\n%s", typ, out)
			}
		})
	}
}
