// ABOUTME: Tests that a Claude Code agent worktree canonicalises to the repository it was cut from.
// ABOUTME: Covers #72, where each worktree cwd produced a project row of its own.

package parser

import "testing"

func TestCanonicalProjectPath(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{{
		name: "agent worktree collapses to the repo it was cut from",
		in:   "/Users/dylanr/work/2389/ccvault/.claude/worktrees/agent-a00fffcae156bea14",
		want: "/Users/dylanr/work/2389/ccvault",
	}, {
		name: "a directory inside the worktree collapses the same way",
		in:   "/Users/dylanr/work/2389/ccvault/.claude/worktrees/agent-a00fff/internal/sync",
		want: "/Users/dylanr/work/2389/ccvault",
	}, {
		name: "trailing separator does not change the answer",
		in:   "/Users/dylanr/work/2389/ccvault/.claude/worktrees/agent-a00fff/",
		want: "/Users/dylanr/work/2389/ccvault",
	}, {
		name: "an agent dispatched from inside a worktree still lands on the repo",
		in:   "/repo/.claude/worktrees/agent-outer/.claude/worktrees/agent-inner",
		want: "/repo",
	}, {
		// A plain repo path has no worktree segment and must pass through
		// untouched — this is the overwhelmingly common case.
		name: "an ordinary project path is unchanged",
		in:   "/Users/dylanr/work/2389/ccvault",
		want: "/Users/dylanr/work/2389/ccvault",
	}, {
		// A subagent that genuinely ran in a different repo keeps that
		// identity. The rule is about one path convention, not about
		// subagents, so it cannot misattribute this case.
		name: "an unrelated directory is not collapsed",
		in:   "/Users/dylanr/work/2389/other-repo",
		want: "/Users/dylanr/work/2389/other-repo",
	}, {
		// A git worktree the user made and named themselves is not the
		// ephemeral agent convention; it keeps its own identity.
		name: "a hand-made git worktree outside .claude is left alone",
		in:   "/Users/dylanr/work/2389/skills-dev/intent/.worktrees/evals",
		want: "/Users/dylanr/work/2389/skills-dev/intent/.worktrees/evals",
	}, {
		name: "the worktrees container itself is not a worktree",
		in:   "/repo/.claude/worktrees",
		want: "/repo/.claude/worktrees",
	}, {
		name: "the container with a trailing separator names no worktree",
		in:   "/repo/.claude/worktrees/",
		want: "/repo/.claude/worktrees/",
	}, {
		// Collapsing this would leave an empty project path, which is worse
		// than an odd one.
		name: "a worktree segment at the root has no repo to collapse to",
		in:   "/.claude/worktrees/agent-a00fff",
		want: "/.claude/worktrees/agent-a00fff",
	}, {
		name: "empty stays empty",
		in:   "",
		want: "",
	}, {
		// ".claude/settings.json" and friends live under every repo; only the
		// worktrees directory is ephemeral.
		name: "other paths under .claude are not worktrees",
		in:   "/repo/.claude/agents",
		want: "/repo/.claude/agents",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CanonicalProjectPath(tt.in); got != tt.want {
				t.Errorf("CanonicalProjectPath(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
