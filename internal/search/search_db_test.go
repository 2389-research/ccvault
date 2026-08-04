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
