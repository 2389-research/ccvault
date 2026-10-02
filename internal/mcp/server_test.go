// ABOUTME: Tests for the MCP JSON-RPC server — notification dispatch (issue #7) and handler behavior.
// ABOUTME: Uses real temp SQLite for handler tests; a bytes.Buffer for dispatch output inspection.

package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/2389-research/ccvault/internal/db"
	"github.com/2389-research/ccvault/pkg/models"
)

// newBufferedServer builds a Server with just enough state to exercise the
// dispatch layer and an in-memory buffer to inspect what bytes are emitted.
// The db is nil because notification handlers don't touch it.
func newBufferedServer(t *testing.T) (*Server, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	return &Server{out: buf}, buf
}

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

// seedTurns appends n user turns to an existing session. Used to push a
// session past get_session's 100-turn inline-markdown limit.
func seedTurns(t *testing.T, database *db.DB, sessionID string, n int) {
	t.Helper()

	turns := make([]models.Turn, n)
	for i := range turns {
		turns[i] = models.Turn{
			ID:        fmt.Sprintf("%s-bulk-%d", sessionID, i),
			SessionID: sessionID,
			Type:      "user",
			Timestamp: time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC).Add(time.Duration(i) * time.Second),
			Content:   fmt.Sprintf("bulk turn %d", i),
		}
	}
	if err := database.InsertTurns(turns); err != nil {
		t.Fatalf("insert %d bulk turns for %s: %v", n, sessionID, err)
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

// seedToolUse records one tool invocation against a session's first turn,
// which is what gives GetToolNamesLike something to match on.
func seedToolUse(t *testing.T, database *db.DB, sessionID, toolName string) {
	t.Helper()

	uses := []models.ToolUse{{
		TurnID:    sessionID + "-turn-1",
		SessionID: sessionID,
		ToolName:  toolName,
		Timestamp: time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC),
	}}
	if err := database.InsertToolUses(uses); err != nil {
		t.Fatalf("insert tool uses for %s: %v", sessionID, err)
	}
}

// narrowProjectsTable replaces the projects table with a two-column view of
// the same rows. Search still resolves (it reads only projects.id and
// projects.path) while GetProjects fails on the columns the view drops —
// the precise shape of a degraded enrichment query, with no mocking.
func narrowProjectsTable(t *testing.T, database *db.DB) {
	t.Helper()

	for _, stmt := range []string{
		`ALTER TABLE projects RENAME TO projects_full`,
		`CREATE VIEW projects AS SELECT id, path FROM projects_full`,
	} {
		if _, err := database.Exec(stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
}

func TestSearchConversations_EmptyResultsIncludeHint(t *testing.T) {
	s, database := newTestServer(t)
	p := seedProject(t, database, "/test/proj")
	seedSession(t, database, "session-1", p.ID)
	seedToolUse(t, database, "session-1", "mcp__ccvault__search_conversations")

	// Fragment does not full-name match, so the search comes back empty
	result, err := s.searchConversations(map[string]interface{}{"query": "tool:ccvault"})
	if err != nil {
		t.Fatalf("searchConversations: %v", err)
	}

	m := resultMap(t, result)
	if count := mustInt(t, m, "count"); count != 0 {
		t.Fatalf("count = %v, want 0", count)
	}
	hint, _ := m["hint"].(string)
	if hint == "" {
		t.Error("empty result should include a hint")
	}
	similar := mustField[[]string](t, m, "similar_tool_names")
	if len(similar) != 1 || similar[0] != "mcp__ccvault__search_conversations" {
		t.Errorf("similar_tool_names = %v, want [mcp__ccvault__search_conversations]", similar)
	}
	if _, present := m["warnings"]; present {
		t.Errorf("healthy lookup should emit no warnings, got %v", m["warnings"])
	}
}

func TestSimilarToolNames_WarnsWhenLookupFails(t *testing.T) {
	s, database := newTestServer(t)
	p := seedProject(t, database, "/test/proj")
	seedSession(t, database, "session-1", p.ID)
	seedToolUse(t, database, "session-1", "mcp__ccvault__search_conversations")

	// Dropping tool_uses is what a half-migrated or truncated archive looks
	// like to this lookup: the query errors rather than returning no rows.
	if _, err := database.Exec("DROP TABLE tool_uses"); err != nil {
		t.Fatalf("drop tool_uses: %v", err)
	}

	names, warning := s.similarToolNames("ccvault", 5)
	if names != nil {
		t.Errorf("names = %v, want nil when the lookup failed", names)
	}
	if !strings.HasPrefix(warning, "similar_tool_names unavailable: ") {
		t.Errorf("warning = %q, want a 'similar_tool_names unavailable: <why>' entry", warning)
	}
}

func TestSimilarToolNames_NoWarningWhenLookupSucceeds(t *testing.T) {
	s, database := newTestServer(t)
	p := seedProject(t, database, "/test/proj")
	seedSession(t, database, "session-1", p.ID)
	seedToolUse(t, database, "session-1", "mcp__ccvault__search_conversations")

	names, warning := s.similarToolNames("ccvault", 5)
	if warning != "" {
		t.Errorf("warning = %q, want empty on a healthy lookup", warning)
	}
	if len(names) != 1 || names[0] != "mcp__ccvault__search_conversations" {
		t.Errorf("names = %v, want [mcp__ccvault__search_conversations]", names)
	}
}

// A degraded project lookup leaves every result's project_name on the
// basename fallback. The response must say so in warnings[] rather than
// presenting the fallback as the adapter-provided label.
func TestSearchConversations_WarnsWhenProjectEnrichmentFails(t *testing.T) {
	s, database := newTestServer(t)
	p := seedProject(t, database, "/test/proj")
	seedSession(t, database, "session-1", p.ID)
	narrowProjectsTable(t, database)

	result, err := s.searchConversations(map[string]interface{}{"query": "hello"})
	if err != nil {
		t.Fatalf("searchConversations: %v", err)
	}

	m := resultMap(t, result)
	if mustInt(t, m, "count") != 1 {
		t.Fatalf("count = %v, want 1 — the search itself must still succeed", m["count"])
	}
	warnings := mustField[[]string](t, m, "warnings")
	if len(warnings) != 1 || !strings.HasPrefix(warnings[0], "project enrichment unavailable: ") {
		t.Errorf("warnings = %v, want one 'project enrichment unavailable: <why>' entry", warnings)
	}
}

// Both enrichments report into the same top-level warnings[] array, and a
// degraded one does not suppress a healthy one's field.
func TestSearchConversations_WarningsCoexistWithSimilarToolNames(t *testing.T) {
	s, database := newTestServer(t)
	p := seedProject(t, database, "/test/proj")
	seedSession(t, database, "session-1", p.ID)
	seedToolUse(t, database, "session-1", "mcp__ccvault__search_conversations")
	narrowProjectsTable(t, database)

	result, err := s.searchConversations(map[string]interface{}{"query": "tool:ccvault"})
	if err != nil {
		t.Fatalf("searchConversations: %v", err)
	}

	m := resultMap(t, result)
	if mustInt(t, m, "count") != 0 {
		t.Fatalf("count = %v, want 0 for a partial tool name", m["count"])
	}
	if similar := mustField[[]string](t, m, "similar_tool_names"); len(similar) != 1 {
		t.Errorf("similar_tool_names = %v, want the one seeded tool name", similar)
	}
	if warnings := mustField[[]string](t, m, "warnings"); len(warnings) != 1 {
		t.Errorf("warnings = %v, want exactly the project enrichment entry", warnings)
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

	m := resultMap(t, result)
	if mustInt(t, m, "count") == 0 {
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
	m := resultMap(t, result)
	if count := mustInt(t, m, "count"); count != 2 {
		t.Errorf("count = %v, want 2", count)
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
	mAll := resultMap(t, all)
	if _, present := mAll["has_more"]; present {
		t.Error("has_more should be absent when everything fit")
	}
}

func TestListSessions_PaginatesWithOffset(t *testing.T) {
	s, database := newTestServer(t)
	p := seedProject(t, database, "/test/proj")
	for i := 1; i <= 3; i++ {
		seedSession(t, database, fmt.Sprintf("session-%d", i), p.ID)
	}

	first, err := s.listSessions(map[string]interface{}{"limit": float64(2)})
	if err != nil {
		t.Fatalf("listSessions page 1: %v", err)
	}
	m1 := resultMap(t, first)
	if m1["offset"] != 0 || m1["limit"] != 2 {
		t.Errorf("page 1: offset/limit = %v/%v, want 0/2", m1["offset"], m1["limit"])
	}
	// Fatal, not just an error: page 2 is fetched with this value, so a
	// wrong next_offset makes every assertion below meaningless.
	nextOffset := mustInt(t, m1, "next_offset")
	if nextOffset != 2 {
		t.Fatalf("page 1: next_offset = %v, want 2", nextOffset)
	}
	if hint, _ := m1["hint"].(string); !strings.Contains(hint, "offset") {
		t.Errorf("page 1 hint should point at offset paging, got %q", hint)
	}

	second, err := s.listSessions(map[string]interface{}{
		"limit":  float64(2),
		"offset": float64(nextOffset),
	})
	if err != nil {
		t.Fatalf("listSessions page 2: %v", err)
	}
	m2 := resultMap(t, second)
	if count := mustInt(t, m2, "count"); count != 1 {
		t.Errorf("page 2: count = %v, want 1", count)
	}
	if m2["offset"] != 2 {
		t.Errorf("page 2: offset = %v, want 2", m2["offset"])
	}
	if _, present := m2["has_more"]; present {
		t.Error("page 2 exhausts the set; has_more should be absent")
	}

	// The two pages must tile the set with no gaps or repeats.
	seen := map[string]bool{}
	for _, page := range []map[string]interface{}{m1, m2} {
		for _, ref := range mustRefs(t, page, "sessions") {
			id := mustString(t, ref, "id")
			if seen[id] {
				t.Errorf("session %s appeared on two pages", id)
			}
			seen[id] = true
		}
	}
	if len(seen) != 3 {
		t.Errorf("paging covered %d sessions, want 3", len(seen))
	}

	// Past the end: an empty page, not an error and not a wrap-around.
	past, err := s.listSessions(map[string]interface{}{"offset": float64(99)})
	if err != nil {
		t.Fatalf("listSessions past end: %v", err)
	}
	mPast := resultMap(t, past)
	if count := mustInt(t, mPast, "count"); count != 0 {
		t.Errorf("offset past end: count = %v, want 0", count)
	}
}

func TestListProjects_PaginatesWithOffset(t *testing.T) {
	s, database := newTestServer(t)
	for _, path := range []string{"/test/proj-a", "/test/proj-b", "/test/proj-c"} {
		seedProject(t, database, path)
	}

	first, err := s.listProjects(map[string]interface{}{"limit": float64(2)})
	if err != nil {
		t.Fatalf("listProjects page 1: %v", err)
	}
	m1 := resultMap(t, first)
	if m1["offset"] != 0 || m1["limit"] != 2 {
		t.Errorf("page 1: offset/limit = %v/%v, want 0/2", m1["offset"], m1["limit"])
	}
	// Fatal for the same reason as in TestListSessions_PaginatesWithOffset:
	// the value is used to fetch page 2.
	nextOffset := mustInt(t, m1, "next_offset")
	if nextOffset != 2 {
		t.Fatalf("page 1: next_offset = %v, want 2", nextOffset)
	}
	if hint, _ := m1["hint"].(string); !strings.Contains(hint, "offset") {
		t.Errorf("page 1 hint should point at offset paging, got %q", hint)
	}

	second, err := s.listProjects(map[string]interface{}{
		"limit":  float64(2),
		"offset": float64(nextOffset),
	})
	if err != nil {
		t.Fatalf("listProjects page 2: %v", err)
	}
	m2 := resultMap(t, second)
	if count := mustInt(t, m2, "count"); count != 1 {
		t.Errorf("page 2: count = %v, want 1", count)
	}
	if _, present := m2["has_more"]; present {
		t.Error("page 2 exhausts the set; has_more should be absent")
	}

	seen := map[string]bool{}
	for _, page := range []map[string]interface{}{m1, m2} {
		for _, ref := range mustRefs(t, page, "projects") {
			path := mustString(t, ref, "path")
			if seen[path] {
				t.Errorf("project %s appeared on two pages", path)
			}
			seen[path] = true
		}
	}
	if len(seen) != 3 {
		t.Errorf("paging covered %d projects, want 3", len(seen))
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
	m := resultMap(t, result)
	if count := mustInt(t, m, "count"); count != 1 {
		t.Errorf("count = %v, want 1", count)
	}
	if m["has_more"] != true {
		t.Error("expected has_more=true when projects exceed limit")
	}

	// limit 0 must fall back to the default, not dump unbounded or return nothing
	zero, err := s.listProjects(map[string]interface{}{"limit": float64(0)})
	if err != nil {
		t.Fatalf("listProjects limit 0: %v", err)
	}
	mZero := resultMap(t, zero)
	if count := mustInt(t, mZero, "count"); count != 2 {
		t.Errorf("limit 0: count = %v, want 2 (default limit applied)", count)
	}
	if mZero["limit"] != 50 {
		t.Errorf("limit 0: limit = %v, want 50 (default)", mZero["limit"])
	}
	if _, present := mZero["has_more"]; present {
		t.Error("limit 0: has_more should be absent when everything fit")
	}

	// An oversized limit is clamped to the page maximum. The response
	// echoes the limit actually applied so the agent can compute offsets
	// from it instead of from what it asked for.
	big, err := s.listProjects(map[string]interface{}{"limit": float64(200)})
	if err != nil {
		t.Fatalf("listProjects limit 200: %v", err)
	}
	mBig := resultMap(t, big)
	if mBig["limit"] != 100 {
		t.Errorf("limit 200: limit = %v, want 100 (clamped)", mBig["limit"])
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

	m := resultMap(t, result)
	warnings, ok := m["warnings"].([]string)
	if !ok || len(warnings) == 0 {
		t.Fatalf("expected warnings about missing project, got %#v", m["warnings"])
	}
	// Every MCP warning reads "<what> unavailable: <why>" so an agent can
	// scan the list without learning per-tool phrasing.
	if !strings.Contains(warnings[0], "project 9999 unavailable:") {
		t.Errorf("warning should read 'project 9999 unavailable: ...', got %q", warnings[0])
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

	m := resultMap(t, result)
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

	m := resultMap(t, result)
	warnings, ok := m["warnings"].([]string)
	if !ok || len(warnings) == 0 {
		t.Fatalf("expected warnings about missing project, got %#v", m["warnings"])
	}
}

func TestGetSession_LargeSessionNoticeIsAWarningsEntry(t *testing.T) {
	s, database := newTestServer(t)
	p := seedProject(t, database, "/test/proj")
	seedSession(t, database, "session-big", p.ID)
	// seedSession already added one turn; 100 more clears the limit.
	seedTurns(t, database, "session-big", 100)

	result, err := s.getSession(map[string]interface{}{"session_id": "session-big"})
	if err != nil {
		t.Fatalf("getSession: %v", err)
	}

	m := resultMap(t, result)
	// One place to look, not two fields with nearly identical names.
	if _, present := m["warning"]; present {
		t.Errorf("get_session must not emit a singular 'warning' field, got %#v", m["warning"])
	}
	warnings, ok := m["warnings"].([]string)
	if !ok || len(warnings) != 1 {
		t.Fatalf("expected exactly one warning for a large session, got %#v", m["warnings"])
	}
	// Same "<what> unavailable: <why>" phrasing as every other MCP warning,
	// and it names the field the response is missing: markdown.
	if !strings.Contains(warnings[0], "markdown unavailable:") {
		t.Errorf("warning should read 'markdown unavailable: ...', got %q", warnings[0])
	}
	if !strings.Contains(warnings[0], "101 turns") {
		t.Errorf("warning should carry the turn count, got %q", warnings[0])
	}
	if _, present := m["markdown"]; present {
		t.Errorf("markdown should be absent when the warning says it is unavailable, got %#v", m["markdown"])
	}
	if turnCount := mustInt(t, m, "turn_count"); turnCount != 101 {
		t.Errorf("turn_count = %v, want 101", turnCount)
	}
}

func TestGetSession_LargeSessionAndMissingProjectShareOneWarningsArray(t *testing.T) {
	s, database := newTestServer(t)
	// No project 9999 exists, so both degradations hit the same response.
	seedSession(t, database, "session-big-orphan", 9999)
	seedTurns(t, database, "session-big-orphan", 100)

	result, err := s.getSession(map[string]interface{}{"session_id": "session-big-orphan"})
	if err != nil {
		t.Fatalf("getSession: %v", err)
	}

	m := resultMap(t, result)
	if _, present := m["warning"]; present {
		t.Errorf("get_session must not emit a singular 'warning' field, got %#v", m["warning"])
	}
	warnings, ok := m["warnings"].([]string)
	if !ok || len(warnings) != 2 {
		t.Fatalf("expected both warnings in one array, got %#v", m["warnings"])
	}
	joined := strings.Join(warnings, "\n")
	for _, want := range []string{"markdown unavailable:", "project 9999 unavailable:"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings should include %q, got %#v", want, warnings)
		}
	}
}

func TestGetAnalytics_ReportsUnavailableAnalytics(t *testing.T) {
	s, _ := newTestServer(t)
	s.analyzerErr = errors.New("duckdb cache missing")

	result, err := s.getAnalytics(map[string]interface{}{})
	if err != nil {
		t.Fatalf("getAnalytics: %v", err)
	}

	m := resultMap(t, result)
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

func TestGetAnalytics_LiftsStatsWarningsToTopLevel(t *testing.T) {
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

	m := resultMap(t, result)
	warnings, ok := m["warnings"].([]string)
	if !ok || len(warnings) == 0 {
		t.Fatalf("result[warnings] should be a non-empty []string, got %#v", m["warnings"])
	}
	if !strings.Contains(warnings[0], "tool") {
		t.Errorf("warning should mention tool stats, got %q", warnings[0])
	}

	summary, ok := m["summary"].(map[string]interface{})
	if !ok {
		t.Fatalf("result[summary] is not a map, got %#v", m["summary"])
	}
	if _, present := summary["warnings"]; present {
		t.Errorf("summary should not carry its own warnings, got %#v", summary["warnings"])
	}
}

func TestGetStats_DegradedFieldsWarn(t *testing.T) {
	s, database := newTestServer(t)
	p := seedProject(t, database, "/test/proj")
	seedSession(t, database, "session-1", p.ID)

	// Dropping tool_uses breaks GetToolUsageStats only — the core
	// project/session/token queries still answer.
	if _, err := database.Exec("DROP TABLE tool_uses"); err != nil {
		t.Fatalf("drop tool_uses: %v", err)
	}

	result, err := s.getStats(nil)
	if err != nil {
		t.Fatalf("getStats: %v", err)
	}

	m := resultMap(t, result)
	warnings, ok := m["warnings"].([]string)
	if !ok || len(warnings) != 1 {
		t.Fatalf("expected exactly one warning, got %#v", m["warnings"])
	}
	if !strings.Contains(warnings[0], "tool stats unavailable:") {
		t.Errorf("warning should read 'tool stats unavailable: ...', got %q", warnings[0])
	}
	// The field is omitted, not emitted as null — an agent checking
	// presence must not see an empty top_tools and read it as "no tools".
	if _, present := m["top_tools"]; present {
		t.Errorf("top_tools should be absent when its query failed, got %#v", m["top_tools"])
	}
	// Core fields survive the degradation.
	if sessions := mustInt(t, m, "sessions"); sessions != 1 {
		t.Errorf("sessions = %v, want 1", sessions)
	}
}

func TestGetStats_OmitsActivityRangeWhenUnavailable(t *testing.T) {
	s, database := newTestServer(t)
	p := seedProject(t, database, "/test/proj")
	seedSession(t, database, "session-1", p.ID)

	// GetFirstAndLastActivity reads projects.first_seen_at; GetProjectStats
	// reads only COUNT(*)/total_tokens from the same table. Dropping the
	// column degrades the activity range while the rest still answers.
	if _, err := database.Exec("ALTER TABLE projects DROP COLUMN first_seen_at"); err != nil {
		t.Fatalf("drop projects.first_seen_at: %v", err)
	}

	result, err := s.getStats(nil)
	if err != nil {
		t.Fatalf("getStats: %v", err)
	}

	m := resultMap(t, result)
	warnings, ok := m["warnings"].([]string)
	if !ok || len(warnings) == 0 {
		t.Fatalf("expected a warning about the activity range, got %#v", m["warnings"])
	}
	if !strings.Contains(warnings[0], "activity range unavailable:") {
		t.Errorf("warning should read 'activity range unavailable: ...', got %q", warnings[0])
	}
	for _, field := range []string{"first_activity", "last_activity", "days_span"} {
		if _, present := m[field]; present {
			t.Errorf("%s should be absent when the activity range failed, got %#v", field, m[field])
		}
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

	m := resultMap(t, result)
	if _, present := m["warnings"]; present {
		t.Errorf("healthy db should have no warnings, got %#v", m["warnings"])
	}
}

// --- dispatch tests (issue #7) -------------------------------------------

func TestServer_NotificationsInitialized_IsSilent(t *testing.T) {
	// Reporter's exact repro shape (issue #7).
	s, buf := newBufferedServer(t)
	s.handleRequest(&jsonRPCRequest{
		JSONRPC: "2.0",
		Method:  "notifications/initialized",
	})
	if buf.Len() != 0 {
		t.Errorf("notification produced a response: %q", buf.String())
	}
}

func TestServer_NotificationsCancelled_IsSilent(t *testing.T) {
	s, buf := newBufferedServer(t)
	s.handleRequest(&jsonRPCRequest{
		JSONRPC: "2.0",
		Method:  "notifications/cancelled",
		Params:  json.RawMessage(`{"requestId":1}`),
	})
	if buf.Len() != 0 {
		t.Errorf("notification produced a response: %q", buf.String())
	}
}

func TestServer_NotificationsRootsListChanged_IsSilent(t *testing.T) {
	s, buf := newBufferedServer(t)
	s.handleRequest(&jsonRPCRequest{
		JSONRPC: "2.0",
		Method:  "notifications/roots/list_changed",
	})
	if buf.Len() != 0 {
		t.Errorf("notification produced a response: %q", buf.String())
	}
}

func TestServer_UnknownNotification_IsSilent(t *testing.T) {
	// Any unrecognized method with no id must produce no output, per the
	// JSON-RPC 2.0 notification rule. Guards the default-branch fix.
	s, buf := newBufferedServer(t)
	s.handleRequest(&jsonRPCRequest{
		JSONRPC: "2.0",
		Method:  "notifications/some/future/thing",
	})
	if buf.Len() != 0 {
		t.Errorf("unknown notification produced a response: %q", buf.String())
	}
}

func TestServer_UnknownRequest_ReturnsErrorWithID(t *testing.T) {
	// Regression guard: an unknown REQUEST (has id) must still receive
	// a proper -32601 Method not found. The fix should only suppress
	// responses when id is nil.
	s, buf := newBufferedServer(t)
	s.handleRequest(&jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      float64(42), // json.Unmarshal turns numeric ids into float64
		Method:  "resources/list",
	})
	var resp map[string]any
	if err := json.Unmarshal(buf.Bytes(), &resp); err != nil {
		t.Fatalf("response is not valid JSON: %q", buf.String())
	}
	if resp["id"] != float64(42) {
		t.Errorf("response id = %v, want 42", resp["id"])
	}
	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("no error object in response: %v", resp)
	}
	if errObj["code"] != float64(-32601) {
		t.Errorf("error code = %v, want -32601", errObj["code"])
	}
}

func TestServer_Initialize_ReturnsExpectedShape(t *testing.T) {
	// Regression guard: the happy-path handler still returns a
	// spec-shaped result. Verifies our fix didn't break the working
	// request path.
	s, buf := newBufferedServer(t)
	s.handleRequest(&jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      float64(1),
		Method:  "initialize",
		Params:  json.RawMessage(`{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"1"}}`),
	})
	body := buf.String()
	for _, want := range []string{`"id":1`, `"result"`, `"protocolVersion":"2024-11-05"`, `"serverInfo"`} {
		if !strings.Contains(body, want) {
			t.Errorf("initialize response missing %q; got: %s", want, body)
		}
	}
}
