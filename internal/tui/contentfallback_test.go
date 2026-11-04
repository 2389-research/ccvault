// ABOUTME: Tests that the conversation view renders a turn of a type it has no case for when it carries content, and skips it when it does not.
// ABOUTME: Real SQLite in a temp dir; the fixtures are the shapes behind the archive's 51,794 nanoclaw system turns and ~169,000 machinery turns.

package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/2389-research/ccvault/internal/db"
	"github.com/2389-research/ccvault/pkg/models"
)

// scheduledTaskPrompt is the start of a real nanoclaw scheduled-task prompt.
// The nanoclaw adapter retags such a turn from `user` to `system` because its
// own scheduler injected it, so the turn's raw_json still says `"type":"user"`
// while the stored type is `system`.
//
// There are 51,834 of these in the author's archive and 51,794 carry content
// like this — each a 1,000+ character agent prompt (max 6,433, 424 distinct)
// explaining why the session did anything. With no case for `system`, a
// nanoclaw conversation read as though the agent started acting for no reason.
const scheduledTaskPrompt = "[SCHEDULED TASK - The following message was sent automatically " +
	"and is not coming directly from the user or group.] " +
	"You are Hal, a homelab manager. Your job is to clean apt cache and check disk usage."

// nanoclawSessionDB builds a nanoclaw session holding the scheduled-task
// `system` turn that starts it and the assistant turn that answers.
func nanoclawSessionDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	ts := time.Date(2026, 8, 14, 19, 25, 27, 0, time.UTC)
	if err := database.UpsertSession(&models.Session{
		ID:        "sess-nanoclaw",
		StartedAt: ts,
		SourceFile: "/Users/x/work/tools/nanoclaw/data/sessions/hal/.claude/projects/" +
			"-workspace-group/00591c63-4a59-425c-b80d-9e2a8c8778d8.jsonl",
		Source: "nanoclaw",
	}); err != nil {
		t.Fatalf("upsert session: %v", err)
	}

	if err := database.InsertTurns([]models.Turn{
		{
			ID: "turn-sched", SessionID: "sess-nanoclaw", Type: "system", Timestamp: ts, Ordinal: 0,
			Content: scheduledTaskPrompt,
			RawJSON: []byte(`{"parentUuid":null,"isSidechain":false,` +
				`"promptId":"72363906-aace-41ef-9c83-91d977c8fc7c","type":"user",` +
				`"message":{"role":"user","content":"a prompt the view must not read from here"}}`),
		},
		{
			ID: "turn-reply", SessionID: "sess-nanoclaw", Type: "assistant", Timestamp: ts, Ordinal: 1,
			Content: "Checking disk usage on every container.",
			RawJSON: []byte(`{"message":{"content":[{"type":"text",` +
				`"text":"Checking disk usage on every container."}]}}`),
		},
	}); err != nil {
		t.Fatalf("insert turns: %v", err)
	}

	return database
}

// machinerySessionDB builds a session of nothing but the types the view has no
// case for that carry nothing to show. Real raw_json, real counts from the
// author's archive: 131,581 claude-code progress turns, 26,971 attachment
// turns and 6,396 claude-code system turns, every one with an empty content
// column.
func machinerySessionDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	ts := time.Date(2026, 8, 14, 19, 25, 27, 0, time.UTC)
	if err := database.UpsertSession(&models.Session{
		ID: "sess-machinery", StartedAt: ts, SourceFile: "/tmp/sess-machinery.jsonl", Source: "claude-code",
	}); err != nil {
		t.Fatalf("upsert session: %v", err)
	}

	if err := database.InsertTurns([]models.Turn{
		{
			ID: "turn-prog", SessionID: "sess-machinery", Type: "progress", Timestamp: ts, Ordinal: 0,
			RawJSON: []byte(`{"parentUuid":null,"isSidechain":false,"type":"progress",` +
				`"data":{"type":"hook_progress","hookEvent":"PreToolUse"}}`),
		},
		{
			ID: "turn-attach", SessionID: "sess-machinery", Type: "attachment", Timestamp: ts, Ordinal: 1,
			RawJSON: []byte(`{"parentUuid":null,"isSidechain":false,` +
				`"attachment":{"type":"deferred_tools_delta","addedNames":["CronCreate"]}}`),
		},
		{
			ID: "turn-cc-system", SessionID: "sess-machinery", Type: "system", Timestamp: ts, Ordinal: 2,
			RawJSON: []byte(`{"parentUuid":null,"isSidechain":false,"type":"system",` +
				`"subtype":"turn_duration","durationMs":15943,"messageCount":20}`),
		},
	}); err != nil {
		t.Fatalf("insert turns: %v", err)
	}

	return database
}

// TestConversation_SystemTurnWithContentRenders is what recovers the 51,794
// nanoclaw system turns. The view's switch had a case for three types and no
// default, so the prompt that started the session was dropped silently and the
// agent's first action arrived unexplained.
//
// Nothing in the fix special-cases `system`: see
// TestConversation_TheRuleIsContentNotTypeName below for the rule itself.
func TestConversation_SystemTurnWithContentRenders(t *testing.T) {
	m := loadConversationFor(t, nanoclawSessionDB(t), "sess-nanoclaw")

	rendered := m.renderConversation()
	visible := visibleText(rendered)

	if !strings.Contains(visible, "You are Hal, a homelab manager") {
		t.Errorf("the scheduled-task prompt did not render:\n%s", rendered)
	}
	// Labelled with the type it is, so a reader can tell an injected prompt
	// from something a human typed.
	if !strings.Contains(visible, "[SYSTEM]") {
		t.Errorf("the turn rendered unlabelled:\n%s", rendered)
	}
	// The assistant turn beside it still renders, so the new branch did not
	// swallow the conversation it sits in.
	if !strings.Contains(visible, "Checking disk usage on every container") {
		t.Errorf("the assistant reply stopped rendering:\n%s", rendered)
	}
}

// TestConversation_MachineryTurnsWithNoContentRenderNothing is the other half
// of the rule, and the reason the rule is worth having: ~169,000 turns in the
// author's archive are machinery with an empty content column. A default
// branch that drew a header for every one of them would bury the 51,794 that
// say something.
//
// Asserted on the whole view being empty rather than on the absence of a
// string, because an empty-content turn has no string to look for.
func TestConversation_MachineryTurnsWithNoContentRenderNothing(t *testing.T) {
	m := loadConversationFor(t, machinerySessionDB(t), "sess-machinery")

	if rendered := m.renderConversation(); strings.TrimSpace(stripANSI(rendered)) != "" {
		t.Errorf("a session of nothing but machinery turns rendered:\n%q", rendered)
	}
}

// TestConversation_TheRuleIsContentNotTypeName pins the generality. A type no
// adapter in the tree writes renders when it carries text and stays quiet when
// it does not, which is the whole point of keying on content: a future source's
// new type should render if it carries text and stay quiet if it doesn't,
// without anyone having to come back here and add a case.
func TestConversation_TheRuleIsContentNotTypeName(t *testing.T) {
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	ts := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	if err := database.UpsertSession(&models.Session{
		ID: "sess-future-type", StartedAt: ts, SourceFile: "/tmp/sess-future-type.jsonl",
		Source: "a-source-not-yet-written",
	}); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	if err := database.InsertTurns([]models.Turn{
		{
			ID: "turn-speaks", SessionID: "sess-future-type", Type: "handoff", Timestamp: ts, Ordinal: 0,
			Content: "handing the task to the review agent because the build is green",
		},
		{
			ID: "turn-silent", SessionID: "sess-future-type", Type: "heartbeat", Timestamp: ts, Ordinal: 1,
		},
	}); err != nil {
		t.Fatalf("insert turns: %v", err)
	}

	m := loadConversationFor(t, database, "sess-future-type")
	visible := visibleText(m.renderConversation())

	if !strings.Contains(visible, "handing the task to the review agent") {
		t.Errorf("a type no adapter writes lost its content:\n%s", visible)
	}
	if strings.Contains(visible, "HEARTBEAT") {
		t.Errorf("a type with no content drew a header anyway:\n%s", visible)
	}
}

// TestConversation_ClipboardFollowsTheSameRule covers the second switch in
// this file. Copying a conversation used to drop exactly the turns rendering
// dropped, so pasting a nanoclaw session into a bug report lost the prompt
// that explained it — and it dropped them for a different reason, having no
// default of its own.
func TestConversation_ClipboardFollowsTheSameRule(t *testing.T) {
	withPrompt := loadConversationFor(t, nanoclawSessionDB(t), "sess-nanoclaw")
	text := withPrompt.plainTextTranscript()

	if !strings.Contains(text, "You are Hal, a homelab manager") {
		t.Errorf("the scheduled-task prompt was not copied:\n%s", text)
	}
	if !strings.Contains(text, "[SYSTEM]") {
		t.Errorf("the copied turn carries no label:\n%s", text)
	}
	if !strings.Contains(text, "Checking disk usage on every container") {
		t.Errorf("the assistant reply stopped being copied:\n%s", text)
	}

	machinery := loadConversationFor(t, machinerySessionDB(t), "sess-machinery")
	if got := strings.TrimSpace(machinery.plainTextTranscript()); got != "" {
		t.Errorf("machinery turns with no content were copied:\n%q", got)
	}
}
