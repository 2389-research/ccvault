// ABOUTME: Tests that the conversation view renders an assistant turn's tool calls from tool_uses, whatever source wrote the turn (#179).
// ABOUTME: Real SQLite in a temp dir; the codex fixtures are the shapes a real rollout stores, copied out of ~/.codex/sessions.

package tui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/2389-research/ccvault/internal/db"
	"github.com/2389-research/ccvault/pkg/models"
)

// visibleText is the rendered view as a reader sees it: ANSI escapes removed
// and every run of whitespace collapsed to one space.
//
// Needed because the markdown renderer styles and wraps word by word, so a
// sentence arrives as "…the skills\x1b[0m\x1b[38;5;252m directory." — an
// assertion on the raw string tests glamour's chunking rather than whether the
// sentence rendered.
func visibleText(rendered string) string {
	return strings.Join(strings.Fields(stripANSI(rendered)), " ")
}

// codexProse is what the assistant said, as codex stores it: the text lives in
// a `payload.content` block of type `output_text`, under no `message` object at
// all. Copied from
// ~/.codex/sessions/2026/03/11/rollout-2026-03-11T16-14-54-*.jsonl.
const codexProse = "The install file says to clone `obra/superpowers`, create a symlink, and restart Codex."

// codexRawJSON is one real codex assistant line, trimmed to the fields the
// view reads. Note what it does not have: a top-level `message`, and any block
// type the view's switch knows. That is the whole defect — it parses perfectly
// and yields nothing.
const codexRawJSON = `{"timestamp":"2026-03-11T16:16:01.457Z","type":"response_item","payload":{` +
	`"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + codexProse + `"}],` +
	`"phase":"commentary"}}`

// codexConversationDB builds a one-turn codex session shaped the way the codex
// adapter stores one: prose in the `content` column, the calls in tool_uses
// attached to the assistant turn that issued them, and is_error NULL because
// codex's function_call_output carries no error flag.
func codexConversationDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	ts := time.Date(2026, 3, 11, 16, 16, 1, 0, time.UTC)
	if err := database.UpsertSession(&models.Session{
		ID:         "sess-codex",
		StartedAt:  ts,
		SourceFile: "/Users/x/.codex/sessions/2026/03/11/rollout-x.jsonl",
		Source:     "codex",
	}); err != nil {
		t.Fatalf("upsert session: %v", err)
	}

	if err := database.InsertTurns([]models.Turn{{
		ID: "sess-codex-turn-4", SessionID: "sess-codex", Type: "assistant", Timestamp: ts, Ordinal: 4,
		Content: codexProse,
		RawJSON: []byte(codexRawJSON),
	}}); err != nil {
		t.Fatalf("insert turns: %v", err)
	}

	// Two calls, in the order codex recorded them. Real argument payloads:
	// exec_command's is JSON, apply_patch's is the plain-text diff body that
	// codex's custom_tool_call carries instead of JSON.
	if err := database.InsertToolUses([]models.ToolUse{
		{TurnID: "sess-codex-turn-4", SessionID: "sess-codex", ToolName: "exec_command", Timestamp: ts,
			ToolUseID: "call_abc123",
			InputJSON: `{"cmd":"ls -la /Users/x/.agents/skills","yield_time_ms":1000}`,
			HasResult: true, ResultContent: "total 0", ResultLength: 7},
		{TurnID: "sess-codex-turn-4", SessionID: "sess-codex", ToolName: "apply_patch", Timestamp: ts,
			ToolUseID: "call_def456",
			InputJSON: "*** Begin Patch\n*** Update File: notes.md\n-old\n+new\n*** End Patch",
			HasResult: true, ResultContent: "Success", ResultLength: 7},
	}); err != nil {
		t.Fatalf("insert tool uses: %v", err)
	}

	return database
}

// TestConversation_CodexTurnRendersItsTextAndItsCalls is issue #179's decisive
// case. Every one of the 7,035 codex assistant turns in the author's archive
// renders as a blank line today: the turn's raw_json parses, matches none of
// the three Claude Code block types the view's switch knows, and joins to the
// empty string — while its prose sits in the `content` column and its 33,294
// calls sit in tool_uses.
func TestConversation_CodexTurnRendersItsTextAndItsCalls(t *testing.T) {
	m := loadConversationFor(t, codexConversationDB(t), "sess-codex")

	rendered := m.renderConversation()
	visible := visibleText(rendered)

	// The turn must not be an empty line under its header. Asserted on the
	// text rather than on the length, because the header alone is not blank.
	if !strings.Contains(visible, "install file says to clone") {
		t.Errorf("the assistant's prose did not render:\n%s", rendered)
	}
	for _, toolName := range []string{"exec_command", "apply_patch"} {
		if !strings.Contains(visible, toolName) {
			t.Errorf("the %s call did not render:\n%s", toolName, rendered)
		}
	}
	// The arguments, not just the name — a call rendered without them tells a
	// reader a command ran and not which one.
	if !strings.Contains(visible, "ls -la /Users/x/.agents/skills") {
		t.Errorf("the exec_command arguments did not render:\n%s", rendered)
	}
}

// TestConversation_CodexCallsRenderInTheOrderIssued pins the within-turn order
// on turn_ordinal. A turn that made fifteen calls renders them in the order it
// made them, or a reader cannot follow what it did.
func TestConversation_CodexCallsRenderInTheOrderIssued(t *testing.T) {
	m := loadConversationFor(t, codexConversationDB(t), "sess-codex")

	rendered := visibleText(m.renderConversation())
	exec, patch := strings.Index(rendered, "exec_command"), strings.Index(rendered, "apply_patch")
	if exec < 0 || patch < 0 {
		t.Fatalf("both calls must render before their order means anything:\n%s", rendered)
	}
	if exec > patch {
		t.Errorf("calls rendered out of issue order: exec_command at %d, apply_patch at %d", exec, patch)
	}
}

// TestConversation_UnknownBlockShapeStillRenders is the pin on #179's design
// argument: the view must be right for a source that does not exist yet.
//
// The turn below is written by no adapter in the tree. Its raw_json parses into
// a content block whose type nothing recognises — the shape pi writes
// (`toolCall`) and the shape any future source will write — and the only way to
// render it is to not depend on knowing the shape: prose from the `content`
// column, calls from tool_uses.
//
// If this test ever has to be edited to add a source, the renderer went back to
// being source-aware.
func TestConversation_UnknownBlockShapeStillRenders(t *testing.T) {
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	ts := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	if err := database.UpsertSession(&models.Session{
		ID: "sess-future", StartedAt: ts, SourceFile: "/tmp/sess-future.jsonl", Source: "a-source-not-yet-written",
	}); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	if err := database.InsertTurns([]models.Turn{{
		ID: "turn-future", SessionID: "sess-future", Type: "assistant", Timestamp: ts, Ordinal: 0,
		Content: "I will check the skills directory.",
		RawJSON: []byte(`{"message":{"content":[` +
			`{"type":"prose_fragment","body":"I will check the skills directory."},` +
			`{"type":"toolCall","id":"tc_1","name":"listFiles","arguments":{"path":"/skills"}}]}}`),
	}}); err != nil {
		t.Fatalf("insert turns: %v", err)
	}
	if err := database.InsertToolUses([]models.ToolUse{{
		TurnID: "turn-future", SessionID: "sess-future", ToolName: "listFiles", Timestamp: ts,
		ToolUseID: "tc_1", InputJSON: `{"path":"/skills"}`,
		HasResult: true, ResultContent: "a.md", ResultLength: 4,
	}}); err != nil {
		t.Fatalf("insert tool uses: %v", err)
	}

	m := loadConversationFor(t, database, "sess-future")
	rendered := visibleText(m.renderConversation())

	if !strings.Contains(rendered, "check the skills directory") {
		t.Errorf("an unrecognised block shape lost the turn's text:\n%s", rendered)
	}
	if !strings.Contains(rendered, "listFiles") {
		t.Errorf("an unrecognised block shape lost the turn's tool call:\n%s", rendered)
	}
}

// TestConversation_ClaudeToolOnlyTurnDoesNotRepeatTheStoredSummary guards the
// other side of the fallback. A claude-code turn that only called tools stores
// `content` as ccvault's own summary of those calls — "[Tool: Bash]" — so a
// fallback that fires whenever no *prose* was recognised would print that
// summary immediately above the call it summarises.
//
// The fallback therefore fires on an unrecognised raw_json shape, not on an
// absent prose block: a `tool_use` block is a shape the view knows, and knowing
// it is enough to say the stored summary adds nothing.
func TestConversation_ClaudeToolOnlyTurnDoesNotRepeatTheStoredSummary(t *testing.T) {
	m := loadConversationFor(t, toolErrorTestDB(t), "sess-tui-e")

	rendered := visibleText(m.renderConversation())
	if strings.Contains(rendered, "[Tool: Bash]") {
		t.Errorf("the stored tool summary rendered alongside the call it summarises:\n%s", rendered)
	}
	if !strings.Contains(rendered, "go build ./...") {
		t.Fatalf("the Bash call itself did not render, so the assertion above is vacuous:\n%s", rendered)
	}
}

// TestConversation_MarksAFailedCallWithNoProviderID is the limitation that
// rendering from tool_uses removes rather than works around.
//
// Marking used to need the provider's tool_use id, because the call came out of
// raw_json and the id was the only thing it had in common with the failed-call
// rows. jeff mints no id on 633 of its 742 calls, so those failures could not
// be marked at all. A call read from its own row carries its own is_error.
func TestConversation_MarksAFailedCallWithNoProviderID(t *testing.T) {
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	ts := time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)
	if err := database.UpsertSession(&models.Session{
		ID: "sess-noid", StartedAt: ts, SourceFile: "/tmp/sess-noid.jsonl", Source: "jeff",
	}); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	if err := database.InsertTurns([]models.Turn{{
		ID: "turn-noid", SessionID: "sess-noid", Type: "assistant", Timestamp: ts, Ordinal: 0,
		Content: "Reading the file.",
		RawJSON: []byte(`{"role":"assistant","text":"Reading the file."}`),
	}}); err != nil {
		t.Fatalf("insert turns: %v", err)
	}
	yes := true
	if err := database.InsertToolUses([]models.ToolUse{{
		TurnID: "turn-noid", SessionID: "sess-noid", ToolName: "read_file", Timestamp: ts,
		InputJSON: `{"path":"/missing.txt"}`,
		HasResult: true, ResultContent: "no such file", ResultLength: 12, IsError: &yes,
	}}); err != nil {
		t.Fatalf("insert tool uses: %v", err)
	}

	m := loadConversationFor(t, database, "sess-noid")
	rendered := visibleText(m.renderConversation())

	if !strings.Contains(rendered, "read_file") {
		t.Fatalf("the call did not render, so the marker assertion would be vacuous:\n%s", rendered)
	}
	if !strings.Contains(rendered, failedCallMarker) {
		t.Errorf("a failed call whose source minted no id is not marked:\n%s", rendered)
	}
}

// TestConversation_RendersTextArgumentsVerbatim covers the arguments that are
// not JSON, which on today's archive is most of them: 26,552 of codex's 33,294
// calls store a plain-text body, because codex's custom_tool_call carries one
// instead of a JSON object — 26,531 `exec` calls whose body is a JavaScript
// snippet, and 21 `apply_patch` calls whose body is a diff.
//
// Rendering the name alone for those would answer "a command ran" without
// answering which one, for four calls in five of the source this change is
// about.
func TestConversation_RendersTextArgumentsVerbatim(t *testing.T) {
	m := loadConversationFor(t, codexConversationDB(t), "sess-codex")

	rendered := visibleText(m.renderConversation())
	if !strings.Contains(rendered, "apply_patch") {
		t.Fatalf("the call did not render, so the argument assertion would be vacuous:\n%s", rendered)
	}
	// The real body of a codex apply_patch, which is not JSON and never was.
	if !strings.Contains(rendered, "*** Update File: notes.md") {
		t.Errorf("a call's plain-text arguments did not render:\n%s", rendered)
	}
}

// TestConversation_TruncatesTextArgumentsOnARuneBoundary keeps a 500-byte cut
// from leaving the terminal half a rune to draw. The body below is multi-byte
// throughout, so the cut lands mid-rune wherever it lands.
func TestConversation_TruncatesTextArgumentsOnARuneBoundary(t *testing.T) {
	m := NewConversationModel(nil)
	m.width = 100

	// 400 three-byte runes: 1,200 bytes, and no boundary at byte 500.
	body := strings.Repeat("→", 400)
	rendered := m.formatToolUseBlock("exec", json.RawMessage(body), false)

	if !utf8.ValidString(rendered) {
		t.Errorf("truncation produced invalid UTF-8: %q", rendered)
	}
	if !strings.Contains(rendered, "...") {
		t.Errorf("a 1,200-byte body rendered untruncated: %q", rendered)
	}
}
