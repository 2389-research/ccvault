// ABOUTME: Tests that search reaches inside subagent transcripts and labels the hits.
// ABOUTME: Search is the one surface that is never filtered by the subagent default.

package search

import (
	"testing"
	"time"

	"github.com/2389-research/ccvault/internal/db"
	"github.com/2389-research/ccvault/pkg/models"
)

func setupSubagentSearchDB(t *testing.T) *db.DB {
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

	now := time.Now()
	sessions := []*models.Session{
		{ID: "parent-a", ProjectID: p.ID, StartedAt: now, SourceFile: "/parent-a.jsonl"},
		{
			ID:              "claude-code:parent-a:agent-a1",
			ProjectID:       p.ID,
			StartedAt:       now.Add(time.Minute),
			SourceFile:      "/parent-a/subagents/agent-a1.jsonl",
			ParentSessionID: "parent-a",
		},
	}
	for _, s := range sessions {
		if err := database.UpsertSession(s); err != nil {
			t.Fatalf("upsert session %s: %v", s.ID, err)
		}
	}

	turns := []models.Turn{
		{ID: "p-turn-1", SessionID: "parent-a", Type: "user", Timestamp: now, Content: "dispatch the pelican work"},
		{ID: "s-turn-1", SessionID: "claude-code:parent-a:agent-a1", Type: "assistant", Timestamp: now.Add(time.Minute), Content: "found the pelican in config.go"},
	}
	if err := database.InsertTurns(turns); err != nil {
		t.Fatalf("insert turns: %v", err)
	}

	return database
}

// TestSearchIsNeverFilteredBySubagentDefault: a hit inside a subagent
// transcript comes back like any other, and carries the parent id so a render
// can label it.
func TestSearchIsNeverFilteredBySubagentDefault(t *testing.T) {
	database := setupSubagentSearchDB(t)
	searcher := New(database.DB)

	results, err := searcher.Search(&Query{Text: "pelican"}, 20)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2 (one in the parent, one in the subagent)", len(results))
	}

	bySession := map[string]Result{}
	for _, r := range results {
		bySession[r.SessionID] = r
	}

	child, ok := bySession["claude-code:parent-a:agent-a1"]
	if !ok {
		t.Fatal("the hit inside the subagent transcript was filtered out")
	}
	if child.ParentSessionID != "parent-a" {
		t.Errorf("subagent hit ParentSessionID = %q, want parent-a", child.ParentSessionID)
	}
	if parent := bySession["parent-a"]; parent.ParentSessionID != "" {
		t.Errorf("top-level hit ParentSessionID = %q, want empty", parent.ParentSessionID)
	}
}
