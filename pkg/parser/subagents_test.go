// ABOUTME: Tests for discovering subagent transcripts under a .claude/projects tree.
// ABOUTME: Covers the agent-*.jsonl naming, the .meta.json sibling, and ScanClaudeHome's skip.

package parser

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// subagentTree builds the real on-disk shape Claude Code writes:
//
//	projects/<encoded>/<session-uuid>/subagents/agent-<hex>.jsonl
//	projects/<encoded>/<session-uuid>/subagents/agent-<hex>.meta.json
//	projects/<encoded>/<session-uuid>.jsonl
func subagentTree(t *testing.T) (claudeHome, projectsDir, parentUUID string) {
	t.Helper()
	claudeHome = t.TempDir()
	projectsDir = filepath.Join(claudeHome, "projects")
	parentUUID = "04fb5717-c508-4503-ac85-dc11787cafaa"

	projectDir := filepath.Join(projectsDir, "-Users-test-myproject")
	subDir := filepath.Join(projectDir, parentUUID, "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// The parent transcript. Note the subagent files carry this same
	// sessionId — that is the whole reason they need a minted id.
	parentLine := `{"uuid":"p1","sessionId":"` + parentUUID + `","type":"user","timestamp":"2026-09-28T18:00:00Z","cwd":"/Users/test/myproject"}` + "\n"
	if err := os.WriteFile(filepath.Join(projectDir, parentUUID+".jsonl"), []byte(parentLine), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, agent := range []string{"agent-a01b71e80ea28b3ad", "agent-ab685cc44d883a956"} {
		line := `{"uuid":"s1","parentUuid":null,"isSidechain":true,"agentId":"` + agent[len("agent-"):] +
			`","promptId":"a8ac7331-4de1-4d54-9e99-9599e4ca36fc","sessionId":"` + parentUUID +
			`","type":"user","timestamp":"2026-09-28T18:08:48.187Z","cwd":"/Users/test/myproject"}` + "\n"
		if err := os.WriteFile(filepath.Join(subDir, agent+".jsonl"), []byte(line), 0o644); err != nil {
			t.Fatal(err)
		}
		meta := `{"agentType":"general-purpose","description":"probe","toolUseId":"toolu_018APMWCXLmJBpvk6iytRVM1","spawnDepth":1}`
		if err := os.WriteFile(filepath.Join(subDir, agent+".meta.json"), []byte(meta), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	return claudeHome, projectsDir, parentUUID
}

func TestScanSubagentFiles(t *testing.T) {
	_, projectsDir, parentUUID := subagentTree(t)

	files, err := ScanSubagentFiles(projectsDir)
	if err != nil {
		t.Fatalf("ScanSubagentFiles: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("got %d subagent files, want 2: %+v", len(files), files)
	}

	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	first := files[0]
	if filepath.Base(first.Path) != "agent-a01b71e80ea28b3ad.jsonl" {
		t.Errorf("Path = %q, want the agent jsonl", first.Path)
	}
	if first.SessionID != "agent-a01b71e80ea28b3ad" {
		t.Errorf("SessionID = %q, want the agent stem", first.SessionID)
	}
	// ProjectPath is the lossy path decode of the *project* directory, not of
	// the parent-uuid or subagents segment underneath it.
	if first.ProjectPath != "/Users/test/myproject" {
		t.Errorf("ProjectPath = %q, want /Users/test/myproject", first.ProjectPath)
	}
	if first.ModTime.IsZero() {
		t.Error("ModTime is zero")
	}
	_ = parentUUID
}

// TestScanSubagentFilesIgnoresNonTranscripts keeps the .meta.json sibling and
// anything outside a subagents/ directory out of the result.
func TestScanSubagentFilesIgnoresNonTranscripts(t *testing.T) {
	_, projectsDir, parentUUID := subagentTree(t)

	stray := filepath.Join(projectsDir, "-Users-test-myproject", parentUUID, "subagents", "notes.jsonl")
	if err := os.WriteFile(stray, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, err := ScanSubagentFiles(projectsDir)
	if err != nil {
		t.Fatalf("ScanSubagentFiles: %v", err)
	}
	for _, f := range files {
		base := filepath.Base(f.Path)
		if base == "notes.jsonl" {
			t.Error("a non agent-* jsonl was discovered as a subagent transcript")
		}
		if filepath.Ext(base) != ".jsonl" {
			t.Errorf("non-jsonl file discovered: %s", base)
		}
	}
	if len(files) != 2 {
		t.Errorf("got %d files, want 2", len(files))
	}
}

func TestScanSubagentFilesMissingTree(t *testing.T) {
	files, err := ScanSubagentFiles(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("a missing projects tree is not an error: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("got %d files, want 0", len(files))
	}
}

// TestScanClaudeHomeStillSkipsSubagents pins the split: ScanClaudeHome returns
// top-level transcripts only. The nanoclaw adapter depends on that — it calls
// ScanClaudeHome for parents and discovers sidechains separately.
func TestScanClaudeHomeStillSkipsSubagents(t *testing.T) {
	claudeHome, _, parentUUID := subagentTree(t)

	files, err := ScanClaudeHome(claudeHome)
	if err != nil {
		t.Fatalf("ScanClaudeHome: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("got %d files, want 1 (the parent only): %+v", len(files), files)
	}
	if files[0].SessionID != parentUUID {
		t.Errorf("SessionID = %q, want %q", files[0].SessionID, parentUUID)
	}
}
