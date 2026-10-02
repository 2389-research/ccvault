// ABOUTME: Tests the MCP listing defaults for subagent sessions and the flags that expand them.
// ABOUTME: Same defaults as the CLI and TUI — one mental model across surfaces.

package mcp

import (
	"testing"
	"time"

	"github.com/2389-research/ccvault/internal/db"
	"github.com/2389-research/ccvault/pkg/models"
)

// seedSubagent inserts a subagent session row linked to parentID.
func seedSubagent(t *testing.T, database *db.DB, id, parentID string, projectID int64) {
	t.Helper()
	s := &models.Session{
		ID:              id,
		ProjectID:       projectID,
		StartedAt:       time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC),
		SourceFile:      "/tmp/" + id + ".jsonl",
		ParentSessionID: parentID,
	}
	if err := database.UpsertSession(s); err != nil {
		t.Fatalf("upsert subagent %s: %v", id, err)
	}
}

func TestListSessions_HidesSubagentsButCountsThem(t *testing.T) {
	s, database := newTestServer(t)
	p := seedProject(t, database, "/test/proj")
	seedSession(t, database, "parent-a", p.ID)
	seedSubagent(t, database, "claude-code:parent-a:agent-a1", "parent-a", p.ID)
	seedSubagent(t, database, "claude-code:parent-a:agent-a2", "parent-a", p.ID)

	m := resultMap(t, mustListSessions(t, s, map[string]interface{}{}))
	if count := mustInt(t, m, "count"); count != 1 {
		t.Errorf("count = %v, want 1 (parents only by default)", count)
	}
	sessions, ok := m["sessions"].([]map[string]any)
	if !ok {
		t.Fatalf("sessions has type %T", m["sessions"])
	}
	if sessions[0]["id"] != "parent-a" {
		t.Errorf("listed %v, want parent-a", sessions[0]["id"])
	}
	if sessions[0]["subagent_count"] != 2 {
		t.Errorf("subagent_count = %v, want 2 — the count is what earns the filtering", sessions[0]["subagent_count"])
	}
}

func TestListSessions_IncludeSubagentsFlattens(t *testing.T) {
	s, database := newTestServer(t)
	p := seedProject(t, database, "/test/proj")
	seedSession(t, database, "parent-a", p.ID)
	seedSubagent(t, database, "claude-code:parent-a:agent-a1", "parent-a", p.ID)

	m := resultMap(t, mustListSessions(t, s, map[string]interface{}{"include_subagents": true}))
	if count := mustInt(t, m, "count"); count != 2 {
		t.Errorf("count = %v, want 2 with include_subagents", count)
	}
}

func TestListSessions_SubagentsOfOneParent(t *testing.T) {
	s, database := newTestServer(t)
	p := seedProject(t, database, "/test/proj")
	seedSession(t, database, "parent-a", p.ID)
	seedSession(t, database, "parent-b", p.ID)
	seedSubagent(t, database, "claude-code:parent-a:agent-a1", "parent-a", p.ID)
	seedSubagent(t, database, "claude-code:parent-b:agent-b1", "parent-b", p.ID)

	m := resultMap(t, mustListSessions(t, s, map[string]interface{}{"subagents_of": "parent-a"}))
	sessions, ok := m["sessions"].([]map[string]any)
	if !ok {
		t.Fatalf("sessions has type %T", m["sessions"])
	}
	if len(sessions) != 1 || sessions[0]["id"] != "claude-code:parent-a:agent-a1" {
		t.Fatalf("subagents_of returned %v, want parent-a's one child", sessions)
	}
	if sessions[0]["parent_session_id"] != "parent-a" {
		t.Errorf("parent_session_id = %v, want parent-a", sessions[0]["parent_session_id"])
	}
}

// TestGetSession_ReachesSubagentWithNoFlag is the "never unreachable" half:
// a subagent id resolves through the same tool as any other session.
func TestGetSession_ReachesSubagentWithNoFlag(t *testing.T) {
	s, database := newTestServer(t)
	p := seedProject(t, database, "/test/proj")
	seedSession(t, database, "parent-a", p.ID)
	seedSubagent(t, database, "claude-code:parent-a:agent-a1", "parent-a", p.ID)
	seedTurns(t, database, "claude-code:parent-a:agent-a1", 2)

	result, err := s.getSession(map[string]interface{}{"session_id": "claude-code:parent-a:agent-a1"})
	if err != nil {
		t.Fatalf("getSession on a subagent id: %v", err)
	}
	m := resultMap(t, result)
	if m["session_id"] != "claude-code:parent-a:agent-a1" {
		t.Errorf("session_id = %v", m["session_id"])
	}

	turns, err := s.getTurns(map[string]interface{}{"session_id": "claude-code:parent-a:agent-a1"})
	if err != nil {
		t.Fatalf("getTurns on a subagent id: %v", err)
	}
	if mustInt(t, resultMap(t, turns), "count") == 0 {
		t.Error("get_turns returned nothing for a subagent session")
	}
}

func mustListSessions(t *testing.T, s *Server, args map[string]interface{}) interface{} {
	t.Helper()
	result, err := s.listSessions(args)
	if err != nil {
		t.Fatalf("listSessions(%v): %v", args, err)
	}
	return result
}
