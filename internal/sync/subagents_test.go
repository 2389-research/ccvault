// ABOUTME: End-to-end sync test: subagent transcripts land as their own session rows.
// ABOUTME: Real JSONL fixtures, real SQLite, no mocks — the whole point is the write path.

package sync

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/2389-research/ccvault/internal/config"
	"github.com/2389-research/ccvault/internal/db"
	"github.com/2389-research/ccvault/pkg/models"
)

const syncProbeParent = "04fb5717-c508-4503-ac85-dc11787cafaa"

// writeSubagentFixture lays down one parent transcript and two subagent
// transcripts under it, in the layout Claude Code actually writes. Both
// subagent files carry the parent's sessionId, which is what makes the id
// collision real rather than theoretical.
func writeSubagentFixture(t *testing.T, claudeHome string) (parentFile string, subagentFiles []string) {
	t.Helper()

	projectDir := filepath.Join(claudeHome, "projects", "-Users-test-myproject")
	subDir := filepath.Join(projectDir, syncProbeParent, "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}

	parentFile = filepath.Join(projectDir, syncProbeParent+".jsonl")
	parentLines := `{"uuid":"p1","sessionId":"` + syncProbeParent + `","type":"user","timestamp":"2026-09-28T18:00:00Z","cwd":"/Users/test/myproject","message":{"role":"user","content":"go"}}` + "\n" +
		`{"uuid":"p2","parentUuid":"p1","sessionId":"` + syncProbeParent + `","type":"assistant","timestamp":"2026-09-28T18:00:05Z","cwd":"/Users/test/myproject","message":{"id":"m1","model":"claude-opus-4","role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Task","input":{}}],"usage":{"input_tokens":10,"output_tokens":20}}}` + "\n"
	if err := os.WriteFile(parentFile, []byte(parentLines), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, agent := range []string{"agent-a01b71e80ea28b3ad", "agent-ab685cc44d883a956"} {
		path := filepath.Join(subDir, agent+".jsonl")
		lines := `{"uuid":"` + agent + `-s1","isSidechain":true,"sessionId":"` + syncProbeParent + `","type":"user","timestamp":"2026-09-28T18:08:48Z","cwd":"/Users/test/myproject","message":{"role":"user","content":"sub work"}}` + "\n" +
			`{"uuid":"` + agent + `-s2","parentUuid":"` + agent + `-s1","isSidechain":true,"sessionId":"` + syncProbeParent + `","type":"assistant","timestamp":"2026-09-28T18:09:00Z","cwd":"/Users/test/myproject","message":{"id":"m2","model":"claude-opus-4","role":"assistant","content":[{"type":"text","text":"done"}],"usage":{"input_tokens":100,"output_tokens":200}}}` + "\n"
		if err := os.WriteFile(path, []byte(lines), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(subDir, agent+".meta.json"),
			[]byte(`{"agentType":"general-purpose","toolUseId":"toolu_1","spawnDepth":1}`), 0o644); err != nil {
			t.Fatal(err)
		}
		subagentFiles = append(subagentFiles, path)
	}

	return parentFile, subagentFiles
}

func TestSyncIndexesSubagentTranscripts(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	claudeHome := t.TempDir()
	parentFile, subagentFiles := writeSubagentFixture(t, claudeHome)

	syncer := New(database, []config.SourceConfig{
		{Name: "claude-code", Type: "claude-code", Path: claudeHome},
	})
	stats, err := syncer.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(stats.Errors) != 0 {
		t.Fatalf("sync errors: %v", stats.Errors)
	}
	if stats.SessionsIndexed != 3 {
		t.Errorf("SessionsIndexed = %d, want 3 (one parent + two subagents)", stats.SessionsIndexed)
	}

	// The parent survived: its row still holds its own turns, not a
	// subagent's. This is the collision guard at the sync level.
	parent, err := database.GetSession(syncProbeParent)
	if err != nil {
		t.Fatalf("get parent: %v", err)
	}
	if parent == nil {
		t.Fatal("parent session missing")
	}
	if parent.SourceFile != parentFile {
		t.Errorf("parent source_file = %q, want %q", parent.SourceFile, parentFile)
	}
	if parent.TurnCount != 2 {
		t.Errorf("parent turn_count = %d, want 2", parent.TurnCount)
	}
	if parent.ParentSessionID != "" {
		t.Errorf("parent ParentSessionID = %q, want empty", parent.ParentSessionID)
	}
	if parent.SubagentCount != 2 {
		t.Errorf("parent SubagentCount = %d, want 2", parent.SubagentCount)
	}

	// Each subagent is its own row, reachable by its minted id with no flag.
	for i, agent := range []string{"agent-a01b71e80ea28b3ad", "agent-ab685cc44d883a956"} {
		id := "claude-code:" + syncProbeParent + ":" + agent
		got, err := database.GetSession(id)
		if err != nil {
			t.Fatalf("get subagent %s: %v", id, err)
		}
		if got == nil {
			t.Fatalf("subagent session %s missing", id)
		}
		if got.ParentSessionID != syncProbeParent {
			t.Errorf("%s ParentSessionID = %q, want %q", id, got.ParentSessionID, syncProbeParent)
		}
		if got.SourceFile != subagentFiles[i] {
			t.Errorf("%s source_file = %q, want %q", id, got.SourceFile, subagentFiles[i])
		}
		if got.TurnCount != 2 {
			t.Errorf("%s turn_count = %d, want 2", id, got.TurnCount)
		}
		if got.ProjectID == 0 {
			t.Errorf("%s has no project — a subagent belongs to its parent's project", id)
		}
	}

	// Default listing: parents only, with the count that earns the filter.
	hidden, err := database.QuerySessions(db.SessionQuery{Scope: db.SubagentsHidden})
	if err != nil {
		t.Fatalf("query hidden: %v", err)
	}
	if len(hidden) != 1 || hidden[0].ID != syncProbeParent {
		t.Fatalf("default listing = %v, want just the parent", sessionIDs(hidden))
	}
	if hidden[0].SubagentCount != 2 {
		t.Errorf("listed parent SubagentCount = %d, want 2", hidden[0].SubagentCount)
	}

	all, err := database.QuerySessions(db.SessionQuery{Scope: db.SubagentsIncluded})
	if err != nil {
		t.Fatalf("query included: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("flattened listing = %v, want 3 rows", sessionIDs(all))
	}

	// Analytics counts them: the subagent turns and tokens are additive,
	// because the parent transcript contains no sidechain lines at all.
	count, totalTurns, totalTokens, err := database.GetSessionStats()
	if err != nil {
		t.Fatalf("session stats: %v", err)
	}
	if count != 3 {
		t.Errorf("stats session count = %d, want 3", count)
	}
	if totalTurns != 6 {
		t.Errorf("stats total turns = %d, want 6 (2 parent + 2x2 subagent)", totalTurns)
	}
	// 30 from the parent, 300 from each subagent.
	if totalTokens != 630 {
		t.Errorf("stats total tokens = %d, want 630", totalTokens)
	}
}

// TestSyncSubagentTurnsAreSearchable pins the rule that search is never
// filtered: the work a subagent did has to be findable.
func TestSyncSubagentTurnsAreSearchable(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	claudeHome := t.TempDir()
	writeSubagentFixture(t, claudeHome)

	syncer := New(database, []config.SourceConfig{
		{Name: "claude-code", Type: "claude-code", Path: claudeHome},
	})
	if _, err := syncer.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var hits int
	err := database.QueryRow(`SELECT COUNT(*) FROM turns t
		JOIN turns_fts f ON t.rowid = f.rowid
		WHERE turns_fts MATCH 'sub' AND t.session_id LIKE 'claude-code:%'`).Scan(&hits)
	if err != nil {
		t.Fatalf("fts query: %v", err)
	}
	if hits != 2 {
		t.Errorf("fts hits inside subagent transcripts = %d, want 2", hits)
	}
}

// TestSyncSubagentIsIncremental checks the mtime bookkeeping covers subagent
// files too, so a second sync doesn't re-parse them.
func TestSyncSubagentIsIncremental(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	claudeHome := t.TempDir()
	writeSubagentFixture(t, claudeHome)

	sources := []config.SourceConfig{{Name: "claude-code", Type: "claude-code", Path: claudeHome}}
	if _, err := New(database, sources).Run(context.Background()); err != nil {
		t.Fatalf("first run: %v", err)
	}
	stats, err := New(database, sources).Run(context.Background())
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if stats.SessionsSkipped != 3 {
		t.Errorf("SessionsSkipped = %d, want 3 — subagent mtimes are not being tracked", stats.SessionsSkipped)
	}
	if stats.SessionsIndexed != 0 {
		t.Errorf("SessionsIndexed = %d, want 0 on an unchanged tree", stats.SessionsIndexed)
	}
}

func sessionIDs(sessions []models.Session) []string {
	out := make([]string, len(sessions))
	for i, s := range sessions {
		out[i] = s.ID
	}
	return out
}
