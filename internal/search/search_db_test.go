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

// TestSearch_ProjectFilterEscapesLikeWildcards guards against SQL LIKE
// wildcard leakage flagged by adversarial review: a project: filter of
// "foo%bar" should NOT expand to "foo<anything>bar" via SQLite's LIKE
// wildcards. It should match the literal string.
func TestSearch_ProjectFilterEscapesLikeWildcards(t *testing.T) {
	database := setupSearchDB(t)
	searcher := New(database.DB)

	// Add a second project whose path CONTAINS "foobar" — the exploit
	// would be a filter of "fo%ar" matching this project via wildcards.
	p2 := &models.Project{Path: "/other/foobar-project", DisplayName: "foobar-project"}
	if err := database.UpsertProject(p2); err != nil {
		t.Fatalf("upsert second project: %v", err)
	}
	s2 := &models.Session{ID: "session-2", ProjectID: p2.ID, StartedAt: time.Now(), SourceFile: "/other.jsonl"}
	if err := database.UpsertSession(s2); err != nil {
		t.Fatalf("upsert session-2: %v", err)
	}
	if err := database.InsertTurns([]models.Turn{{
		ID: "turn-2", SessionID: "session-2", Type: "user",
		Timestamp: time.Now(), Content: "run the deploy script here too",
	}}); err != nil {
		t.Fatalf("insert turn-2: %v", err)
	}

	// A literal-percent filter — must not act as a wildcard.
	// Contains substring match on "%" would match neither project
	// because their paths/display_names don't contain a literal "%".
	results, err := searcher.Search(Parse("project:fo%ar deploy"), 10)
	if err != nil {
		t.Fatalf("search escaped %%: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("filter 'fo%%ar' matched %d results, want 0 (literal '%%' should not wildcard-match foobar-project)", len(results))
	}

	// A literal-underscore filter — same story.
	underscoreResults, err := searcher.Search(Parse("project:fo_ba deploy"), 10)
	if err != nil {
		t.Fatalf("search escaped _: %v", err)
	}
	if len(underscoreResults) != 0 {
		t.Errorf("filter 'fo_ba' matched %d results, want 0 (literal '_' should not wildcard-match foobar-project)", len(underscoreResults))
	}
}
