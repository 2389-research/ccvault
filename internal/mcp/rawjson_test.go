// ABOUTME: Tests that an assistant turn whose raw_json cannot be read says so.
// ABOUTME: Absent tools must not read as "used no tools" when it means "could not tell".

package mcp

import (
	"strings"
	"testing"
	"time"

	"github.com/2389-research/ccvault/internal/db"
	"github.com/2389-research/ccvault/pkg/models"
)

// seedAssistantTurns writes assistant turns with exact raw_json bytes,
// bypassing InsertTurns so a test can store the corrupt and odd-shaped
// blobs the real archive holds. db.GetTurns drops raw_json that is not
// valid JSON, so a corrupt blob reaches the MCP layer as no raw_json at
// all — which is the dominant shape of this failure in practice.
func seedAssistantTurns(t *testing.T, database *db.DB, sessionID string, rawJSON []string) {
	t.Helper()

	session := &models.Session{
		ID:         sessionID,
		StartedAt:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		SourceFile: "/tmp/" + sessionID + ".jsonl",
	}
	if err := database.UpsertSession(session); err != nil {
		t.Fatalf("upsert session %s: %v", sessionID, err)
	}

	ts := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)
	for i, raw := range rawJSON {
		_, err := database.Exec(
			`INSERT INTO turns (id, session_id, type, timestamp, ordinal, content, raw_json)
			 VALUES (?, ?, 'assistant', ?, ?, ?, ?)`,
			sessionID+"-turn-"+string(rune('a'+i)), sessionID, ts, i, "turn body", raw)
		if err != nil {
			t.Fatalf("insert turn %d: %v", i, err)
		}
		ts = ts.Add(time.Second)
	}
}

const (
	// A turn whose raw_json reads cleanly: one tool call, no thinking.
	rawWithToolUse = `{"message":{"content":[{"type":"tool_use","name":"Read"}]}}`
	// Corrupt bytes, as left by syncs predating the oversized-line fix.
	// db.GetTurns drops these, so the MCP layer sees no raw_json.
	rawTruncated = `{"message":{"content":[{"type":"tool_`
	// Valid JSON in a shape the enrichment struct does not fit:
	// message.content is a string rather than an array of blocks.
	rawWrongShape = `{"message":{"content":"a plain string"}}`
)

// TestGetTurns_MarksTurnsWhoseRawJSONCannotBeRead is the honesty case. A
// consumer must be able to tell "this turn used no tools" from "we could
// not read this turn", and the two failing turns here reach the handler
// by the two different routes measured in the real archive.
func TestGetTurns_MarksTurnsWhoseRawJSONCannotBeRead(t *testing.T) {
	s, database := newTestServer(t)
	seedAssistantTurns(t, database, "session-raw", []string{
		rawWithToolUse, rawTruncated, rawWrongShape,
	})

	result, err := s.getTurns(map[string]interface{}{"session_id": "session-raw"})
	if err != nil {
		t.Fatalf("getTurns: %v", err)
	}
	m := resultMap(t, result)

	turns := mustField[[]map[string]interface{}](t, m, "turns")
	if len(turns) != 3 {
		t.Fatalf("got %d turns, want 3", len(turns))
	}

	// The readable turn reports its tools and no unreadable marker.
	if _, marked := turns[0]["raw_unavailable"]; marked {
		t.Error("turn 0 is marked raw_unavailable, but its raw_json parses")
	}
	tools := mustField[[]string](t, turns[0], "tools")
	if len(tools) != 1 || tools[0] != "Read" {
		t.Errorf("turn 0 tools = %#v, want [Read]", tools)
	}
	if mustField[bool](t, turns[0], "has_thinking") {
		t.Error("turn 0 has_thinking = true, want false")
	}

	// Both unreadable turns omit the fields and say why.
	for _, i := range []int{1, 2} {
		if !mustField[bool](t, turns[i], "raw_unavailable") {
			t.Errorf("turn %d raw_unavailable = false, want true", i)
		}
		if _, present := turns[i]["tools"]; present {
			t.Errorf("turn %d reports tools, but its raw_json could not be read", i)
		}
		if _, present := turns[i]["has_thinking"]; present {
			t.Errorf("turn %d reports has_thinking, but its raw_json could not be read", i)
		}
	}

	if got := mustInt(t, m, "unreadable_turns"); got != 2 {
		t.Errorf("unreadable_turns = %d, want 2", got)
	}

	warnings := mustField[[]string](t, m, "warnings")
	if len(warnings) != 1 {
		t.Fatalf("warnings = %#v, want exactly one entry for the whole page", warnings)
	}
	if !strings.Contains(warnings[0], "unavailable") {
		t.Errorf("warning %q does not read as a '<what> unavailable: <why>' entry", warnings[0])
	}
}

// TestGetTurns_CleanPageSaysNothing keeps the new fields off the happy
// path: a page whose turns all read cleanly gains no count, no warning,
// and no per-turn marker.
func TestGetTurns_CleanPageSaysNothing(t *testing.T) {
	s, database := newTestServer(t)
	seedAssistantTurns(t, database, "session-clean", []string{rawWithToolUse, rawWithToolUse})

	result, err := s.getTurns(map[string]interface{}{"session_id": "session-clean"})
	if err != nil {
		t.Fatalf("getTurns: %v", err)
	}
	m := resultMap(t, result)

	if v, present := m["unreadable_turns"]; present {
		t.Errorf("unreadable_turns = %#v on a clean page, want absent", v)
	}
	if v, present := m["warnings"]; present {
		t.Errorf("warnings = %#v on a clean page, want absent", v)
	}
	for i, turn := range mustField[[]map[string]interface{}](t, m, "turns") {
		if v, present := turn["raw_unavailable"]; present {
			t.Errorf("turn %d raw_unavailable = %#v, want absent", i, v)
		}
	}
}

// TestGetTurns_CountsOnlyTheReturnedPage pins the count to the page, not
// to the session. A count covering turns the caller never saw would not
// tell it which of its results to distrust.
func TestGetTurns_CountsOnlyTheReturnedPage(t *testing.T) {
	s, database := newTestServer(t)
	seedAssistantTurns(t, database, "session-page", []string{
		rawWithToolUse, rawTruncated, rawTruncated, rawTruncated,
	})

	result, err := s.getTurns(map[string]interface{}{
		"session_id": "session-page",
		"limit":      float64(2),
	})
	if err != nil {
		t.Fatalf("getTurns: %v", err)
	}
	m := resultMap(t, result)

	if got := len(mustField[[]map[string]interface{}](t, m, "turns")); got != 2 {
		t.Fatalf("got %d turns, want the 2 the limit asked for", got)
	}
	if got := mustInt(t, m, "unreadable_turns"); got != 1 {
		t.Errorf("unreadable_turns = %d, want 1 — only the returned page counts", got)
	}
}

// TestGetTurns_ForeignRawShapeReadsAsNoTools pins a limitation this fix
// does not close, so it is visible in code rather than only in a report.
//
// The enrichment reads message.content[], which is claude-code's and
// nanoclaw's raw shape. The codex, hex and jeff adapters store raw
// entries with no top-level `message` key at all, and Go's json.Unmarshal
// ignores unknown keys — so the parse *succeeds* and reports
// has_thinking: false with no tools, confidently and wrongly. That is a
// false negative the response cannot detect, and it is a different defect
// from the unreadable-raw_json one above: there is no error to notice.
// Closing it needs per-adapter extraction, not a warning.
func TestGetTurns_ForeignRawShapeReadsAsNoTools(t *testing.T) {
	s, database := newTestServer(t)
	seedAssistantTurns(t, database, "session-foreign", []string{
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}`,
	})

	result, err := s.getTurns(map[string]interface{}{"session_id": "session-foreign"})
	if err != nil {
		t.Fatalf("getTurns: %v", err)
	}
	m := resultMap(t, result)
	turns := mustField[[]map[string]interface{}](t, m, "turns")

	if _, marked := turns[0]["raw_unavailable"]; marked {
		t.Error("turn is marked raw_unavailable; a foreign raw shape parses fine, which is the problem")
	}
	if mustField[bool](t, turns[0], "has_thinking") {
		t.Error("has_thinking = true, want the (wrong but current) false")
	}
	if v, present := m["unreadable_turns"]; present {
		t.Errorf("unreadable_turns = %#v; a foreign shape is not detected as unreadable", v)
	}
}

// TestGetSessionSummary_WarnsWhenToolCountsAreIncomplete is the
// response-level half. tools_used aggregates over every assistant turn,
// so unreadable turns undercount it, and an empty tools_used otherwise
// reads as a session that used no tools.
func TestGetSessionSummary_WarnsWhenToolCountsAreIncomplete(t *testing.T) {
	s, database := newTestServer(t)
	seedAssistantTurns(t, database, "session-summary-raw", []string{
		rawWithToolUse, rawTruncated, rawWrongShape,
	})

	result, err := s.getSessionSummary(map[string]interface{}{"session_id": "session-summary-raw"})
	if err != nil {
		t.Fatalf("getSessionSummary: %v", err)
	}
	m := resultMap(t, result)

	if got := mustInt(t, m, "unreadable_turns"); got != 2 {
		t.Errorf("unreadable_turns = %d, want 2", got)
	}
	warnings := mustField[[]string](t, m, "warnings")
	if len(warnings) != 1 {
		t.Fatalf("warnings = %#v, want one entry about tools_used", warnings)
	}
	if !strings.Contains(warnings[0], "tools_used") {
		t.Errorf("warning %q does not name the field it affects", warnings[0])
	}
}

// TestGetSessionSummary_CleanSessionSaysNothing keeps the summary's new
// fields off the happy path too.
func TestGetSessionSummary_CleanSessionSaysNothing(t *testing.T) {
	s, database := newTestServer(t)
	seedAssistantTurns(t, database, "session-summary-clean", []string{rawWithToolUse})

	result, err := s.getSessionSummary(map[string]interface{}{"session_id": "session-summary-clean"})
	if err != nil {
		t.Fatalf("getSessionSummary: %v", err)
	}
	m := resultMap(t, result)

	if v, present := m["unreadable_turns"]; present {
		t.Errorf("unreadable_turns = %#v on a clean session, want absent", v)
	}
	if v, present := m["warnings"]; present {
		t.Errorf("warnings = %#v on a clean session, want absent", v)
	}
}
