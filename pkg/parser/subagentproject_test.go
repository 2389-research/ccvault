// ABOUTME: Tests SubagentProjectPath — the rule that a subagent inherits its parent session's project.
// ABOUTME: Covers the preamble lines before the first cwd, the lookahead bound, and every "nothing to inherit" case.

package parser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testParentUUID = "04fb5717-c508-4503-ac85-dc11787cafaa"

// writeParentAndSubagent lays down <root>/projects/<encoded>/<uuid>.jsonl and
// its <uuid>/subagents/agent-x.jsonl sibling, and returns the subagent path.
// parentLines is written verbatim so a test can choose the transcript's shape.
func writeParentAndSubagent(t *testing.T, parentLines string) string {
	t.Helper()
	root := t.TempDir()
	projectDir := filepath.Join(root, "projects", "-Users-test-skills-dev-design")
	subDir := filepath.Join(projectDir, testParentUUID, "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if parentLines != "" {
		if err := os.WriteFile(filepath.Join(projectDir, testParentUUID+".jsonl"), []byte(parentLines), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	subagent := filepath.Join(subDir, "agent-a01b71e80ea28b3ad.jsonl")
	if err := os.WriteFile(subagent, []byte(`{"uuid":"s1","type":"user","timestamp":"2026-09-28T18:08:48Z","cwd":"/wherever"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return subagent
}

func TestSubagentProjectPathReadsParentCWD(t *testing.T) {
	// The first two lines carry no cwd, which is how Claude Code opens a
	// transcript: a "mode" record and a file-history snapshot.
	parent := `{"type":"mode","mode":"default"}` + "\n" +
		`{"type":"file-history-snapshot","messageId":"m0","snapshot":{}}` + "\n" +
		`{"uuid":"p1","type":"user","timestamp":"2026-09-28T18:00:00Z","cwd":"/Users/test/skills-dev/design"}` + "\n"

	got := SubagentProjectPath(writeParentAndSubagent(t, parent))
	if want := "/Users/test/skills-dev/design"; got != want {
		t.Errorf("SubagentProjectPath = %q, want %q", got, want)
	}
}

func TestSubagentProjectPathCanonicalisesParentWorktree(t *testing.T) {
	parent := `{"uuid":"p1","type":"user","timestamp":"2026-09-28T18:00:00Z","cwd":"/Users/test/repo/.claude/worktrees/agent-aparent"}` + "\n"

	got := SubagentProjectPath(writeParentAndSubagent(t, parent))
	if want := "/Users/test/repo"; got != want {
		t.Errorf("SubagentProjectPath = %q, want %q", got, want)
	}
}

func TestSubagentProjectPathEmptyWhenNothingToInherit(t *testing.T) {
	cwdLine := `{"uuid":"p1","type":"user","timestamp":"2026-09-28T18:00:00Z","cwd":"/Users/test/skills-dev/design"}` + "\n"

	cases := []struct {
		name   string
		parent string
	}{
		{"no parent transcript", ""},
		{"parent is empty", ""},
		{"parent records no cwd", `{"type":"mode","mode":"default"}` + "\n"},
		{"parent is not json", "not json at all\n"},
		{"parent cwd is blank", `{"uuid":"p1","type":"user","cwd":"   "}` + "\n"},
		// One line past the lookahead cap: the cwd is there, and the reader
		// is required not to go looking that far.
		{"cwd beyond the lookahead cap", strings.Repeat(`{"type":"mode"}`+"\n", maxCWDLookaheadLines) + cwdLine},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SubagentProjectPath(writeParentAndSubagent(t, tc.parent)); got != "" {
				t.Errorf("SubagentProjectPath = %q, want \"\"", got)
			}
		})
	}
}

// The cwd on the last line the reader is allowed to look at still counts. Paired
// with the case above, this pins the boundary rather than just the rejection.
func TestSubagentProjectPathReadsTheLastAllowedLine(t *testing.T) {
	parent := strings.Repeat(`{"type":"mode"}`+"\n", maxCWDLookaheadLines-1) +
		`{"uuid":"p1","type":"user","cwd":"/Users/test/skills-dev/design"}` + "\n"

	if got := SubagentProjectPath(writeParentAndSubagent(t, parent)); got != "/Users/test/skills-dev/design" {
		t.Errorf("SubagentProjectPath = %q, want the cwd on line %d", got, maxCWDLookaheadLines)
	}
}

func TestSubagentProjectPathRejectsNonSubagentPaths(t *testing.T) {
	for _, p := range []string{
		"/home/u/.claude/projects/-Users-test-x/04fb5717-c508-4503-ac85-dc11787cafaa.jsonl",
		"/home/u/.claude/projects/-Users-test-x/notsubagents/agent-a1.jsonl",
		"agent-a1.jsonl",
		"",
	} {
		if got := SubagentProjectPath(p); got != "" {
			t.Errorf("SubagentProjectPath(%q) = %q, want \"\"", p, got)
		}
	}
}
