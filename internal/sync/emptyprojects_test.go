// ABOUTME: Tests that a sync prunes project rows no session references any more.
// ABOUTME: Completes #156's repair: the 37 worktree rows the re-parsed subagents leave behind.

package sync

import (
	"context"
	"testing"

	"github.com/2389-research/ccvault/internal/config"
	"github.com/2389-research/ccvault/pkg/models"

	// Register the claude-code adapter.
	_ "github.com/2389-research/ccvault/pkg/adapter/claudecode"
)

// TestSyncPrunesProjectsNoSessionReferences is the second half of the #156
// repair. Migration 015 forgets the mtime of every misfiled subagent
// transcript, the re-parse moves those sessions onto their parent's project,
// and this is what disposes of the rows they came from — 37 of them on the
// author's archive, each holding exactly one dispatch.
//
// Standing in for the migration's output: a project row with the counters a
// re-parse leaves behind and nothing filed under it. UpsertProject maintains
// session_count additively, so the stale count is part of the condition, not
// decoration.
func TestSyncPrunesProjectsNoSessionReferences(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	claudeHome := t.TempDir()
	writeTestSession(t, claudeHome, "9c9c9c9c-1111-2222-3333-444444444444", "-Users-test-liveproject")

	abandoned := &models.Project{
		Path:         "/Users/test/liveproject/.claude/worktrees/agent-a00fffcae156bea14",
		DisplayName:  "agent-a00fffcae156bea14",
		SessionCount: 1,
		TotalTokens:  476,
	}
	if err := database.UpsertProject(abandoned); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}

	syncer := New(database, []config.SourceConfig{
		{Name: "claude-code", Type: "claude-code", Path: claudeHome},
	}, WithFullSync(true))
	if _, err := syncer.Run(context.Background()); err != nil {
		t.Fatalf("Run(): %v", err)
	}

	projects, err := database.GetProjects("name", 100, 0)
	if err != nil {
		t.Fatalf("GetProjects(): %v", err)
	}
	paths := make([]string, 0, len(projects))
	for _, p := range projects {
		paths = append(paths, p.Path)
	}
	if len(projects) != 1 || projects[0].Path != "/Users/test/liveproject" {
		t.Fatalf("want only the project with sessions in it, got %v", paths)
	}
}

// A project whose sessions were all skipped this run is not empty — its rows
// are still in the sessions table — so a sync that wrote nothing must not take
// it with it. This is the case that makes the prune safe to run on every sync
// rather than only after a repair.
func TestSyncKeepsProjectsWhoseSessionsWereSkipped(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	claudeHome := t.TempDir()
	writeTestSession(t, claudeHome, "9d9d9d9d-1111-2222-3333-444444444444", "-Users-test-liveproject")

	sources := []config.SourceConfig{
		{Name: "claude-code", Type: "claude-code", Path: claudeHome},
	}
	if _, err := New(database, sources).Run(context.Background()); err != nil {
		t.Fatalf("first Run(): %v", err)
	}

	// Second run: nothing on disk changed, so every file is skipped and
	// projectsTouched comes back empty.
	stats, err := New(database, sources).Run(context.Background())
	if err != nil {
		t.Fatalf("second Run(): %v", err)
	}
	if stats.SessionsIndexed != 0 {
		t.Fatalf("second run indexed %d sessions, want 0 — the fixture did not exercise the skip path", stats.SessionsIndexed)
	}

	projects, err := database.GetProjects("name", 100, 0)
	if err != nil {
		t.Fatalf("GetProjects(): %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("want the project to survive a sync that wrote nothing, got %d rows", len(projects))
	}
}
