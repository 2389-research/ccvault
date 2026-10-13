// ABOUTME: Canonicalises a Claude Code agent-worktree cwd back to the repository it was cut from.
// ABOUTME: Without it each worktree becomes a project row of its own, splitting one logical project (#72).

package parser

import "strings"

// claudeWorktreeSegment is where Claude Code's worktree isolation puts an
// agent's checkout, relative to the repository root:
//
//	<repo>/.claude/worktrees/<name>
//
// The leading and trailing separators are part of the constant so it only
// matches a whole path segment — ".claude/agents" and a hand-made
// ".worktrees/evals" are both left alone.
const claudeWorktreeSegment = "/.claude/worktrees/"

// CanonicalProjectPath maps a working directory to the project it belongs to.
//
// Everything passes through unchanged except a Claude Code agent worktree,
// which resolves to the repository it was cut from. Worktree directories are
// ephemeral by construction — the name is an agent id that means nothing once
// the agent exits — so deriving a project identity from one splits a single
// logical project into a row per dispatched agent. On the archive this was
// written for, 35 of 163 project rows were worktrees, 27 of them belonging to
// the repo ccvault itself, which left `--project ccvault` unable to resolve to
// anything (#72).
//
// This is a rule about one path convention, not about subagents. A top-level
// session started inside a worktree canonicalises the same way, and a subagent
// dispatched into a genuinely different directory keeps that directory, since
// its cwd carries no worktree segment. Nothing is lost that cannot be
// recovered: the worktree's branch is still on the session, and each turn's
// raw_json still holds the cwd it was written with.
func CanonicalProjectPath(path string) string {
	i := strings.Index(path, claudeWorktreeSegment)

	// i == 0 would leave an empty project path, which is worse than an odd
	// one, so a worktree segment at the filesystem root is left alone.
	if i <= 0 {
		return path
	}

	// ".../worktrees/" naming no worktree is the container directory, not a
	// checkout inside it.
	if strings.Trim(path[i+len(claudeWorktreeSegment):], "/") == "" {
		return path
	}

	return path[:i]
}
