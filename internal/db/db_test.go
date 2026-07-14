// ABOUTME: Integration tests for database operations
// ABOUTME: Tests CRUD operations and FTS5 search functionality

package db

import (
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/2389-research/ccvault/pkg/models"
)

func setupTestDB(t *testing.T) (*DB, func()) {
	t.Helper()

	tmpDir, err := os.MkdirTemp("", "ccvault-test-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}

	db, err := Open(tmpDir)
	if err != nil {
		_ = os.RemoveAll(tmpDir)
		t.Fatalf("open db: %v", err)
	}

	cleanup := func() {
		_ = db.Close()
		_ = os.RemoveAll(tmpDir)
	}

	return db, cleanup
}

func TestOpenAndClose(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	// Verify database file exists
	if _, err := os.Stat(db.Path()); os.IsNotExist(err) {
		t.Error("database file not created")
	}

	// Verify tables exist
	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='projects'").Scan(&count)
	if err != nil {
		t.Fatalf("query tables: %v", err)
	}
	if count != 1 {
		t.Error("projects table not created")
	}
}

func TestProjectCRUD(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	// Create project
	p := &models.Project{
		Path:           "/Users/test/project",
		DisplayName:    "test/project",
		FirstSeenAt:    time.Now(),
		LastActivityAt: time.Now(),
		SessionCount:   1,
		TotalTokens:    1000,
	}

	err := db.UpsertProject(p)
	if err != nil {
		t.Fatalf("upsert project: %v", err)
	}

	if p.ID == 0 {
		t.Error("project ID not set after upsert")
	}

	// Get project
	got, err := db.GetProject(p.ID)
	if err != nil {
		t.Fatalf("get project: %v", err)
	}

	if got.Path != p.Path {
		t.Errorf("path = %s, want %s", got.Path, p.Path)
	}

	// Get by path
	got, err = db.GetProjectByPath(p.Path)
	if err != nil {
		t.Fatalf("get project by path: %v", err)
	}

	if got.ID != p.ID {
		t.Errorf("id = %d, want %d", got.ID, p.ID)
	}

	// List projects
	projects, err := db.GetProjects("activity", 10)
	if err != nil {
		t.Fatalf("get projects: %v", err)
	}

	if len(projects) != 1 {
		t.Errorf("len(projects) = %d, want 1", len(projects))
	}
}

func TestProjectUpsertAccumulates(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	// First insert
	p := &models.Project{
		Path:           "/Users/test/project",
		DisplayName:    "test/project",
		FirstSeenAt:    time.Now(),
		LastActivityAt: time.Now(),
		SessionCount:   1,
		TotalTokens:    1000,
	}
	_ = db.UpsertProject(p)

	// Second insert (simulating another session)
	p2 := &models.Project{
		Path:           "/Users/test/project",
		DisplayName:    "test/project",
		FirstSeenAt:    time.Now(),
		LastActivityAt: time.Now().Add(time.Hour),
		SessionCount:   1,
		TotalTokens:    500,
	}
	_ = db.UpsertProject(p2)

	// Verify accumulation
	got, _ := db.GetProjectByPath(p.Path)
	if got.SessionCount != 2 {
		t.Errorf("session_count = %d, want 2", got.SessionCount)
	}
	if got.TotalTokens != 1500 {
		t.Errorf("total_tokens = %d, want 1500", got.TotalTokens)
	}
}

func TestSessionCRUD(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	// Create project first
	p := &models.Project{
		Path:           "/Users/test/project",
		DisplayName:    "test/project",
		FirstSeenAt:    time.Now(),
		LastActivityAt: time.Now(),
	}
	_ = db.UpsertProject(p)

	// Create session
	s := &models.Session{
		ID:           "test-session-123",
		ProjectID:    p.ID,
		StartedAt:    time.Now(),
		EndedAt:      time.Now().Add(time.Hour),
		Model:        "claude-opus-4-5-20251101",
		GitBranch:    "main",
		TurnCount:    10,
		InputTokens:  500,
		OutputTokens: 300,
		SourceFile:   "/test/session.jsonl",
	}

	err := db.UpsertSession(s)
	if err != nil {
		t.Fatalf("upsert session: %v", err)
	}

	// Get session
	got, err := db.GetSession(s.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}

	if got.Model != s.Model {
		t.Errorf("model = %s, want %s", got.Model, s.Model)
	}

	// List sessions
	sessions, err := db.GetSessions(p.ID, 10)
	if err != nil {
		t.Fatalf("get sessions: %v", err)
	}

	if len(sessions) != 1 {
		t.Errorf("len(sessions) = %d, want 1", len(sessions))
	}
}

func TestTurnCRUD(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	// Setup project and session
	p := &models.Project{Path: "/test", DisplayName: "test"}
	_ = db.UpsertProject(p)

	s := &models.Session{
		ID:         "session-1",
		ProjectID:  p.ID,
		StartedAt:  time.Now(),
		SourceFile: "/test.jsonl",
	}
	_ = db.UpsertSession(s)

	// Insert turns
	turns := []models.Turn{
		{
			ID:        "turn-1",
			SessionID: s.ID,
			Type:      "user",
			Timestamp: time.Now(),
			Content:   "Hello, can you help me with Go programming?",
		},
		{
			ID:        "turn-2",
			SessionID: s.ID,
			ParentID:  "turn-1",
			Type:      "assistant",
			Timestamp: time.Now().Add(time.Second),
			Content:   "Of course! I'd be happy to help with Go programming.",
		},
	}

	err := db.InsertTurns(turns)
	if err != nil {
		t.Fatalf("insert turns: %v", err)
	}

	// Get turns
	got, err := db.GetTurns(s.ID)
	if err != nil {
		t.Fatalf("get turns: %v", err)
	}

	if len(got) != 2 {
		t.Errorf("len(turns) = %d, want 2", len(got))
	}

	if got[0].Content != turns[0].Content {
		t.Errorf("content = %s, want %s", got[0].Content, turns[0].Content)
	}
}

func TestFullTextSearch(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	// Setup
	p := &models.Project{Path: "/test", DisplayName: "test"}
	_ = db.UpsertProject(p)

	s := &models.Session{ID: "session-1", ProjectID: p.ID, StartedAt: time.Now(), SourceFile: "/test.jsonl"}
	_ = db.UpsertSession(s)

	turns := []models.Turn{
		{ID: "turn-1", SessionID: s.ID, Type: "user", Timestamp: time.Now(), Content: "How do I implement a REST API in Go?"},
		{ID: "turn-2", SessionID: s.ID, Type: "assistant", Timestamp: time.Now(), Content: "You can use the net/http package or a framework like Gin."},
		{ID: "turn-3", SessionID: s.ID, Type: "user", Timestamp: time.Now(), Content: "What about database connections?"},
	}
	_ = db.InsertTurns(turns)

	// Search
	results, err := db.SearchTurns("REST API", 10)
	if err != nil {
		t.Fatalf("search turns: %v", err)
	}

	if len(results) == 0 {
		t.Error("expected search results, got none")
	}

	// Search for Go
	results, err = db.SearchTurns("Go", 10)
	if err != nil {
		t.Fatalf("search turns: %v", err)
	}

	if len(results) < 1 {
		t.Error("expected results for 'Go' search")
	}
}

func TestToolUses(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	// Setup
	p := &models.Project{Path: "/test", DisplayName: "test"}
	_ = db.UpsertProject(p)

	s := &models.Session{ID: "session-1", ProjectID: p.ID, StartedAt: time.Now(), SourceFile: "/test.jsonl"}
	_ = db.UpsertSession(s)

	toolUses := []models.ToolUse{
		{TurnID: "turn-1", SessionID: s.ID, ToolName: "Bash", FilePath: "", Timestamp: time.Now()},
		{TurnID: "turn-2", SessionID: s.ID, ToolName: "Read", FilePath: "/main.go", Timestamp: time.Now()},
		{TurnID: "turn-3", SessionID: s.ID, ToolName: "Bash", Timestamp: time.Now()},
	}

	err := db.InsertToolUses(toolUses)
	if err != nil {
		t.Fatalf("insert tool uses: %v", err)
	}

	// Get stats
	stats, err := db.GetToolUsageStats(10)
	if err != nil {
		t.Fatalf("get tool stats: %v", err)
	}

	if stats["Bash"] != 2 {
		t.Errorf("Bash count = %d, want 2", stats["Bash"])
	}

	if stats["Read"] != 1 {
		t.Errorf("Read count = %d, want 1", stats["Read"])
	}
}

func TestTransaction(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	p := &models.Project{Path: "/test", DisplayName: "test"}

	err := db.WithTx(func(tx *sql.Tx) error {
		return db.UpsertProjectTx(tx, p)
	})

	if err != nil {
		t.Fatalf("transaction: %v", err)
	}

	got, _ := db.GetProject(p.ID)
	if got == nil {
		t.Error("project not created in transaction")
	}
}

func TestGetFirstAndLastActivity(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	now := time.Now()
	earlier := now.Add(-24 * time.Hour)

	p1 := &models.Project{Path: "/test1", DisplayName: "test1", FirstSeenAt: earlier, LastActivityAt: earlier}
	p2 := &models.Project{Path: "/test2", DisplayName: "test2", FirstSeenAt: now, LastActivityAt: now}
	_ = db.UpsertProject(p1)
	_ = db.UpsertProject(p2)

	first, last, err := db.GetFirstAndLastActivity()
	if err != nil {
		t.Fatalf("get activity range: %v", err)
	}

	if first.After(earlier.Add(time.Second)) {
		t.Errorf("first activity should be around %v, got %v", earlier, first)
	}

	if last.Before(now.Add(-time.Second)) {
		t.Errorf("last activity should be around %v, got %v", now, last)
	}
}

func TestProjectSourceField(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	// Insert project with explicit source
	p := &models.Project{
		Path:           "/Users/test/codex-project",
		DisplayName:    "codex-project",
		FirstSeenAt:    time.Now(),
		LastActivityAt: time.Now(),
		SessionCount:   1,
		TotalTokens:    500,
		Source:         "codex",
	}

	err := db.UpsertProject(p)
	if err != nil {
		t.Fatalf("upsert project with source: %v", err)
	}

	// Verify source is stored via GetProject
	got, err := db.GetProject(p.ID)
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	if got.Source != "codex" {
		t.Errorf("source = %q, want %q", got.Source, "codex")
	}

	// Verify source via GetProjectByPath
	got, err = db.GetProjectByPath(p.Path)
	if err != nil {
		t.Fatalf("get project by path: %v", err)
	}
	if got.Source != "codex" {
		t.Errorf("source = %q, want %q", got.Source, "codex")
	}

	// Verify source via GetProjects
	projects, err := db.GetProjects("activity", 10)
	if err != nil {
		t.Fatalf("get projects: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("len(projects) = %d, want 1", len(projects))
	}
	if projects[0].Source != "codex" {
		t.Errorf("source = %q, want %q", projects[0].Source, "codex")
	}
}

func TestProjectSourceDefaultsToClaudeCode(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	// Insert project without setting Source (empty string)
	p := &models.Project{
		Path:           "/Users/test/default-project",
		DisplayName:    "default-project",
		FirstSeenAt:    time.Now(),
		LastActivityAt: time.Now(),
		SessionCount:   1,
		TotalTokens:    100,
	}

	err := db.UpsertProject(p)
	if err != nil {
		t.Fatalf("upsert project: %v", err)
	}

	got, err := db.GetProject(p.ID)
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	if got.Source != "claude-code" {
		t.Errorf("source = %q, want %q", got.Source, "claude-code")
	}
}

func TestSessionSourceField(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	// Create project
	p := &models.Project{
		Path:        "/Users/test/project",
		DisplayName: "test/project",
		Source:      "codex",
	}
	_ = db.UpsertProject(p)

	// Create session with explicit source
	s := &models.Session{
		ID:           "codex-session-123",
		ProjectID:    p.ID,
		StartedAt:    time.Now(),
		EndedAt:      time.Now().Add(time.Hour),
		Model:        "o3",
		TurnCount:    5,
		InputTokens:  200,
		OutputTokens: 100,
		SourceFile:   "/test/codex-session.jsonl",
		Source:       "codex",
	}

	err := db.UpsertSession(s)
	if err != nil {
		t.Fatalf("upsert session with source: %v", err)
	}

	// Verify via GetSession
	got, err := db.GetSession(s.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.Source != "codex" {
		t.Errorf("source = %q, want %q", got.Source, "codex")
	}

	// Verify via GetSessions
	sessions, err := db.GetSessions(p.ID, 10)
	if err != nil {
		t.Fatalf("get sessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("len(sessions) = %d, want 1", len(sessions))
	}
	if sessions[0].Source != "codex" {
		t.Errorf("source = %q, want %q", sessions[0].Source, "codex")
	}
}

func TestSessionSourceDefaultsToClaudeCode(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	p := &models.Project{Path: "/test", DisplayName: "test"}
	_ = db.UpsertProject(p)

	s := &models.Session{
		ID:         "default-session-1",
		ProjectID:  p.ID,
		StartedAt:  time.Now(),
		SourceFile: "/test/default.jsonl",
	}
	err := db.UpsertSession(s)
	if err != nil {
		t.Fatalf("upsert session: %v", err)
	}

	got, err := db.GetSession(s.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.Source != "claude-code" {
		t.Errorf("source = %q, want %q", got.Source, "claude-code")
	}
}

func TestGetAllSourceMtimesWithFilter(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	now := time.Now()

	// Insert source files with different sources
	err := db.UpsertSourceFileMtime("/path/claude1.jsonl", now, "claude-code")
	if err != nil {
		t.Fatalf("upsert mtime: %v", err)
	}
	err = db.UpsertSourceFileMtime("/path/claude2.jsonl", now, "claude-code")
	if err != nil {
		t.Fatalf("upsert mtime: %v", err)
	}
	err = db.UpsertSourceFileMtime("/path/codex1.jsonl", now, "codex")
	if err != nil {
		t.Fatalf("upsert mtime: %v", err)
	}

	// Get all (no filter)
	all, err := db.GetAllSourceMtimes()
	if err != nil {
		t.Fatalf("get all mtimes: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("len(all) = %d, want 3", len(all))
	}

	// Filter by claude-code
	claude, err := db.GetAllSourceMtimes("claude-code")
	if err != nil {
		t.Fatalf("get claude mtimes: %v", err)
	}
	if len(claude) != 2 {
		t.Errorf("len(claude) = %d, want 2", len(claude))
	}

	// Filter by codex
	codex, err := db.GetAllSourceMtimes("codex")
	if err != nil {
		t.Fatalf("get codex mtimes: %v", err)
	}
	if len(codex) != 1 {
		t.Errorf("len(codex) = %d, want 1", len(codex))
	}
	if _, ok := codex["/path/codex1.jsonl"]; !ok {
		t.Error("expected /path/codex1.jsonl in codex results")
	}
}

func TestUpsertProjectTxWithSource(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	p := &models.Project{
		Path:        "/test/tx-project",
		DisplayName: "tx-project",
		Source:      "codex",
	}

	err := db.WithTx(func(tx *sql.Tx) error {
		return db.UpsertProjectTx(tx, p)
	})
	if err != nil {
		t.Fatalf("transaction: %v", err)
	}

	got, _ := db.GetProject(p.ID)
	if got == nil {
		t.Fatal("project not created in transaction")
	}
	if got.Source != "codex" {
		t.Errorf("source = %q, want %q", got.Source, "codex")
	}
}

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
