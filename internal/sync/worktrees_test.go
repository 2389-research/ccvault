// ABOUTME: End-to-end test that an agent worktree syncs into its parent repo's project row.
// ABOUTME: Covers #72, where 35 of 163 real project rows were ephemeral agent worktrees.

package sync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/2389-research/ccvault/internal/config"

	// Register the claude-code adapter.
	_ "github.com/2389-research/ccvault/pkg/adapter/claudecode"
)

// writeSubagentTranscript writes a sidechain transcript where Claude Code
// puts one — under the dispatching session's uuid directory, inside the
// PARENT project's encoded directory — with the worktree cwd that a
// worktree-isolated agent actually runs in.
//
// Written literally so the bytes on disk can be checked against a real
// transcript rather than inferred from map-building code.
func writeSubagentTranscript(t *testing.T, claudeHome, encodedProject, parentUUID, agentID, cwd string) {
	t.Helper()

	dir := filepath.Join(claudeHome, "projects", encodedProject, parentUUID, "subagents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}

	lines := fmt.Sprintf(`{"uuid":"%[1]s-t1","parentUuid":null,"sessionId":"%[2]s","type":"user","isSidechain":true,"timestamp":"2026-10-01T10:00:00.000Z","cwd":%[3]q,"gitBranch":"agent-branch","message":{"role":"user","content":"do the isolated work"}}
{"uuid":"%[1]s-t2","parentUuid":"%[1]s-t1","sessionId":"%[2]s","type":"assistant","isSidechain":true,"timestamp":"2026-10-01T10:00:01.000Z","cwd":%[3]q,"gitBranch":"agent-branch","message":{"id":"msg-%[1]s","model":"claude-sonnet-4-20250514","role":"assistant","content":[{"type":"text","text":"isolated work done"}],"usage":{"input_tokens":9,"output_tokens":4}}}
`, agentID, parentUUID, cwd)

	path := filepath.Join(dir, agentID+".jsonl")
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// Two worktree-isolated agents dispatched from one session must not produce
// two project rows. Before #72 each worktree cwd became its own project,
// named after an agent id that means nothing once the agent exits, and the
// repo's own session and token counts were split across them.
func TestWorktreeSubagentsJoinTheirParentProject(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	claudeHome := t.TempDir()
	const (
		encodedProject = "-Users-test-worktreerepo"
		repoPath       = "/Users/test/worktreerepo"
		parentUUID     = "7a7a7a7a-1111-2222-3333-444444444444"
	)

	writeTestSession(t, claudeHome, parentUUID, encodedProject)
	writeSubagentTranscript(t, claudeHome, encodedProject, parentUUID,
		"agent-aaaa1111bbbb2222c", repoPath+"/.claude/worktrees/agent-aaaa1111bbbb2222c")
	writeSubagentTranscript(t, claudeHome, encodedProject, parentUUID,
		"agent-cccc3333dddd4444e", repoPath+"/.claude/worktrees/agent-cccc3333dddd4444e")

	syncer := New(database, []config.SourceConfig{
		{Name: "claude-code", Type: "claude-code", Path: claudeHome},
	}, WithFullSync(true))
	if _, err := syncer.Run(context.Background()); err != nil {
		t.Fatalf("Run(): %v", err)
	}

	projects, err := database.GetProjects("last_activity", 100, 0)
	if err != nil {
		t.Fatalf("GetProjects(): %v", err)
	}
	if len(projects) != 1 {
		paths := make([]string, 0, len(projects))
		for _, p := range projects {
			paths = append(paths, p.Path)
		}
		t.Fatalf("want 1 project row for the repo, got %d: %v", len(projects), paths)
	}
	if projects[0].Path != repoPath {
		t.Errorf("project path = %q, want the repo %q", projects[0].Path, repoPath)
	}
	if projects[0].DisplayName != "worktreerepo" {
		t.Errorf("display name = %q, want the repo's basename, not an agent id", projects[0].DisplayName)
	}

	// The sessions still exist as their own rows — the worktree collapses the
	// project identity, not the session.
	if projects[0].SessionCount != 3 {
		t.Errorf("session count = %d, want 3 (one parent plus two subagents)", projects[0].SessionCount)
	}

	// The worktree branch is still recorded, which is what makes the
	// discarded cwd recoverable rather than lost.
	var branch string
	if err := database.QueryRow(
		`SELECT git_branch FROM sessions WHERE id LIKE '%agent-aaaa1111bbbb2222c'`).Scan(&branch); err != nil {
		t.Fatalf("scan subagent branch: %v", err)
	}
	if branch != "agent-branch" {
		t.Errorf("subagent git_branch = %q, want the worktree's branch", branch)
	}
}

// The counterpart that stops this being a blanket "subagents inherit their
// parent" rule: a subagent whose cwd is a genuinely different directory keeps
// that directory as its project.
func TestSubagentOutsideAWorktreeKeepsItsOwnProject(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	claudeHome := t.TempDir()
	const (
		encodedProject = "-Users-test-dispatcher"
		parentUUID     = "8b8b8b8b-1111-2222-3333-444444444444"
		elsewhere      = "/Users/test/some-other-repo"
	)

	writeTestSession(t, claudeHome, parentUUID, encodedProject)
	writeSubagentTranscript(t, claudeHome, encodedProject, parentUUID,
		"agent-eeee5555ffff6666a", elsewhere)

	syncer := New(database, []config.SourceConfig{
		{Name: "claude-code", Type: "claude-code", Path: claudeHome},
	}, WithFullSync(true))
	if _, err := syncer.Run(context.Background()); err != nil {
		t.Fatalf("Run(): %v", err)
	}

	projects, err := database.GetProjects("last_activity", 100, 0)
	if err != nil {
		t.Fatalf("GetProjects(): %v", err)
	}
	if len(projects) != 2 {
		paths := make([]string, 0, len(projects))
		for _, p := range projects {
			paths = append(paths, p.Path)
		}
		t.Fatalf("want 2 project rows, got %d: %v", len(projects), paths)
	}

	found := false
	for _, p := range projects {
		if p.Path == elsewhere {
			found = true
		}
	}
	if !found {
		t.Errorf("a subagent that ran outside a worktree lost its own project identity; rows: %+v", projects)
	}
}
