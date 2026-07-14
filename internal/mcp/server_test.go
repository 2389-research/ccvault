// ABOUTME: Tests for MCP server tool handlers
// ABOUTME: Exercises handlers against a real temporary SQLite database

package mcp

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/2389-research/ccvault/internal/db"
	"github.com/2389-research/ccvault/pkg/models"
)

// newTestServer returns a Server backed by a real temp SQLite database.
// cfg and analyzer stay nil: handlers under test only touch s.db.
func newTestServer(t *testing.T) (*Server, *db.DB) {
	t.Helper()

	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	return &Server{db: database}, database
}

// seedSession inserts a session with one user turn. projectID may point at
// a project that does not exist — the schema has no FK constraints, which
// is exactly the integrity gap the warnings surface.
func seedSession(t *testing.T, database *db.DB, sessionID string, projectID int64) {
	t.Helper()

	s := &models.Session{
		ID:         sessionID,
		ProjectID:  projectID,
		StartedAt:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		SourceFile: "/tmp/" + sessionID + ".jsonl",
	}
	if err := database.UpsertSession(s); err != nil {
		t.Fatalf("upsert session %s: %v", sessionID, err)
	}

	turns := []models.Turn{{
		ID:        sessionID + "-turn-1",
		SessionID: sessionID,
		Type:      "user",
		Timestamp: time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC),
		Content:   "hello from " + sessionID,
	}}
	if err := database.InsertTurns(turns); err != nil {
		t.Fatalf("insert turns for %s: %v", sessionID, err)
	}
}

func seedProject(t *testing.T, database *db.DB, path string) *models.Project {
	t.Helper()

	p := &models.Project{Path: path, DisplayName: path}
	if err := database.UpsertProject(p); err != nil {
		t.Fatalf("upsert project %s: %v", path, err)
	}
	return p
}

func TestSearchConversations_EmptyResultsIncludeHint(t *testing.T) {
	s, database := newTestServer(t)
	p := seedProject(t, database, "/test/proj")
	seedSession(t, database, "session-1", p.ID)
	toolUses := []models.ToolUse{{
		TurnID:    "session-1-turn-1",
		SessionID: "session-1",
		ToolName:  "mcp__ccvault__search_conversations",
		Timestamp: time.Now(),
	}}
	if err := database.InsertToolUses(toolUses); err != nil {
		t.Fatalf("insert tool uses: %v", err)
	}

	// Fragment does not full-name match, so the search comes back empty
	result, err := s.searchConversations(map[string]interface{}{"query": "tool:ccvault"})
	if err != nil {
		t.Fatalf("searchConversations: %v", err)
	}

	m := result.(map[string]interface{})
	if m["count"].(int) != 0 {
		t.Fatalf("count = %v, want 0", m["count"])
	}
	hint, _ := m["hint"].(string)
	if hint == "" {
		t.Error("empty result should include a hint")
	}
	similar, _ := m["similar_tool_names"].([]string)
	if len(similar) != 1 || similar[0] != "mcp__ccvault__search_conversations" {
		t.Errorf("similar_tool_names = %v, want [mcp__ccvault__search_conversations]", similar)
	}
}

func TestSearchConversations_ResultsHaveNoHint(t *testing.T) {
	s, database := newTestServer(t)
	p := seedProject(t, database, "/test/proj")
	seedSession(t, database, "session-1", p.ID)

	result, err := s.searchConversations(map[string]interface{}{"query": "hello"})
	if err != nil {
		t.Fatalf("searchConversations: %v", err)
	}

	m := result.(map[string]interface{})
	if m["count"].(int) == 0 {
		t.Fatal("expected results for 'hello'")
	}
	if _, present := m["hint"]; present {
		t.Errorf("non-empty result should not carry a hint, got %v", m["hint"])
	}
}

func TestListSessions_ReportsHasMore(t *testing.T) {
	s, database := newTestServer(t)
	p := seedProject(t, database, "/test/proj")
	for i := 1; i <= 3; i++ {
		seedSession(t, database, fmt.Sprintf("session-%d", i), p.ID)
	}

	result, err := s.listSessions(map[string]interface{}{"limit": float64(2)})
	if err != nil {
		t.Fatalf("listSessions: %v", err)
	}
	m := result.(map[string]interface{})
	if m["count"].(int) != 2 {
		t.Errorf("count = %v, want 2", m["count"])
	}
	if m["has_more"] != true {
		t.Error("expected has_more=true when sessions exceed limit")
	}
	if hint, _ := m["hint"].(string); hint == "" {
		t.Error("truncated list should include a hint")
	}

	all, err := s.listSessions(map[string]interface{}{"limit": float64(100)})
	if err != nil {
		t.Fatalf("listSessions all: %v", err)
	}
	mAll := all.(map[string]interface{})
	if _, present := mAll["has_more"]; present {
		t.Error("has_more should be absent when everything fit")
	}
}

func TestListProjects_ReportsHasMoreAndClampsLimit(t *testing.T) {
	s, database := newTestServer(t)
	seedProject(t, database, "/test/proj-a")
	seedProject(t, database, "/test/proj-b")

	result, err := s.listProjects(map[string]interface{}{"limit": float64(1)})
	if err != nil {
		t.Fatalf("listProjects: %v", err)
	}
	m := result.(map[string]interface{})
	if m["count"].(int) != 1 {
		t.Errorf("count = %v, want 1", m["count"])
	}
	if m["has_more"] != true {
		t.Error("expected has_more=true when projects exceed limit")
	}

	// limit 0 must fall back to the default, not dump unbounded or return nothing
	zero, err := s.listProjects(map[string]interface{}{"limit": float64(0)})
	if err != nil {
		t.Fatalf("listProjects limit 0: %v", err)
	}
	mZero := zero.(map[string]interface{})
	if mZero["count"].(int) != 2 {
		t.Errorf("limit 0: count = %v, want 2 (default limit applied)", mZero["count"])
	}
	if _, present := mZero["has_more"]; present {
		t.Error("limit 0: has_more should be absent when everything fit")
	}
}

func TestGetSessionSummary_WarnsWhenProjectMissing(t *testing.T) {
	s, database := newTestServer(t)
	// No project 9999 exists — a dangling reference the schema permits
	seedSession(t, database, "session-orphan", 9999)

	result, err := s.getSessionSummary(map[string]interface{}{"session_id": "session-orphan"})
	if err != nil {
		t.Fatalf("getSessionSummary: %v", err)
	}

	m := result.(map[string]interface{})
	warnings, ok := m["warnings"].([]string)
	if !ok || len(warnings) == 0 {
		t.Fatalf("expected warnings about missing project, got %#v", m["warnings"])
	}
	if !strings.Contains(warnings[0], "9999") {
		t.Errorf("warning should name the missing project id, got %q", warnings[0])
	}
}

func TestGetSessionSummary_NoWarningsWhenProjectExists(t *testing.T) {
	s, database := newTestServer(t)
	p := seedProject(t, database, "/test/proj")
	seedSession(t, database, "session-ok", p.ID)

	result, err := s.getSessionSummary(map[string]interface{}{"session_id": "session-ok"})
	if err != nil {
		t.Fatalf("getSessionSummary: %v", err)
	}

	m := result.(map[string]interface{})
	if _, present := m["warnings"]; present {
		t.Errorf("healthy session should have no warnings, got %#v", m["warnings"])
	}
	if m["project_path"] != "/test/proj" {
		t.Errorf("project_path = %v, want /test/proj", m["project_path"])
	}
}

func TestGetSession_WarnsWhenProjectMissing(t *testing.T) {
	s, database := newTestServer(t)
	seedSession(t, database, "session-orphan", 9999)

	result, err := s.getSession(map[string]interface{}{"session_id": "session-orphan"})
	if err != nil {
		t.Fatalf("getSession: %v", err)
	}

	m := result.(map[string]interface{})
	warnings, ok := m["warnings"].([]string)
	if !ok || len(warnings) == 0 {
		t.Fatalf("expected warnings about missing project, got %#v", m["warnings"])
	}
}

func TestGetAnalytics_ReportsUnavailableAnalytics(t *testing.T) {
	s, _ := newTestServer(t)
	s.analyzerErr = errors.New("duckdb cache missing")

	result, err := s.getAnalytics(map[string]interface{}{})
	if err != nil {
		t.Fatalf("getAnalytics: %v", err)
	}

	m := result.(map[string]interface{})
	info, ok := m["analytics"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected analytics availability object, got %#v", m["analytics"])
	}
	if info["available"] != false {
		t.Errorf("available = %v, want false", info["available"])
	}
	if reason, _ := info["reason"].(string); !strings.Contains(reason, "duckdb cache missing") {
		t.Errorf("reason should carry the init error, got %q", info["reason"])
	}
	if hint, _ := info["hint"].(string); !strings.Contains(hint, "build-cache") {
		t.Errorf("hint should point at 'ccvault build-cache', got %q", info["hint"])
	}
}

func TestGetAnalytics_PropagatesSummaryWarnings(t *testing.T) {
	s, database := newTestServer(t)

	// Break only an enrichment query so getStats degrades (warns) instead of
	// hard-failing. Dropping tool_uses makes GetToolUsageStats fail while
	// the core session/project/token queries still succeed on empty tables.
	if _, err := database.Exec("DROP TABLE tool_uses"); err != nil {
		t.Fatalf("drop tool_uses: %v", err)
	}

	result, err := s.getAnalytics(map[string]interface{}{})
	if err != nil {
		t.Fatalf("getAnalytics: %v", err)
	}

	m := result.(map[string]interface{})
	summary, ok := m["summary"].(map[string]interface{})
	if !ok {
		t.Fatalf("result[summary] is not a map, got %#v", m["summary"])
	}
	warnings, ok := summary["warnings"].([]string)
	if !ok || len(warnings) == 0 {
		t.Fatalf("summary[warnings] should be a non-empty []string, got %#v", summary["warnings"])
	}
	if !strings.Contains(warnings[0], "tool") {
		t.Errorf("warning should mention tool stats, got %q", warnings[0])
	}
}

func TestGetStats_HealthyDBHasNoWarnings(t *testing.T) {
	s, database := newTestServer(t)
	p := seedProject(t, database, "/test/proj")
	seedSession(t, database, "session-1", p.ID)

	result, err := s.getStats(nil)
	if err != nil {
		t.Fatalf("getStats: %v", err)
	}

	m := result.(map[string]interface{})
	if _, present := m["warnings"]; present {
		t.Errorf("healthy db should have no warnings, got %#v", m["warnings"])
	}
}
