# Agent Surface Hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make ccvault's agent-facing surfaces (MCP server, `orient` command, search filters, skill docs) fail loud instead of silently degrading, and give agents actionable guidance in tool output.

**Architecture:** Four playbook patterns applied to existing code — no new subsystems. (1) *Fail loud over fallbacks*: enrichment-path errors (analytics init, project lookups, stats queries, markdown export) currently vanish via `_ =` and `if err == nil`; they become `warnings` arrays in responses or returned errors. (2) *Agents optimize tools*: `tool:` search becomes case-insensitive and the skill docs' factually false claim about MCP tool names gets corrected (the DB holds 3,833 `mcp__`-prefixed tool_use rows; the docs claim it holds none). (3) *Embed guidance in tool output*: empty search results get hints + similar-tool-name suggestions; `list_sessions`/`list_projects` get `has_more`. (4) *Standing eval capability*: `scenarios.jsonl` is currently executed by nothing; a guard test keeps it mapped to real tests.

**Tech Stack:** Go, SQLite (mattn/go-sqlite3 via internal/db), MCP JSON-RPC over stdio, cobra CLI, standard `testing` package.

## Design Context

The pattern-mapping analysis behind this plan was done against https://github.com/ramparte/agent-building-playbook on 2026-07-13. Verified facts this plan relies on:

- The live DB (`~/.ccvault/ccvault.db`) contains MCP-prefixed tool names (`mcp__socialmedia__create_post`: 844 rows, `mcp__chronicle__remember_this`: 405 rows). `skills/ccvault/SKILL.md:114` claims the DB stores only unprefixed names — false, must be corrected, not encoded into behavior.
- `internal/mcp/` has **no tests today**. This plan introduces `server_test.go` calling handler methods directly against a real temp SQLite DB (handlers are plain methods `func (s *Server) x(args map[string]interface{}) (interface{}, error)` — no stdio needed).
- The schema declares **no FOREIGN KEY constraints**, so a session row with a dangling `project_id` is insertable in tests — that's exactly the data-integrity case the new warnings surface.
- `analytics.NewAnalyzer` failure is currently swallowed in `NewServer` (server.go:35-39); `get_analytics` then silently omits sections.
- The analytics cache is built by the CLI command `ccvault build-cache` (main.go:862).

## Global Constraints

- Every new code file starts with two `// ABOUTME: ` comment lines (repo convention).
- Conventional commits, imperative present tense. Never use `--no-verify` or any hook-bypass flag.
- Match surrounding style: handlers return `map[string]interface{}`; DB errors wrap with `fmt.Errorf("context: %w", err)`; tests use stdlib `testing` only (no assertion libs).
- Response shape rule for this PR: enrichment failures append human-readable strings to a `warnings` key (`[]string`); core-data failures still return `(nil, err)`. Never drop an error silently.
- Run tests per-package while iterating (`go test ./internal/search/ -run TestName -v`); full gate before each commit is `go test ./...`; final gate is `make test-race` and `make lint`.
- Work happens on branch `feat/agent-surface-hardening` (already created).

## File Map

| File | Action | Responsibility |
|---|---|---|
| `internal/search/search.go` | Modify (1 line) | Case-insensitive `tool:` SQL comparison |
| `internal/search/search_db_test.go` | Create | DB-backed tests for SQL-level filter behavior |
| `internal/db/turns.go` | Modify (add func) | `GetToolNamesLike` suggestion helper |
| `internal/db/db_test.go` | Modify (add test) | Test for `GetToolNamesLike` |
| `internal/mcp/server.go` | Modify | Hints, `has_more`, limit clamps, warnings, `analyzerErr` |
| `internal/mcp/server_test.go` | Create | Handler tests against real temp DB |
| `cmd/ccvault/main.go` | Modify | Extract `gatherOrientation`, collect warnings |
| `cmd/ccvault/main_test.go` | Create | Tests for `gatherOrientation` |
| `test/integration/scenarios_test.go` | Create | scenarios.jsonl coverage guard |
| `skills/ccvault/SKILL.md` | Modify | Correct false MCP-tool-name claim |
| `skills/ccvault/reference.md` | Modify | Tool-matching + pagination doc rows |

---

### Task 1: Case-insensitive `tool:` matching + doc truth fix

**Files:**
- Create: `internal/search/search_db_test.go`
- Modify: `internal/search/search.go:104`
- Modify: `skills/ccvault/reference.md:22`
- Modify: `skills/ccvault/SKILL.md:114`

**Interfaces:**
- Consumes: `db.Open(dir string) (*db.DB, error)`, `(*db.DB).UpsertProject/UpsertSession/InsertTurns/InsertToolUses`, `search.New(db *sql.DB) *Searcher`, `search.Parse(input string) *Query`, `(*Searcher).Search(q *Query, limit int) ([]Result, error)`
- Produces: `tool:` filter matches full tool names case-insensitively (SQL `COLLATE NOCASE`). Task 3's hint feature and doc edits assume this exact semantic: **full-name, case-insensitive, no substring match**.

- [ ] **Step 1: Write the failing test**

Create `internal/search/search_db_test.go`:

```go
// ABOUTME: Database-backed tests for search execution
// ABOUTME: Verifies SQL-level filter behavior against a real SQLite database

package search

import (
	"testing"
	"time"

	"github.com/2389-research/ccvault/internal/db"
	"github.com/2389-research/ccvault/pkg/models"
)

func setupSearchDB(t *testing.T) *db.DB {
	t.Helper()

	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	p := &models.Project{Path: "/test/proj", DisplayName: "proj"}
	if err := database.UpsertProject(p); err != nil {
		t.Fatalf("upsert project: %v", err)
	}

	s := &models.Session{ID: "session-1", ProjectID: p.ID, StartedAt: time.Now(), SourceFile: "/test.jsonl"}
	if err := database.UpsertSession(s); err != nil {
		t.Fatalf("upsert session: %v", err)
	}

	turns := []models.Turn{
		{ID: "turn-1", SessionID: "session-1", Type: "user", Timestamp: time.Now(), Content: "run the deploy script"},
	}
	if err := database.InsertTurns(turns); err != nil {
		t.Fatalf("insert turns: %v", err)
	}

	toolUses := []models.ToolUse{
		{TurnID: "turn-1", SessionID: "session-1", ToolName: "Bash", Timestamp: time.Now()},
		{TurnID: "turn-1", SessionID: "session-1", ToolName: "mcp__ccvault__search_conversations", Timestamp: time.Now()},
	}
	if err := database.InsertToolUses(toolUses); err != nil {
		t.Fatalf("insert tool uses: %v", err)
	}

	return database
}

func TestSearch_ToolFilterIsCaseInsensitive(t *testing.T) {
	database := setupSearchDB(t)
	searcher := New(database.DB)

	for _, queryStr := range []string{"tool:Bash", "tool:bash", "tool:BASH"} {
		results, err := searcher.Search(Parse(queryStr), 10)
		if err != nil {
			t.Fatalf("search %q: %v", queryStr, err)
		}
		if len(results) == 0 {
			t.Errorf("%q returned no results, want at least 1", queryStr)
		}
	}
}

func TestSearch_ToolFilterMatchesFullMCPNames(t *testing.T) {
	database := setupSearchDB(t)
	searcher := New(database.DB)

	// Full prefixed MCP names are stored and must match
	results, err := searcher.Search(Parse("tool:mcp__ccvault__search_conversations"), 10)
	if err != nil {
		t.Fatalf("search full mcp name: %v", err)
	}
	if len(results) == 0 {
		t.Error("full MCP tool name returned no results, want at least 1")
	}

	// Matching is full-name, not substring: a fragment must NOT match
	fragResults, err := searcher.Search(Parse("tool:ccvault"), 10)
	if err != nil {
		t.Fatalf("search fragment: %v", err)
	}
	if len(fragResults) != 0 {
		t.Errorf("fragment 'ccvault' matched %d results, want 0 (full-name matching)", len(fragResults))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/search/ -run TestSearch_Tool -v`
Expected: `TestSearch_ToolFilterIsCaseInsensitive` FAILS on `"tool:bash"` and `"tool:BASH"` ("returned no results"); `TestSearch_ToolFilterMatchesFullMCPNames` passes (it pins current exact-match behavior against regression).

- [ ] **Step 3: Write minimal implementation**

In `internal/search/search.go`, the tool filter block currently reads:

```go
	// Tool filter requires join
	if q.Tool != "" {
		baseQuery += ` JOIN tool_uses tu ON t.session_id = tu.session_id`
		conditions = append(conditions, fmt.Sprintf("tu.tool_name = $%d", argNum))
		args = append(args, q.Tool)
		argNum++
	}
```

Change the `conditions` line to:

```go
		conditions = append(conditions, fmt.Sprintf("tu.tool_name = $%d COLLATE NOCASE", argNum))
```

(SQLite's `NOCASE` collation folds ASCII case, which covers all real tool names — they are ASCII identifiers.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/search/ -v`
Expected: ALL PASS (including the pre-existing parse/snippet tests).

- [ ] **Step 5: Fix the docs that describe this behavior**

In `skills/ccvault/reference.md`, replace line 22:

```markdown
| Tool | `tool:Name` | `tool:Bash` | Case-sensitive, must match exact tool name (e.g., `Bash`, `Read`, `Edit`, `Write`, `Grep`, `Glob`, `Task`, `WebFetch`) |
```

with:

```markdown
| Tool | `tool:Name` | `tool:Bash` | Case-insensitive, must match the full tool name (e.g., `Bash`, `Read`, `Edit`, `Write`, `Grep`, `Glob`, `Task`, `WebFetch`; MCP tools are stored under their full prefixed names like `mcp__ccvault__search_conversations`) |
```

In `skills/ccvault/SKILL.md`, replace line 114 (a factually false claim — the DB does store `mcp__`-prefixed names):

```markdown
- Searching for MCP tool names like `mcp__ccvault__search_conversations` — the database stores tool names as they appear in Claude Code logs (e.g., `Bash`, `Read`, `Edit`, not the prefixed MCP names)
```

with:

```markdown
- Searching `tool:` with a fragment like `tool:ccvault` — `tool:` matches the full tool name (case-insensitive), never substrings. Built-in tools are stored as `Bash`, `Read`, `Edit`; MCP tools under their full prefixed names like `mcp__ccvault__search_conversations`
```

- [ ] **Step 6: Run the full suite**

Run: `go test ./...`
Expected: ALL PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/search/search.go internal/search/search_db_test.go skills/ccvault/reference.md skills/ccvault/SKILL.md
git commit -m "fix: make tool: search filter case-insensitive and correct tool-name docs"
```

---

### Task 2: `GetToolNamesLike` suggestion helper

**Files:**
- Modify: `internal/db/turns.go` (append function after `GetToolUsageStats`, which ends near line 310)
- Test: `internal/db/db_test.go` (append test)

**Interfaces:**
- Consumes: existing `tool_uses` table, `setupTestDB(t)` helper already in `db_test.go`.
- Produces: `func (db *DB) GetToolNamesLike(fragment string, limit int) ([]string, error)` — distinct tool names containing `fragment` (case-insensitive), sorted alphabetically, at most `limit` (default 10 when `limit <= 0`). Task 3's empty-result hint calls exactly this signature.

- [ ] **Step 1: Write the failing test**

Append to `internal/db/db_test.go`:

```go
func TestGetToolNamesLike(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	p := &models.Project{Path: "/test", DisplayName: "test"}
	_ = db.UpsertProject(p)
	s := &models.Session{ID: "session-1", ProjectID: p.ID, StartedAt: time.Now(), SourceFile: "/test.jsonl"}
	_ = db.UpsertSession(s)

	toolUses := []models.ToolUse{
		{TurnID: "turn-1", SessionID: s.ID, ToolName: "Bash", Timestamp: time.Now()},
		{TurnID: "turn-2", SessionID: s.ID, ToolName: "mcp__ccvault__search_conversations", Timestamp: time.Now()},
		{TurnID: "turn-3", SessionID: s.ID, ToolName: "mcp__chronicle__remember_this", Timestamp: time.Now()},
	}
	if err := db.InsertToolUses(toolUses); err != nil {
		t.Fatalf("insert tool uses: %v", err)
	}

	names, err := db.GetToolNamesLike("ccvault", 5)
	if err != nil {
		t.Fatalf("GetToolNamesLike: %v", err)
	}
	if len(names) != 1 || names[0] != "mcp__ccvault__search_conversations" {
		t.Errorf("names = %v, want [mcp__ccvault__search_conversations]", names)
	}

	upper, err := db.GetToolNamesLike("CCVAULT", 5)
	if err != nil {
		t.Fatalf("GetToolNamesLike upper: %v", err)
	}
	if len(upper) != 1 {
		t.Errorf("case-insensitive match failed, got %v", upper)
	}

	none, err := db.GetToolNamesLike("zzz-no-such-tool", 5)
	if err != nil {
		t.Fatalf("GetToolNamesLike none: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("expected no matches, got %v", none)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/db/ -run TestGetToolNamesLike -v`
Expected: COMPILE ERROR — `db.GetToolNamesLike undefined`.

- [ ] **Step 3: Write minimal implementation**

Append to `internal/db/turns.go`:

```go
// GetToolNamesLike returns distinct tool names containing fragment
// (case-insensitive), ordered by name. Used for "did you mean" hints
// when a tool: search matches nothing.
func (db *DB) GetToolNamesLike(fragment string, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 10
	}

	rows, err := db.Query(`
		SELECT DISTINCT tool_name
		FROM tool_uses
		WHERE tool_name LIKE '%' || ? || '%'
		ORDER BY tool_name
		LIMIT ?`, fragment, limit)
	if err != nil {
		return nil, fmt.Errorf("query tool names: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan tool name: %w", err)
		}
		names = append(names, name)
	}

	return names, rows.Err()
}
```

(SQLite's `LIKE` is case-insensitive for ASCII by default — the test's `CCVAULT` assertion pins that assumption.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/db/ -run TestGetToolNamesLike -v`
Expected: PASS.

- [ ] **Step 5: Run the full suite**

Run: `go test ./...`
Expected: ALL PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/db/turns.go internal/db/db_test.go
git commit -m "feat: add GetToolNamesLike for tool-name suggestions"
```

---

### Task 3: MCP guidance — empty-result hints, `has_more` on list tools, limit clamps

**Files:**
- Create: `internal/mcp/server_test.go`
- Modify: `internal/mcp/server.go` — `searchConversations` (ends ~line 615), `listSessions` (~line 930), `listProjects` (~line 969), and the `list_sessions`/`list_projects` tool schemas inside `handleToolsList` (~lines 246-390)
- Modify: `skills/ccvault/reference.md:11-12`
- Modify: `skills/ccvault/SKILL.md:114` (append one sentence to the line rewritten in Task 1)

**Interfaces:**
- Consumes: `(*db.DB).GetToolNamesLike(fragment string, limit int) ([]string, error)` from Task 2; existing `GetSessions(projectID int64, limit int)`, `GetProjects(orderBy string, limit int)`.
- Produces: `searchConversations` responses with zero results carry `"hint"` (string) and, when a `tool:` filter was present and near-misses exist, `"similar_tool_names"` (`[]string`). `listSessions`/`listProjects` responses carry `"has_more": true` + `"hint"` when truncated. Both clamp `limit` to `[default, 100]`. Task 4 reuses `newTestServer`/`seedSession` from the test file created here.

- [ ] **Step 1: Write the failing tests**

Create `internal/mcp/server_test.go`:

```go
// ABOUTME: Tests for MCP server tool handlers
// ABOUTME: Exercises handlers against a real temporary SQLite database

package mcp

import (
	"fmt"
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/mcp/ -v`
Expected: `TestSearchConversations_EmptyResultsIncludeHint` FAILS (no hint key), `TestListSessions_ReportsHasMore` FAILS (no has_more), `TestListProjects_ReportsHasMoreAndClampsLimit` FAILS. `TestSearchConversations_ResultsHaveNoHint` PASSES (pins current behavior).

- [ ] **Step 3: Implement in `searchConversations`**

In `internal/mcp/server.go`, `searchConversations` currently ends:

```go
	response := map[string]interface{}{
		"count":   len(compactResults),
		"offset":  offset,
		"limit":   limit,
		"results": compactResults,
	}

	if hasMore {
		response["next_offset"] = offset + limit
		response["has_more"] = true
	}

	return response, nil
```

Insert an empty-result block before `return response, nil`:

```go
	if len(compactResults) == 0 {
		hint := "No results. Broaden the search: drop one filter or try different terms; use list_projects to verify project names."
		if parsed.Tool != "" {
			if names, err := s.db.GetToolNamesLike(parsed.Tool, 5); err == nil && len(names) > 0 {
				response["similar_tool_names"] = names
				hint = fmt.Sprintf("No results for tool:%s — tool matching requires the full tool name. See similar_tool_names for close matches.", parsed.Tool)
			}
		}
		response["hint"] = hint
	}

	return response, nil
```

- [ ] **Step 4: Implement in `listSessions`**

Replace the body sections of `listSessions`. The limit block currently reads:

```go
	limit := 20
	if l, ok := args["limit"].(float64); ok {
		limit = int(l)
		if limit > 100 {
			limit = 100
		}
	}
```

Change to:

```go
	limit := 20
	if l, ok := args["limit"].(float64); ok {
		limit = int(l)
		if limit > 100 {
			limit = 100
		}
	}
	if limit <= 0 {
		limit = 20
	}
```

The fetch-and-return section currently reads:

```go
	sessions, err := s.db.GetSessions(projectID, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to get sessions: %w", err)
	}

	return map[string]interface{}{
		"count":    len(sessions),
		"sessions": sessions,
	}, nil
```

Change to:

```go
	// Fetch one extra row to detect whether more sessions exist
	sessions, err := s.db.GetSessions(projectID, limit+1)
	if err != nil {
		return nil, fmt.Errorf("failed to get sessions: %w", err)
	}

	hasMore := len(sessions) > limit
	if hasMore {
		sessions = sessions[:limit]
	}

	response := map[string]interface{}{
		"count":    len(sessions),
		"sessions": sessions,
	}
	if hasMore {
		response["has_more"] = true
		response["hint"] = "More sessions exist. Raise limit (max 100) or narrow with the project filter."
	}

	return response, nil
```

- [ ] **Step 5: Implement in `listProjects`**

The whole function currently reads:

```go
	limit := 50
	if l, ok := args["limit"].(float64); ok {
		limit = int(l)
	}

	projects, err := s.db.GetProjects(sortBy, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to get projects: %w", err)
	}

	return map[string]interface{}{
		"count":    len(projects),
		"projects": projects,
	}, nil
```

Change to:

```go
	limit := 50
	if l, ok := args["limit"].(float64); ok {
		limit = int(l)
		if limit > 100 {
			limit = 100
		}
	}
	if limit <= 0 {
		limit = 50
	}

	// Fetch one extra row to detect whether more projects exist
	projects, err := s.db.GetProjects(sortBy, limit+1)
	if err != nil {
		return nil, fmt.Errorf("failed to get projects: %w", err)
	}

	hasMore := len(projects) > limit
	if hasMore {
		projects = projects[:limit]
	}

	response := map[string]interface{}{
		"count":    len(projects),
		"projects": projects,
	}
	if hasMore {
		response["has_more"] = true
		response["hint"] = "More projects exist. Raise limit (max 100) or use sort to surface the relevant ones."
	}

	return response, nil
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/mcp/ -v`
Expected: ALL PASS.

- [ ] **Step 7: Update tool schemas and docs**

In `handleToolsList` (server.go, between lines ~246-390), locate the `list_sessions` tool definition and set its `limit` property description to exactly:

```
Maximum sessions to return (default 20, max 100)
```

Locate the `list_projects` tool definition and set its `limit` property description to exactly:

```
Maximum projects to return (default 50, max 100)
```

(Only the description strings change; if the current strings already say this, leave them.)

In `skills/ccvault/reference.md`, update the two list rows. Line 11's Notes cell currently ends with `returns error (not empty) if no project matches` — append `; sets has_more when truncated`. Line 12 currently reads:

```markdown
| `list_projects` | — | `sort` (name/activity/tokens/sessions, default: activity), `limit` (number, default 50) | Projects with session counts and token usage | Use to discover project names before searching |
```

Replace with:

```markdown
| `list_projects` | — | `sort` (name/activity/tokens/sessions, default: activity), `limit` (number, default 50, max 100) | Projects with session counts and token usage | Use to discover project names before searching; sets has_more when truncated |
```

In `skills/ccvault/SKILL.md`, append one sentence to the anti-pattern line rewritten in Task 1, so it ends:

```markdown
- Searching `tool:` with a fragment like `tool:ccvault` — `tool:` matches the full tool name (case-insensitive), never substrings. Built-in tools are stored as `Bash`, `Read`, `Edit`; MCP tools under their full prefixed names like `mcp__ccvault__search_conversations`. Empty results include `similar_tool_names` suggestions when close matches exist
```

- [ ] **Step 8: Run the full suite**

Run: `go test ./...`
Expected: ALL PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/mcp/server.go internal/mcp/server_test.go skills/ccvault/reference.md skills/ccvault/SKILL.md
git commit -m "feat: add empty-result hints and has_more to MCP search and list tools"
```

---

### Task 4: Fail loud in MCP enrichment paths

**Files:**
- Modify: `internal/mcp/server.go` — `Server` struct (~line 25), `NewServer` (~line 33), `getSessionSummary` (~lines 636-643 and 733-750), `getSession` (~lines 889-927), `getStats` (~lines 1007-1018), `getAnalytics` (~lines 1049-1064), `promptReviewSession` (~lines 1237-1251)
- Test: `internal/mcp/server_test.go` (append tests; uses helpers from Task 3)

**Interfaces:**
- Consumes: `newTestServer`, `seedSession`, `seedProject` from Task 3's test file. `analytics.NewAnalyzer(cacheDir string)` returning `(*analytics.Analyzer, error)`.
- Produces: `Server` gains field `analyzerErr error`. Responses gain optional `"warnings"` (`[]string`). `get_analytics` gains an `"analytics"` availability object (`available`/`reason`/`hint`) when the analyzer is nil. `promptReviewSession` returns export errors instead of discarding them.

- [ ] **Step 1: Write the failing tests**

Append to `internal/mcp/server_test.go` (add `"errors"` and `"strings"` to the import block):

```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/mcp/ -v`
Expected: COMPILE ERROR — `s.analyzerErr undefined` (struct field doesn't exist yet). That is the red state; the warning tests will fail on assertions once it compiles.

- [ ] **Step 3: Add `analyzerErr` to the Server struct and `NewServer`**

Struct (currently at server.go:24-30):

```go
// Server handles MCP protocol communication
type Server struct {
	db          *db.DB
	cfg         *config.Config
	analyzer    *analytics.Analyzer
	analyzerErr error
	debug       bool
}
```

`NewServer` (currently swallows the error at lines 35-39):

```go
// NewServer creates a new MCP server
func NewServer(database *db.DB, cfg *config.Config) (*Server, error) {
	cacheDir := filepath.Join(cfg.DataDir, "analytics")
	analyzer, analyzerErr := analytics.NewAnalyzer(cacheDir)
	if analyzerErr != nil {
		// Analytics stays optional, but the reason is kept and surfaced by get_analytics
		analyzer = nil
	}

	return &Server{
		db:          database,
		cfg:         cfg,
		analyzer:    analyzer,
		analyzerErr: analyzerErr,
		debug:       os.Getenv("CCVAULT_MCP_DEBUG") == "1",
	}, nil
}
```

- [ ] **Step 4: Warnings in `getSessionSummary`**

Replace the project block (lines ~636-643):

```go
	// Get project info
	var projectPath string
	if session.ProjectID > 0 {
		project, err := s.db.GetProject(session.ProjectID)
		if err == nil && project != nil {
			projectPath = project.Path
		}
	}
```

with:

```go
	// Get project info
	var projectPath string
	var warnings []string
	if session.ProjectID > 0 {
		project, err := s.db.GetProject(session.ProjectID)
		switch {
		case err != nil:
			warnings = append(warnings, fmt.Sprintf("project lookup failed for project %d: %v", session.ProjectID, err))
		case project == nil:
			warnings = append(warnings, fmt.Sprintf("project %d not found — session references a missing project", session.ProjectID))
		default:
			projectPath = project.Path
		}
	}
```

Replace the final return (lines ~733-750) — same map contents, assigned to a variable so warnings can attach:

```go
	result := map[string]interface{}{
		"session_id":     session.ID,
		"project_path":   projectPath,
		"source":         session.Source,
		"model":          session.Model,
		"started_at":     session.StartedAt.Format(time.RFC3339),
		"ended_at":       session.EndedAt.Format(time.RFC3339),
		"git_branch":     session.GitBranch,
		"turn_count":     len(turns),
		"turn_types":     turnTypeCounts,
		"input_tokens":   session.InputTokens,
		"output_tokens":  session.OutputTokens,
		"total_tokens":   session.TotalTokens(),
		"tools_used":     topToolsMap,
		"first_user_msg": firstUserMsg,
		"last_user_msg":  lastUserMsg,
		"hint":           "Use get_turns to paginate through the conversation",
	}
	if len(warnings) > 0 {
		result["warnings"] = warnings
	}
	return result, nil
```

- [ ] **Step 5: Warnings in `getSession`**

`getSession` has its own copy of the project block (lines ~889-896):

```go
	// Get project info
	var projectPath string
	if session.ProjectID > 0 {
		project, err := s.db.GetProject(session.ProjectID)
		if err == nil && project != nil {
			projectPath = project.Path
		}
	}
```

Replace it with:

```go
	// Get project info
	var projectPath string
	var warnings []string
	if session.ProjectID > 0 {
		project, err := s.db.GetProject(session.ProjectID)
		switch {
		case err != nil:
			warnings = append(warnings, fmt.Sprintf("project lookup failed for project %d: %v", session.ProjectID, err))
		case project == nil:
			warnings = append(warnings, fmt.Sprintf("project %d not found — session references a missing project", session.ProjectID))
		default:
			projectPath = project.Path
		}
	}
```

Then attach warnings to both return paths. The large-session return (lines ~899-906) becomes:

```go
	// For large sessions, recommend using get_session_summary + get_turns
	if len(turns) > 100 {
		result := map[string]interface{}{
			"warning":    "Large session with " + fmt.Sprintf("%d", len(turns)) + " turns. Use get_session_summary and get_turns for better results.",
			"session_id": sessionID,
			"turn_count": len(turns),
			"hint":       "Call get_session_summary first, then use get_turns with offset/limit to paginate",
		}
		if len(warnings) > 0 {
			result["warnings"] = warnings
		}
		return result, nil
	}
```

The final return (lines ~923-927) becomes:

```go
	result := map[string]interface{}{
		"session_id": sessionID,
		"turn_count": len(turns),
		"markdown":   content,
	}
	if len(warnings) > 0 {
		result["warnings"] = warnings
	}
	return result, nil
```

- [ ] **Step 6: Warnings in `getStats`**

Replace (lines ~1007-1009):

```go
	firstActivity, lastActivity, _ := s.db.GetFirstAndLastActivity()

	toolStats, _ := s.db.GetToolUsageStats(10)
```

with:

```go
	var warnings []string

	firstActivity, lastActivity, err := s.db.GetFirstAndLastActivity()
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("activity range unavailable: %v", err))
	}

	toolStats, err := s.db.GetToolUsageStats(10)
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("tool stats unavailable: %v", err))
	}
```

And before `return result, nil` at the end of `getStats`, insert:

```go
	if len(warnings) > 0 {
		result["warnings"] = warnings
	}
```

- [ ] **Step 7: Availability object and warnings in `getAnalytics`**

Replace the analyzer block (lines ~1048-1064):

```go
	// Try to get DuckDB analytics if available
	if s.analyzer != nil {
		dailyTokens, err := s.analyzer.GetTokensByDay(days)
		if err == nil {
			result["tokens_by_day"] = dailyTokens
		}

		topProjects, err := s.analyzer.GetTopProjects(10)
		if err == nil {
			result["top_projects"] = topProjects
		}

		modelStats, err := s.analyzer.GetTokensByModel()
		if err == nil {
			result["model_breakdown"] = modelStats
		}
	}
```

with:

```go
	// DuckDB analytics: report failures instead of silently omitting sections
	if s.analyzer != nil {
		var warnings []string

		dailyTokens, err := s.analyzer.GetTokensByDay(days)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("tokens_by_day unavailable: %v", err))
		} else {
			result["tokens_by_day"] = dailyTokens
		}

		topProjects, err := s.analyzer.GetTopProjects(10)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("top_projects unavailable: %v", err))
		} else {
			result["top_projects"] = topProjects
		}

		modelStats, err := s.analyzer.GetTokensByModel()
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("model_breakdown unavailable: %v", err))
		} else {
			result["model_breakdown"] = modelStats
		}

		if len(warnings) > 0 {
			result["warnings"] = warnings
		}
	} else {
		reason := "analytics cache not initialized"
		if s.analyzerErr != nil {
			reason = s.analyzerErr.Error()
		}
		result["analytics"] = map[string]interface{}{
			"available": false,
			"reason":    reason,
			"hint":      "Run 'ccvault build-cache' to enable DuckDB analytics",
		}
	}
```

- [ ] **Step 8: Stop discarding the export error in `promptReviewSession`**

Replace (lines ~1237-1251):

```go
	// Get project info
	var projectPath string
	if session.ProjectID > 0 {
		project, _ := s.db.GetProject(session.ProjectID)
		if project != nil {
			projectPath = project.Path
		}
	}

	// Export to markdown for easier reading
	var buf strings.Builder
	exporter := export.NewMarkdownExporter(
		export.WithThinking(false), // Skip thinking for summary
	)
	_ = exporter.Export(&buf, session, turns, projectPath)
```

with:

```go
	// Get project info — cosmetic in a prompt, so a lookup failure only logs
	var projectPath string
	if session.ProjectID > 0 {
		project, err := s.db.GetProject(session.ProjectID)
		if err != nil {
			s.log("project lookup failed for project %d: %v", session.ProjectID, err)
		}
		if project != nil {
			projectPath = project.Path
		}
	}

	// Export to markdown for easier reading; the markdown IS the prompt,
	// so a failed export must error rather than produce an empty review
	var buf strings.Builder
	exporter := export.NewMarkdownExporter(
		export.WithThinking(false), // Skip thinking for summary
	)
	if err := exporter.Export(&buf, session, turns, projectPath); err != nil {
		return promptGetResult{}, fmt.Errorf("export session markdown: %w", err)
	}
```

(No practical way to force `Export` to fail against a healthy DB without mocks, which this repo forbids — the happy-path prompt tests plus this diff are the coverage. Flag it in review.)

- [ ] **Step 9: Run tests to verify they pass**

Run: `go test ./internal/mcp/ -v`
Expected: ALL PASS.

- [ ] **Step 10: Run the full suite**

Run: `go test ./...`
Expected: ALL PASS.

- [ ] **Step 11: Commit**

```bash
git add internal/mcp/server.go internal/mcp/server_test.go
git commit -m "fix: surface MCP enrichment failures as warnings instead of dropping them"
```

---

### Task 5: `orient` collects errors into warnings

**Files:**
- Modify: `cmd/ccvault/main.go` — `orientCmd` RunE (lines ~126-271); add `orientation` struct + `gatherOrientation` function near it
- Create: `cmd/ccvault/main_test.go`

**Interfaces:**
- Consumes: `db.Open`, `GetProjectStats() (int, int64, error)`, `GetSessionStats() (int, int, int64, error)`, `GetFirstAndLastActivity() (time.Time, time.Time, error)`, `GetToolUsageStats(int) (map[string]int, error)`, `GetTokensByModel() (map[string]int64, error)`, `GetProjects(string, int) ([]models.Project, error)`
- Produces: `type orientation struct` and `func gatherOrientation(database *db.DB) orientation` in package main. JSON output gains a `"warnings"` key when queries fail; human output prints warnings to stderr.

- [ ] **Step 1: Write the failing test**

Create `cmd/ccvault/main_test.go`:

```go
// ABOUTME: Tests for CLI helpers in package main
// ABOUTME: Verifies orient gathers database state and reports failures as warnings

package main

import (
	"testing"

	"github.com/2389-research/ccvault/internal/db"
)

func TestGatherOrientation_HealthyDBHasNoWarnings(t *testing.T) {
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = database.Close() }()

	o := gatherOrientation(database)
	if len(o.Warnings) != 0 {
		t.Errorf("healthy db should produce no warnings, got %v", o.Warnings)
	}
}

func TestGatherOrientation_CollectsWarningsOnFailure(t *testing.T) {
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	_ = database.Close() // force every stats query to fail

	o := gatherOrientation(database)
	if len(o.Warnings) != 6 {
		t.Errorf("closed db should produce 6 warnings (one per query), got %d: %v", len(o.Warnings), o.Warnings)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/ccvault/ -v`
Expected: COMPILE ERROR — `gatherOrientation undefined`.

- [ ] **Step 3: Add the struct and gather function**

Add to `cmd/ccvault/main.go`, directly above `var orientCmd`:

```go
// orientation holds the database state gathered for the orient command.
type orientation struct {
	ProjectCount  int
	SessionCount  int
	TurnCount     int
	SessionTokens int64
	FirstActivity time.Time
	LastActivity  time.Time
	ToolStats     map[string]int
	TokensByModel map[string]int64
	ProjectNames  []string
	Warnings      []string
}

// gatherOrientation collects database state for the orient command.
// Query failures land in Warnings instead of being silently dropped.
func gatherOrientation(database *db.DB) orientation {
	var o orientation
	warn := func(what string, err error) {
		if err != nil {
			o.Warnings = append(o.Warnings, fmt.Sprintf("%s unavailable: %v", what, err))
		}
	}

	var err error
	o.ProjectCount, _, err = database.GetProjectStats()
	warn("project stats", err)

	o.SessionCount, o.TurnCount, o.SessionTokens, err = database.GetSessionStats()
	warn("session stats", err)

	o.FirstActivity, o.LastActivity, err = database.GetFirstAndLastActivity()
	warn("activity range", err)

	o.ToolStats, err = database.GetToolUsageStats(5)
	warn("tool stats", err)

	o.TokensByModel, err = database.GetTokensByModel()
	warn("model stats", err)

	projects, err := database.GetProjects("activity", 5)
	warn("recent projects", err)
	for _, p := range projects {
		o.ProjectNames = append(o.ProjectNames, p.DisplayName)
	}

	return o
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/ccvault/ -v`
Expected: PASS (both tests).

- [ ] **Step 5: Rewire `orientCmd` RunE onto the struct**

In the RunE, replace the gathering block (lines ~147-159):

```go
		// Gather stats
		projectCount, projectTokens, _ := database.GetProjectStats()
		sessionCount, turnCount, sessionTokens, _ := database.GetSessionStats()
		firstActivity, lastActivity, _ := database.GetFirstAndLastActivity()
		toolStats, _ := database.GetToolUsageStats(5)
		tokensByModel, _ := database.GetTokensByModel()

		// Get recent projects
		projects, _ := database.GetProjects("activity", 5)
		projectNames := make([]string, 0, len(projects))
		for _, p := range projects {
			projectNames = append(projectNames, p.DisplayName)
		}
```

with:

```go
		o := gatherOrientation(database)
```

Then update every later reference in the RunE to read from `o` (the surrounding code is otherwise unchanged):

- `orientation := map[string]interface{}{` → rename the local to `orientationMap := map[string]interface{}{` (the type now owns the name `orientation`)
- `"projects":     projectCount,` → `"projects":     o.ProjectCount,`
- `"sessions":     sessionCount,` → `"sessions":     o.SessionCount,`
- `"turns":        turnCount,` → `"turns":        o.TurnCount,`
- `"total_tokens": sessionTokens,` → `"total_tokens": o.SessionTokens,`
- `"first_session": firstActivity.Format(time.RFC3339),` → `"first_session": o.FirstActivity.Format(time.RFC3339),`
- `"last_session":  lastActivity.Format(time.RFC3339),` → `"last_session":  o.LastActivity.Format(time.RFC3339),`
- `"days_span":     int(lastActivity.Sub(firstActivity).Hours() / 24),` → `"days_span":     int(o.LastActivity.Sub(o.FirstActivity).Hours() / 24),`
- `"recent_projects": projectNames,` → `"recent_projects": o.ProjectNames,`
- `"top_tools":       toolStats,` → `"top_tools":       o.ToolStats,`
- `"models":          tokensByModel,` → `"models":          o.TokensByModel,`
- Both `if sessionCount == 0 {` occurrences → `if o.SessionCount == 0 {`
- `orientation["status"] = "empty"` / `orientation["hint"] = ...` / `enc.Encode(orientation)` / `orientation["status"]` → same with `orientationMap`
- In the human-readable section: `projectCount`→`o.ProjectCount`, `sessionCount`→`o.SessionCount`, `turnCount`→`o.TurnCount`, `formatTokens(sessionTokens)`→`formatTokens(o.SessionTokens)`, `firstActivity`→`o.FirstActivity`, `lastActivity`→`o.LastActivity` (three lines), `projectNames`→`o.ProjectNames` (two places), `tokensByModel`→`o.TokensByModel`
- Delete the trailing wart near line 267 — these two lines go away entirely:

```go
		// Suppress unused variable warning
		_ = projectTokens
```

Directly after the `orientationMap := map[string]interface{}{...}` literal closes (before the empty-database check), insert:

```go
		if len(o.Warnings) > 0 {
			orientationMap["warnings"] = o.Warnings
		}
```

And at the start of the human-readable section (right after the `if jsonOutput { ... }` block), insert:

```go
		for _, w := range o.Warnings {
			fmt.Fprintf(os.Stderr, "warning: %s\n", w)
		}
```

- [ ] **Step 6: Run the full suite and smoke-test the command**

Run: `go test ./...`
Expected: ALL PASS.

Run: `go build -o /tmp/ccvault-check ./cmd/ccvault && /tmp/ccvault-check orient --json | head -30`
Expected: valid JSON, `"status": "ready"` (or `"empty"`), no `"warnings"` key against the healthy live DB.

- [ ] **Step 7: Commit**

```bash
git add cmd/ccvault/main.go cmd/ccvault/main_test.go
git commit -m "fix: collect orient stat-query errors into warnings"
```

---

### Task 6: scenarios.jsonl coverage guard

**Files:**
- Create: `test/integration/scenarios_test.go`

**Interfaces:**
- Consumes: `scenarios.jsonl` at the repo root (13 scenarios, each a JSON object with a `name` field).
- Produces: a standing guard — adding a scenario without mapping it to a real test fails CI; deleting a scenario while leaving a stale map entry also fails.

- [ ] **Step 1: Write the guard with an empty coverage map (the red state)**

Create `test/integration/scenarios_test.go`:

```go
// ABOUTME: Guard test that keeps scenarios.jsonl mapped to actual test coverage
// ABOUTME: Fails when a scenario has no verification entry or an entry goes stale

package integration

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// scenarioCoverage maps each scenario name in scenarios.jsonl to where it
// is verified. Adding a scenario without a coverage entry fails this test;
// so does removing a scenario and leaving its entry behind.
var scenarioCoverage = map[string]string{}

func TestScenariosHaveCoverage(t *testing.T) {
	f, err := os.Open("../../scenarios.jsonl")
	if err != nil {
		t.Fatalf("open scenarios.jsonl: %v", err)
	}
	defer func() { _ = f.Close() }()

	fromFile := make(map[string]bool)
	scanner := bufio.NewScanner(f)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		var s struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal([]byte(text), &s); err != nil {
			t.Fatalf("scenarios.jsonl line %d: %v", line, err)
		}
		if s.Name == "" {
			t.Fatalf("scenarios.jsonl line %d: missing name", line)
		}
		fromFile[s.Name] = true
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read scenarios.jsonl: %v", err)
	}

	for name := range fromFile {
		if _, ok := scenarioCoverage[name]; !ok {
			t.Errorf("scenario %q has no coverage entry — add a test, then map it in scenarioCoverage", name)
		}
	}
	for name := range scenarioCoverage {
		if !fromFile[name] {
			t.Errorf("stale coverage entry %q — scenario no longer exists in scenarios.jsonl", name)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./test/integration/ -run TestScenariosHaveCoverage -v`
Expected: FAIL with 13 "has no coverage entry" errors, one per scenario.

- [ ] **Step 3: Fill in the coverage map**

Replace the empty `scenarioCoverage` literal with the verified mapping (every referenced test exists today):

```go
var scenarioCoverage = map[string]string{
	"sync-real-claude-code":       "internal/sync TestRunWithClaudeCodeAdapter; TestMultiSourceSyncAndSearch (claude-code fixture)",
	"sync-real-codex":             "TestMultiSourceSyncAndSearch (codex fixture)",
	"multi-source-sync":           "TestMultiSourceSyncAndSearch; internal/sync TestMultipleSourcesSync",
	"source-filtered-search":      "TestMultiSourceSyncAndSearch (source:codex / source:claude-code assertions)",
	"incremental-sync":            "internal/sync TestIncrementalSyncSkipsUnchanged + TestNeedsSyncWithMtimeMap",
	"migration-bootstrap":         "internal/db TestProjectSourceDefaultsToClaudeCode + TestSessionSourceDefaultsToClaudeCode",
	"adapter-registry":            "pkg/adapter registry_test.go",
	"backward-compat-config":      "internal/config config_test.go",
	"binary-builds":               "make build (Makefile target); go test ./... compiles all packages",
	"sync-real-jeff":              "TestMultiSourceSyncAndSearch (jeff fixture)",
	"sync-real-hex":               "TestMultiSourceSyncAndSearch (hex fixture)",
	"source-filtered-search-jeff": "TestMultiSourceSyncAndSearch (source:jeff assertions)",
	"fts-across-sources":          "TestMultiSourceSyncAndSearch (unfiltered 'hello' reaches all four sources)",
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./test/integration/ -v`
Expected: ALL PASS (guard + existing multisource test).

- [ ] **Step 5: Run the full suite**

Run: `go test ./...`
Expected: ALL PASS.

- [ ] **Step 6: Commit**

```bash
git add test/integration/scenarios_test.go
git commit -m "test: guard scenarios.jsonl against coverage drift"
```

---

### Task 7: Final verification gate

**Files:** none (verification only)

- [ ] **Step 1: Race detector**

Run: `make test-race`
Expected: ALL PASS, exit 0.

- [ ] **Step 2: Linter**

Run: `make lint`
Expected: no findings, exit 0. If golangci-lint flags anything in changed code, fix the root cause (never suppress) and amend the relevant commit's follow-up as a new commit.

- [ ] **Step 3: Real-usage smoke test against the live archive (read-only)**

```bash
go build -o /tmp/ccvault-check ./cmd/ccvault
/tmp/ccvault-check orient --json | jq '.status, .warnings'
/tmp/ccvault-check search 'tool:bash' --limit 3
/tmp/ccvault-check search 'tool:mcp__chronicle__remember_this' --limit 3
```

Expected: `"ready"` + `null` warnings; the lowercase `tool:bash` search returns results (it returned nothing before this PR); the MCP-prefixed search returns results.
