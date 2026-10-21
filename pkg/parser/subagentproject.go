// ABOUTME: Resolves the project a subagent transcript belongs to by reading its parent session's cwd.
// ABOUTME: A dispatched agent is not an independent unit of work, and its own cwd is often a throwaway worktree (#156).

package parser

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// maxCWDLookaheadLines bounds how far into a transcript SubagentProjectPath
// will read looking for a cwd.
//
// A Claude Code transcript does not open with a turn. The first lines are a
// "mode" record and a "file-history-snapshot", neither of which carries a cwd;
// measured across the 18 parent transcripts on the author's archive that have
// subagents, the first cwd appears on line 3 or line 4, never later. The cap
// is two orders of magnitude above that so a producer adding more preamble
// does not silently break attribution, and it exists at all so a transcript
// that carries no cwd anywhere costs a bounded read rather than a full one —
// these files run to hundreds of megabytes.
const maxCWDLookaheadLines = 200

// SubagentProjectPath returns the project path a subagent transcript belongs
// to, or "" when it cannot be determined.
//
// A subagent's project is its parent's project, never its own cwd. The cwd a
// dispatched agent reports is wherever the agent was put to work, which for an
// agent given worktree isolation is a directory named after the agent itself
// and deleted when it exits. Trusting it filed 36 of the author's subagent
// sessions — 11,724 turns — under 35 one-off project rows, and left
// `--project ccvault` resolving to 28 candidates instead of one (#156).
//
// The correct attribution is already in the path. Claude Code nests a
// subagent transcript under the session that dispatched it:
//
//	<projects>/<encoded>/<parent-uuid>/subagents/agent-<hex>.jsonl
//	<projects>/<encoded>/<parent-uuid>.jsonl        <- read for its cwd
//
// The parent's transcript is read rather than the <encoded> directory name
// decoded, because that encoding is lossy: Claude Code replaces every
// non-alphanumeric character with a dash, so "-Users-dylanr-work-2389-skills
// -dev-design" could decode to skills-dev/design, skills/dev/design or
// skills-dev-design, and only one of those is the directory that exists.
// Guessing would not merge the subagent into its parent's project row; it
// would mint a third one. The parent's cwd is the same string the parent
// session's own project row was built from, so inheriting it is what puts the
// two rows together.
//
// The result passes through CanonicalProjectPath for the case where the parent
// is itself running in a worktree — a human who ran `claude` inside one has no
// parent to inherit from, and #72's rule is what collapses him onto the repo.
//
// A subagent whose cwd is a genuinely different repository follows its parent
// too, and that is deliberate: a dispatched agent is a part of the session
// that dispatched it, not a unit of work in its own right, and the
// relationship is preserved either way by sessions.parent_session_id. The cwd
// itself is not lost — every turn's raw_json still holds the one it was
// written with.
func SubagentProjectPath(subagentPath string) string {
	parent := subagentParentTranscript(subagentPath)
	if parent == "" {
		return ""
	}
	cwd := firstCWD(parent)
	if cwd == "" {
		return ""
	}
	return CanonicalProjectPath(cwd)
}

// subagentParentTranscript maps a subagent transcript to the path of the
// session that dispatched it, or "" when the path is not shaped like a
// subagent transcript. The parent's transcript is the sibling of the directory
// holding subagents/, with .jsonl appended.
func subagentParentTranscript(subagentPath string) string {
	dir := filepath.Dir(subagentPath)
	if filepath.Base(dir) != subagentDirName {
		return ""
	}
	parentDir := filepath.Dir(dir)
	// filepath.Dir collapses to "." or "/" rather than erroring, and neither
	// names a parent session.
	if base := filepath.Base(parentDir); base == "." || base == string(filepath.Separator) {
		return ""
	}
	return parentDir + ".jsonl"
}

// firstCWD returns the first non-empty cwd field in a transcript, reading at
// most maxCWDLookaheadLines lines. A missing or unreadable file, a malformed
// one, and one that simply never records a cwd all read back as "" — the
// caller has a fallback for every one of them and the distinction would not
// change what it does.
//
// Only the cwd field is decoded. Unmarshalling a whole turn here would parse
// megabytes of message content per line to read one string.
func firstCWD(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	reader := bufio.NewReader(f)
	for i := 0; i < maxCWDLookaheadLines; i++ {
		line, oversized, readErr := readLineBounded(reader, maxLineBytes)
		if !oversized && len(line) > 0 {
			var head struct {
				CWD string `json:"cwd"`
			}
			if err := json.Unmarshal(line, &head); err == nil && strings.TrimSpace(head.CWD) != "" {
				return head.CWD
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return ""
			}
			return ""
		}
	}
	return ""
}
