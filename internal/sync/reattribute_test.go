// ABOUTME: Rehearses #156's whole repair end to end: forget a transcript's mtime, re-parse it, watch the session move.
// ABOUTME: The session row has to follow the corrected attribution, which is what sessions.project_id being updatable buys.

package sync

import (
	"context"
	"testing"

	"github.com/2389-research/ccvault/internal/config"

	// Register the claude-code adapter.
	_ "github.com/2389-research/ccvault/pkg/adapter/claudecode"
)

// TestReparseMovesASessionToItsCorrectedProject is migration 015's repair,
// run for real against the sync path rather than asserted in SQL.
//
// Act one builds the archive the author has: a subagent filed under the
// directory it ran in, because at the time it was parsed there was no parent
// transcript to inherit from. Act two is the repair — the parent appears, the
// transcript's stored mtime is forgotten the way migration 015 forgets it, and
// an ordinary sync re-reads exactly that one file.
//
// The assertion that failed before sessions.project_id became updatable: the
// re-parse computed the right project and wrote the row, and the row kept the
// project it already had. 42 sessions' worth of repair was silently inert.
func TestReparseMovesASessionToItsCorrectedProject(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	claudeHome := t.TempDir()
	const (
		encodedProject = "-Users-test-dispatcher"
		dispatcherPath = "/Users/test/dispatcher"
		parentUUID     = "aeaeaeae-1111-2222-3333-444444444444"
		agentID        = "agent-abcd1234abcd1234a"
		worktree       = dispatcherPath + "/.worktrees/ws-isolated"
	)
	sources := []config.SourceConfig{
		{Name: "claude-code", Type: "claude-code", Path: claudeHome},
	}

	// Act one. No parent transcript, so the subagent's own cwd decides and
	// the worktree becomes a project row of its own.
	writeSubagentTranscript(t, claudeHome, encodedProject, parentUUID, agentID, worktree)
	if _, err := New(database, sources).Run(context.Background()); err != nil {
		t.Fatalf("first Run(): %v", err)
	}

	subagentID := "claude-code:" + parentUUID + ":" + agentID
	var projectPath string
	if err := database.QueryRow(
		`SELECT p.path FROM sessions s JOIN projects p ON p.id = s.project_id WHERE s.id = ?`,
		subagentID).Scan(&projectPath); err != nil {
		t.Fatalf("scan subagent project: %v", err)
	}
	if projectPath != worktree {
		t.Fatalf("setup did not reproduce the misfiling: subagent is on %q, want %q", projectPath, worktree)
	}

	// Act two. The parent transcript is there to inherit from now, and the
	// stored mtime goes the way migration 015 sends it.
	writeTestSession(t, claudeHome, parentUUID, encodedProject)
	subagentPath := claudeHome + "/projects/" + encodedProject + "/" + parentUUID + "/subagents/" + agentID + ".jsonl"
	if _, err := database.Exec(
		`DELETE FROM source_files WHERE path = ? AND source = 'claude-code'`, subagentPath); err != nil {
		t.Fatalf("forget mtime: %v", err)
	}

	stats, err := New(database, sources).Run(context.Background())
	if err != nil {
		t.Fatalf("second Run(): %v", err)
	}

	if err := database.QueryRow(
		`SELECT p.path FROM sessions s JOIN projects p ON p.id = s.project_id WHERE s.id = ?`,
		subagentID).Scan(&projectPath); err != nil {
		t.Fatalf("scan subagent project after repair: %v", err)
	}
	if projectPath != dispatcherPath {
		t.Errorf("subagent still on %q after the re-parse, want its parent's project %q", projectPath, dispatcherPath)
	}

	// The row it came from is gone, and the sync says so rather than leaving
	// the user to notice.
	projects, err := database.GetProjects("name", 100, 0)
	if err != nil {
		t.Fatalf("GetProjects(): %v", err)
	}
	paths := make([]string, 0, len(projects))
	for _, p := range projects {
		paths = append(paths, p.Path)
	}
	if len(projects) != 1 || projects[0].Path != dispatcherPath {
		t.Errorf("projects = %v, want only %q", paths, dispatcherPath)
	}
	if stats.ProjectsPruned != 1 {
		t.Errorf("ProjectsPruned = %d, want 1", stats.ProjectsPruned)
	}

	// And the surviving row's counters count the two sessions it now holds,
	// not the three UpsertProject's additive write would have accumulated.
	if projects[0].SessionCount != 2 {
		t.Errorf("session count = %d, want 2 (the dispatcher plus the subagent it reclaimed)", projects[0].SessionCount)
	}
}
