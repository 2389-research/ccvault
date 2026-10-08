// ABOUTME: Tests that the conversation view marks the tool calls that failed, from the stored is_error column (#83).
// ABOUTME: Real SQLite in a temp dir; the two calls differ only in whether their result reported a failure.

package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/2389-research/ccvault/internal/db"
	"github.com/2389-research/ccvault/pkg/models"
)

// toolErrorTestDB builds a session whose one assistant turn made two calls:
// a Bash that worked and an Edit that did not. Both render identically today,
// which is the problem — a reader scrolling a long session has nothing to look
// for.
func toolErrorTestDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	ts := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if err := database.UpsertSession(&models.Session{
		ID: "sess-tui-e", StartedAt: ts, SourceFile: "/tmp/sess-tui-e.jsonl", Source: "claude-code",
	}); err != nil {
		t.Fatalf("upsert session: %v", err)
	}

	if err := database.InsertTurns([]models.Turn{{
		ID: "turn-tui-e", SessionID: "sess-tui-e", Type: "assistant", Timestamp: ts, Ordinal: 0,
		Content: "[Tool: Bash]",
		RawJSON: []byte(`{"uuid":"turn-tui-e","type":"assistant","message":{"role":"assistant","content":[` +
			`{"type":"tool_use","id":"toolu_TUI_OK","name":"Bash","input":{"command":"go build ./..."}},` +
			`{"type":"tool_use","id":"toolu_TUI_FAIL","name":"Edit","input":{"file_path":"/tmp/x.go"}}]}}`),
	}}); err != nil {
		t.Fatalf("insert turns: %v", err)
	}

	yes, no := true, false
	if err := database.InsertToolUses([]models.ToolUse{
		{TurnID: "turn-tui-e", SessionID: "sess-tui-e", ToolName: "Bash", Timestamp: ts,
			ToolUseID: "toolu_TUI_OK", HasResult: true, ResultContent: "ok", ResultLength: 2, IsError: &no},
		{TurnID: "turn-tui-e", SessionID: "sess-tui-e", ToolName: "Edit", Timestamp: ts,
			ToolUseID: "toolu_TUI_FAIL", HasResult: true,
			ResultContent: "String to replace not found in file", ResultLength: 34, IsError: &yes},
	}); err != nil {
		t.Fatalf("insert tool uses: %v", err)
	}

	return database
}

// loadConversationFor runs the view's own load command and applies the message
// it produces, so the test exercises the same path the TUI does rather than
// setting the model's fields by hand.
func loadConversationFor(t *testing.T, database *db.DB, sessionID string) *ConversationModel {
	t.Helper()
	m := NewConversationModel(database)
	m.SetSession(sessionID)
	m.width, m.height = 100, 40

	msg := m.loadConversation()
	if err, isErr := msg.(ErrorMsg); isErr {
		t.Fatalf("loadConversation: %v", err.Err)
	}
	loaded, ok := msg.(conversationLoadedMsg)
	if !ok {
		t.Fatalf("loadConversation returned %T, want conversationLoadedMsg", msg)
	}
	m.Update(loaded)
	return m
}

// TestConversation_MarksTheFailedCall is the TUI half of issue #83: a reader
// scrolling a session can see which call broke instead of reading every
// result for words that look like an error.
func TestConversation_MarksTheFailedCall(t *testing.T) {
	m := loadConversationFor(t, toolErrorTestDB(t), "sess-tui-e")

	rendered := m.renderConversation()

	if !strings.Contains(rendered, failedCallMarker) {
		t.Fatalf("no failure marker in the rendered conversation:\n%s", rendered)
	}

	// The marker has to be on the call that failed, not merely present
	// somewhere in the turn. Both calls render in one block, so a marker
	// attached to the turn would be worse than none: it would point a reader
	// at the Bash that worked.
	editLine, bashLine := "", ""
	for _, line := range strings.Split(rendered, "\n") {
		if strings.Contains(line, "Edit") {
			editLine = line
		}
		if strings.Contains(line, "Bash") && !strings.Contains(line, "[Tool:") {
			bashLine = line
		}
	}
	if editLine == "" || bashLine == "" {
		t.Fatalf("could not find both tool lines in:\n%s", rendered)
	}
	if !strings.Contains(editLine, failedCallMarker) {
		t.Errorf("the Edit call is not marked as failed: %q", editLine)
	}
	if strings.Contains(bashLine, failedCallMarker) {
		t.Errorf("the Bash call that succeeded is marked as failed: %q", bashLine)
	}
}

// TestConversation_UnmarkedWhenNothingFailed keeps a clean session clean. A
// marker that appears on sessions with nothing wrong in them is a marker a
// reader stops seeing.
func TestConversation_UnmarkedWhenNothingFailed(t *testing.T) {
	database := toolErrorTestDB(t)
	if _, err := database.Exec(`UPDATE tool_uses SET is_error = 0 WHERE tool_use_id = 'toolu_TUI_FAIL'`); err != nil {
		t.Fatalf("clear the flag: %v", err)
	}

	m := loadConversationFor(t, database, "sess-tui-e")
	if rendered := m.renderConversation(); strings.Contains(rendered, failedCallMarker) {
		t.Errorf("a session with no failed calls still renders a failure marker:\n%s", rendered)
	}
}

// TestConversation_UnansweredCallIsNotMarked holds the NULL end of the
// convention in the view. An interrupted call did not fail, and marking it
// would send a reader looking for an error nobody reported.
func TestConversation_UnansweredCallIsNotMarked(t *testing.T) {
	database := toolErrorTestDB(t)
	if _, err := database.Exec(`UPDATE tool_uses SET is_error = NULL, result_length = NULL WHERE tool_use_id = 'toolu_TUI_FAIL'`); err != nil {
		t.Fatalf("clear the flag: %v", err)
	}

	m := loadConversationFor(t, database, "sess-tui-e")
	if rendered := m.renderConversation(); strings.Contains(rendered, failedCallMarker) {
		t.Errorf("an unanswered call is rendered as a failure:\n%s", rendered)
	}
}
