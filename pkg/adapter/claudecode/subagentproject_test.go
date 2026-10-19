// ABOUTME: Holds that a subagent transcript is filed under its parent session's project, not its own cwd.
// ABOUTME: A dispatched agent's cwd is often a throwaway worktree, which used to become a project row of its own (#156).

package claudecode

import (
	"os"
	"path/filepath"
	"testing"
)

// writeSubagentTreeWithCWDs lays down the real Claude Code shape — a parent
// transcript beside a <parent-uuid>/subagents/ directory — with the parent's
// cwd and the subagent's cwd chosen independently, which is the condition the
// attribution rule has to decide.
//
// The encoded project directory name is deliberately one whose dash decode is
// wrong ("-Users-test-skills-dev-design" decodes to
// /Users/test/skills/dev/design, not /Users/test/skills-dev/design), so a fix
// that reached for the directory name instead of the parent's cwd fails here.
func writeSubagentTreeWithCWDs(t *testing.T, parentCWD, subagentCWD string) (root, parentFile, subagentFile string) {
	t.Helper()
	root = t.TempDir()
	projectDir := filepath.Join(root, "projects", "-Users-test-skills-dev-design")
	subDir := filepath.Join(projectDir, probeParentUUID, "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}

	parentFile = filepath.Join(projectDir, probeParentUUID+".jsonl")
	// Lines 1 and 2 carry no cwd on purpose: Claude Code's real transcripts
	// open with a "mode" line and a "file-history-snapshot" line, and the cwd
	// first appears on line 3 or 4. A reader that only looked at the first
	// line would find nothing.
	parentLines := `{"type":"mode","mode":"default","sessionId":"` + probeParentUUID + `"}` + "\n" +
		`{"type":"file-history-snapshot","messageId":"m0","snapshot":{}}` + "\n" +
		`{"uuid":"p1","sessionId":"` + probeParentUUID + `","type":"user","timestamp":"2026-09-28T18:00:00Z","cwd":"` + parentCWD + `","gitBranch":"main","message":{"role":"user","content":"go"}}` + "\n" +
		`{"uuid":"p2","parentUuid":"p1","sessionId":"` + probeParentUUID + `","type":"assistant","timestamp":"2026-09-28T18:00:05Z","cwd":"` + parentCWD + `","message":{"id":"m1","model":"claude-opus-4","role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Task","input":{"subagent_type":"general-purpose"}}],"usage":{"input_tokens":10,"output_tokens":20}}}` + "\n"
	if err := os.WriteFile(parentFile, []byte(parentLines), 0o644); err != nil {
		t.Fatal(err)
	}

	subagentFile = filepath.Join(subDir, "agent-a01b71e80ea28b3ad.jsonl")
	subLines := `{"uuid":"s1","parentUuid":null,"isSidechain":true,"agentId":"a01b71e80ea28b3ad","sessionId":"` + probeParentUUID + `","type":"user","timestamp":"2026-09-28T18:08:48.187Z","cwd":"` + subagentCWD + `","gitBranch":"agent-a01b71e80ea28b3ad","message":{"role":"user","content":"do the thing"}}` + "\n" +
		`{"uuid":"s2","parentUuid":"s1","isSidechain":true,"agentId":"a01b71e80ea28b3ad","sessionId":"` + probeParentUUID + `","type":"assistant","timestamp":"2026-09-28T18:09:00Z","cwd":"` + subagentCWD + `","message":{"id":"m2","model":"claude-opus-4","role":"assistant","content":[{"type":"text","text":"did the thing"}],"usage":{"input_tokens":100,"output_tokens":200}}}` + "\n"
	if err := os.WriteFile(subagentFile, []byte(subLines), 0o644); err != nil {
		t.Fatal(err)
	}

	return root, parentFile, subagentFile
}

// TestParseSubagentFollowsParentProject is the bug in #156: 36 subagent
// sessions on the author's archive were filed under 35 one-off project rows
// named after the worktree each agent ran in.
func TestParseSubagentFollowsParentProject(t *testing.T) {
	const parentCWD = "/Users/test/skills-dev/design"

	// Both spellings of an agent worktree. ".claude/worktrees/" is the one
	// Claude Code creates; ".worktrees/" is a hand-rolled convention
	// CanonicalProjectPath deliberately leaves alone, and six of the author's
	// subagents landed in one.
	for _, tc := range []struct {
		name        string
		subagentCWD string
	}{
		{"claude code worktree", parentCWD + "/.claude/worktrees/agent-a00fffcae156bea14"},
		{"hand rolled worktree", parentCWD + "/.worktrees/ws-shopping-list-read-api"},
		{"unrelated directory", "/Users/test/some/other/repo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, subagentFile := writeSubagentTreeWithCWDs(t, parentCWD, tc.subagentCWD)

			parsed, err := New().Parse(subagentFile)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if parsed.ProjectPath != parentCWD {
				t.Errorf("ProjectPath = %q, want the parent's project %q", parsed.ProjectPath, parentCWD)
			}
			if parsed.DisplayName != "design" {
				t.Errorf("DisplayName = %q, want %q", parsed.DisplayName, "design")
			}
		})
	}
}

// TestParseSubagentCanonicalisesParentCWD keeps the #72 rule underneath the
// new one: a human who ran `claude` inside a worktree himself has no parent
// transcript to inherit from, and a subagent he dispatched should still land
// on the repo rather than on the worktree.
func TestParseSubagentCanonicalisesParentCWD(t *testing.T) {
	const repo = "/Users/test/skills-dev/design"
	parentCWD := repo + "/.claude/worktrees/agent-aparent"

	// The subagent's own cwd is somewhere that canonicalises to itself, so
	// the only way to reach `repo` is through the parent.
	_, _, subagentFile := writeSubagentTreeWithCWDs(t, parentCWD, "/Users/test/elsewhere")

	parsed, err := New().Parse(subagentFile)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if parsed.ProjectPath != repo {
		t.Errorf("ProjectPath = %q, want %q — the parent's cwd canonicalises too", parsed.ProjectPath, repo)
	}
}

// TestParseSubagentWithoutParentTranscript is the fallback: nothing to inherit
// from, so the subagent's own cwd decides, canonicalised the way #72 does it.
func TestParseSubagentWithoutParentTranscript(t *testing.T) {
	const repo = "/Users/test/skills-dev/design"
	_, parentFile, subagentFile := writeSubagentTreeWithCWDs(t, repo, repo+"/.claude/worktrees/agent-a00fffcae156bea14")
	if err := os.Remove(parentFile); err != nil {
		t.Fatal(err)
	}

	parsed, err := New().Parse(subagentFile)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if parsed.ProjectPath != repo {
		t.Errorf("ProjectPath = %q, want %q from the subagent's own canonicalised cwd", parsed.ProjectPath, repo)
	}
}
