// ABOUTME: Tests that markdown export renders an assistant turn's prose and calls whatever source wrote it, the export half of #179.
// ABOUTME: Fixtures are the shapes a real codex rollout stores, copied out of ~/.codex/sessions.

package export

import (
	"strings"
	"testing"
	"time"

	"github.com/2389-research/ccvault/internal/db"
	"github.com/2389-research/ccvault/pkg/models"
)

// codexProse is what the assistant said, as codex stores it: the text lives in
// a `payload.content` block of type `output_text`, under no `message` object at
// all. Copied from
// ~/.codex/sessions/2026/03/11/rollout-2026-03-11T11-14-54-019cddae-01fe-77c0-a805-b273abd1b103.jsonl.
const codexProse = "I'm fetching the install instructions from that URL first, " +
	"then I'll apply them in this workspace and report what changed."

// codexRawJSON is one real codex assistant line, trimmed to the fields export
// reads. Note what it does not have: a top-level `message`, and any block type
// the exporter's switch knows. That is the whole defect — it parses perfectly
// and yields nothing, and the old exporter returned early on a successful
// parse.
const codexRawJSON = `{"timestamp":"2026-03-11T16:15:54.880Z","type":"response_item","payload":{` +
	`"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + codexProse + `"}],` +
	`"phase":"commentary"}}`

// codexAssistantTurn is the turn as the codex adapter stores one: prose in the
// `content` column, raw_json in codex's own shape, and no calls in that
// raw_json because codex records each call on a transcript line of its own.
func codexAssistantTurn() models.Turn {
	return models.Turn{
		ID:        "sess-codex-turn-4",
		SessionID: "sess-codex",
		Type:      "assistant",
		Timestamp: time.Date(2026, 3, 11, 16, 15, 54, 0, time.UTC),
		Ordinal:   4,
		Content:   codexProse,
		RawJSON:   []byte(codexRawJSON),
	}
}

// TestExportAssistant_CodexTurnExportsItsText is the export half of #179's
// decisive case. All 7,035 codex assistant turns in the author's archive
// export with an empty body today: exportAssistantRichContent parses the
// turn's raw_json, matches none of the three Claude Code block types its
// switch knows, writes nothing, and returns a nil error — on which
// exportAssistantTurn returns before reaching the `content` fallback.
//
// The fallback has to fire when parsing succeeds and recognises nothing. A
// fallback keyed on parse failure never fires for codex, because codex's
// raw_json is valid JSON.
func TestExportAssistant_CodexTurnExportsItsText(t *testing.T) {
	out := renderTurn(t, NewMarkdownExporter(), codexAssistantTurn())

	if !strings.Contains(out, "fetching the install instructions") {
		t.Errorf("the assistant's prose did not export:\n%s", out)
	}
}

// codexToolCalls is what the adapter stored for that turn, with real argument
// payloads from the same rollout: exec_command's is JSON, apply_patch's is the
// plain-text diff body codex's custom_tool_call carries instead of JSON.
//
// None of this is in the turn's raw_json, which is why no amount of re-parsing
// raw_json can recover it.
func codexToolCalls(turnID string) map[string][]db.ToolCall {
	return map[string][]db.ToolCall{
		turnID: {
			{ToolName: "exec_command", InputJSON: `{"cmd":"ls -la /Users/dylanr/.agents/skills","yield_time_ms":1000}`},
			{ToolName: "apply_patch", InputJSON: "*** Begin Patch\n*** Update File: notes.md\n-old\n+new\n*** End Patch"},
		},
	}
}

// TestExportAssistant_CodexTurnExportsItsCalls is the other half of the same
// defect. A codex turn's calls live on transcript lines of their own, so
// exporting them from the turn's raw_json exported nothing — for all 33,294
// codex calls in the author's archive.
//
// Both calls are asserted with their arguments, not just their names: a call
// exported without them tells a reader a command ran and not which one. And
// four codex calls in five store arguments that are not JSON at all, so the
// apply_patch case is the common one rather than the exotic one.
func TestExportAssistant_CodexTurnExportsItsCalls(t *testing.T) {
	turn := codexAssistantTurn()
	out := renderTurn(t, NewMarkdownExporter(WithToolCalls(codexToolCalls(turn.ID))), turn)

	for _, want := range []string{
		"🔧 Tool: exec_command",
		"ls -la /Users/dylanr/.agents/skills",
		"🔧 Tool: apply_patch",
		"*** Update File: notes.md",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the export is missing %q:\n%s", want, out)
		}
	}
}

// TestExportAssistant_CodexCallsExportInTheOrderIssued pins the within-turn
// order on the order the grouping arrives in, which db.SessionToolCallsByTurn
// sets from turn_ordinal. A turn that made fifteen calls exports them in the
// order it made them, or a reader cannot follow what it did.
func TestExportAssistant_CodexCallsExportInTheOrderIssued(t *testing.T) {
	turn := codexAssistantTurn()
	out := renderTurn(t, NewMarkdownExporter(WithToolCalls(codexToolCalls(turn.ID))), turn)

	exec, patch := strings.Index(out, "exec_command"), strings.Index(out, "apply_patch")
	if exec < 0 || patch < 0 {
		t.Fatalf("both calls must export before their order means anything:\n%s", out)
	}
	if exec > patch {
		t.Errorf("calls exported out of issue order: exec_command at %d, apply_patch at %d", exec, patch)
	}
}

// TestExportAssistant_ClaudeToolOnlyTurnDoesNotRepeatTheStoredSummary guards
// the other side of the fallback. A claude-code turn that only called tools
// stores `content` as ccvault's own summary of those calls — "[Tool: Bash]" —
// so a fallback that fired whenever no *prose* was recognised would print that
// summary immediately above the call it summarises.
//
// The fallback therefore fires on an unrecognised raw_json shape, not on an
// absent prose block: a `tool_use` block is a shape the exporter knows, and
// knowing it is enough to say the stored summary adds nothing.
func TestExportAssistant_ClaudeToolOnlyTurnDoesNotRepeatTheStoredSummary(t *testing.T) {
	turn := models.Turn{
		ID:        "turn-cc",
		Type:      "assistant",
		Timestamp: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		Content:   "[Tool: Bash]",
		RawJSON: []byte(`{"message":{"content":[` +
			`{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"go build ./..."}}]}}`),
	}
	out := renderTurn(t, NewMarkdownExporter(WithToolCalls(map[string][]db.ToolCall{
		turn.ID: {{ToolName: "Bash", InputJSON: `{"command":"go build ./..."}`}},
	})), turn)

	if !strings.Contains(out, "go build ./...") {
		t.Fatalf("the Bash call itself did not export, so the assertion below is vacuous:\n%s", out)
	}
	if strings.Contains(out, "[Tool: Bash]") {
		t.Errorf("the stored tool summary exported alongside the call it summarises:\n%s", out)
	}
}

// TestExportAssistant_OneCallExportsOnce is the pin on the two halves not
// overlapping. A claude-code turn holds its calls in raw_json *and* in
// tool_uses, and exporting from both would double every call in the archive's
// largest source.
func TestExportAssistant_OneCallExportsOnce(t *testing.T) {
	turn := toolUseTurn(t, "Bash", map[string]any{"command": "go test ./..."})
	out := renderTurn(t, NewMarkdownExporter(WithToolCalls(map[string][]db.ToolCall{
		turn.ID: {{ToolName: "Bash", InputJSON: `{"command":"go test ./..."}`}},
	})), turn)

	if got := strings.Count(out, "🔧 Tool: Bash"); got != 1 {
		t.Errorf("one call exported %d times:\n%s", got, out)
	}
}
