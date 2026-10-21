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

// TestReparseReconcilesTheProjectASessionLeft is the case the prune cannot
// cover: the project a session departs still holds other sessions, so it
// survives — carrying the departed session in its counters.
//
// reconcileProjects recomputes the projects a run wrote *into*. The project a
// session moved *out of* was never written to, so nothing recomputed it, and
// its session_count and total_tokens stayed inflated by a session it no longer
// holds. Unreachable before a re-parse could move a session at all, which is
// why it arrives with that change rather than before it.
func TestReparseReconcilesTheProjectASessionLeft(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	claudeHome := t.TempDir()
	const (
		encodedProject = "-Users-test-dispatcher"
		dispatcherPath = "/Users/test/dispatcher"
		parentUUID     = "afafafaf-1111-2222-3333-444444444444"
		agentID        = "agent-bbbb2222bbbb2222b"
		// A worktree of some OTHER repo, which has a session of its own, so
		// the subagent leaving it does not empty it.
		otherRepo = "/Users/test/otherrepo"
		worktree  = otherRepo + "/.worktrees/ws-isolated"
	)
	sources := []config.SourceConfig{
		{Name: "claude-code", Type: "claude-code", Path: claudeHome},
	}

	// A top-level session of the author's own, run inside that worktree. It
	// keeps the project row alive after the subagent leaves.
	writeTestSessionWithCWD(t, claudeHome, "abababab-1111-2222-3333-444444444444",
		"-Users-test-otherrepo-worktrees-ws-isolated", worktree)
	writeSubagentTranscript(t, claudeHome, encodedProject, parentUUID, agentID, worktree)
	if _, err := New(database, sources).Run(context.Background()); err != nil {
		t.Fatalf("first Run(): %v", err)
	}

	before, err := database.GetProjectByPath(worktree)
	if err != nil || before == nil {
		t.Fatalf("GetProjectByPath(%q) = %v, %v", worktree, before, err)
	}
	if before.SessionCount != 2 {
		t.Fatalf("setup: worktree project holds %d sessions, want 2 (a top-level one plus the misfiled subagent)", before.SessionCount)
	}

	// The repair: the parent appears, the mtime goes the way migration 015
	// sends it, one sync.
	writeTestSession(t, claudeHome, parentUUID, encodedProject)
	subagentPath := claudeHome + "/projects/" + encodedProject + "/" + parentUUID + "/subagents/" + agentID + ".jsonl"
	if _, err := database.Exec(
		`DELETE FROM source_files WHERE path = ? AND source = 'claude-code'`, subagentPath); err != nil {
		t.Fatalf("forget mtime: %v", err)
	}
	if _, err := New(database, sources).Run(context.Background()); err != nil {
		t.Fatalf("second Run(): %v", err)
	}

	after, err := database.GetProjectByPath(worktree)
	if err != nil || after == nil {
		t.Fatalf("the worktree project should survive, it still has a session of its own: %v, %v", after, err)
	}

	var held int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM sessions WHERE project_id = ?`, after.ID).Scan(&held); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if held != 1 {
		t.Fatalf("worktree project holds %d sessions after the repair, want 1", held)
	}
	if int(after.SessionCount) != held {
		t.Errorf("session_count = %d but the project holds %d sessions — the project a session left was never reconciled",
			after.SessionCount, held)
	}

	var heldTokens int64
	if err := database.QueryRow(`
		SELECT COALESCE(SUM(COALESCE(input_tokens, 0) + COALESCE(output_tokens, 0)
			+ COALESCE(cache_read_tokens, 0) + COALESCE(cache_write_tokens, 0)), 0)
		FROM sessions WHERE project_id = ?`, after.ID).Scan(&heldTokens); err != nil {
		t.Fatalf("sum tokens: %v", err)
	}
	if after.TotalTokens != heldTokens {
		t.Errorf("total_tokens = %d but the sessions it holds total %d", after.TotalTokens, heldTokens)
	}
}
