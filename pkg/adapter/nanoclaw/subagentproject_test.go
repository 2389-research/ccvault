// ABOUTME: Holds that a nanoclaw subagent's project comes from its group, not from the cwd in its transcript.
// ABOUTME: Companion to the claude-code fix for #156, where the cwd was authoritative and a worktree cwd split the project.

package nanoclaw

import (
	"os"
	"path/filepath"
	"testing"
)

// TestNanoclawSubagentIgnoresWorktreeCWD is the check that said nanoclaw does
// not need the claude-code fix. nanoclaw derives a project from the group
// directory a session lives under and never reads cwd, so an agent worktree
// cwd cannot split the project — this pins that, because the two adapters
// have drifted apart on subagent handling before.
func TestNanoclawSubagentIgnoresWorktreeCWD(t *testing.T) {
	const (
		group      = "reed"
		parentUUID = "11111111-2222-3333-4444-555555555555"
		agentID    = "agent-abc123"
	)

	root := t.TempDir()
	projectDir := filepath.Join(root, group, ".claude", "projects", "-workspace-group")
	subDir := filepath.Join(projectDir, parentUUID, "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// cwd is a throwaway agent worktree, which is what misfiled 36 of the
	// author's claude-code subagents.
	const worktree = "/workspace/group/.claude/worktrees/agent-abc123"
	subPath := filepath.Join(subDir, agentID+".jsonl")
	lines := `{"uuid":"s1","sessionId":"` + parentUUID + `","isSidechain":true,"type":"user","timestamp":"2026-05-26T14:36:22Z","cwd":"` + worktree + `","message":{"role":"user","content":"go"}}` + "\n"
	if err := os.WriteFile(subPath, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}

	parsed, err := New().Parse(subPath)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if parsed.ProjectPath != "nanoclaw/"+group {
		t.Errorf("ProjectPath = %q, want %q", parsed.ProjectPath, "nanoclaw/"+group)
	}
}
