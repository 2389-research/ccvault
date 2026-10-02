// ABOUTME: Tests that the Claude Code adapter archives subagent transcripts as their own sessions.
// ABOUTME: Guards the id collision that made the scanner skip subagents/ in the first place.

package claudecode

import (
	"os"
	"path/filepath"
	"testing"
)

const probeParentUUID = "04fb5717-c508-4503-ac85-dc11787cafaa"

// writeSubagentTree lays down the real Claude Code shape: a parent transcript
// beside a <parent-uuid>/subagents/ directory holding agent-*.jsonl files and
// their .meta.json siblings. Every subagent line carries the PARENT's
// sessionId, exactly as Claude Code writes it.
func writeSubagentTree(t *testing.T) (root, parentFile, subagentFile string) {
	t.Helper()
	root = t.TempDir()
	projectDir := filepath.Join(root, "projects", "-Users-test-myproject")
	subDir := filepath.Join(projectDir, probeParentUUID, "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}

	parentFile = filepath.Join(projectDir, probeParentUUID+".jsonl")
	parentLines := `{"uuid":"p1","sessionId":"` + probeParentUUID + `","type":"user","timestamp":"2026-09-28T18:00:00Z","cwd":"/Users/test/myproject","gitBranch":"main","promptId":"a8ac7331-4de1-4d54-9e99-9599e4ca36fc","message":{"role":"user","content":"go"}}` + "\n" +
		`{"uuid":"p2","parentUuid":"p1","sessionId":"` + probeParentUUID + `","type":"assistant","timestamp":"2026-09-28T18:00:05Z","cwd":"/Users/test/myproject","message":{"id":"m1","model":"claude-opus-4","role":"assistant","content":[{"type":"tool_use","id":"toolu_018APMWCXLmJBpvk6iytRVM1","name":"Task","input":{"subagent_type":"general-purpose"}}],"usage":{"input_tokens":10,"output_tokens":20}}}` + "\n"
	if err := os.WriteFile(parentFile, []byte(parentLines), 0o644); err != nil {
		t.Fatal(err)
	}

	subagentFile = filepath.Join(subDir, "agent-a01b71e80ea28b3ad.jsonl")
	subLines := `{"uuid":"s1","parentUuid":null,"isSidechain":true,"agentId":"a01b71e80ea28b3ad","promptId":"a8ac7331-4de1-4d54-9e99-9599e4ca36fc","sessionId":"` + probeParentUUID + `","type":"user","timestamp":"2026-09-28T18:08:48.187Z","cwd":"/Users/test/myproject","gitBranch":"main","message":{"role":"user","content":"do the thing"}}` + "\n" +
		`{"uuid":"s2","parentUuid":"s1","isSidechain":true,"agentId":"a01b71e80ea28b3ad","sessionId":"` + probeParentUUID + `","type":"assistant","timestamp":"2026-09-28T18:09:00Z","cwd":"/Users/test/myproject","message":{"id":"m2","model":"claude-opus-4","role":"assistant","content":[{"type":"text","text":"did the thing"}],"usage":{"input_tokens":100,"output_tokens":200}}}` + "\n"
	if err := os.WriteFile(subagentFile, []byte(subLines), 0o644); err != nil {
		t.Fatal(err)
	}
	meta := `{"agentType":"general-purpose","description":"do the thing","toolUseId":"toolu_018APMWCXLmJBpvk6iytRVM1","spawnDepth":1}`
	if err := os.WriteFile(filepath.Join(subDir, "agent-a01b71e80ea28b3ad.meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}

	return root, parentFile, subagentFile
}

// TestDiscoverIncludesSubagents is the non-vacuous half of the fix: before it,
// Discover returned the parent only, because the scanner skipped subagents/.
func TestDiscoverIncludesSubagents(t *testing.T) {
	root, parentFile, subagentFile := writeSubagentTree(t)

	files, err := New().Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	found := map[string]bool{}
	for _, f := range files {
		found[f.Path] = true
	}
	if !found[parentFile] {
		t.Error("parent transcript not discovered")
	}
	if !found[subagentFile] {
		t.Error("subagent transcript not discovered — the subagents/ skip is still in force")
	}
	if len(files) != 2 {
		t.Errorf("discovered %d files, want 2: %+v", len(files), files)
	}
}

func TestParseSubagentMintsCompositeID(t *testing.T) {
	_, _, subagentFile := writeSubagentTree(t)

	parsed, err := New().Parse(subagentFile)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	wantID := "claude-code:" + probeParentUUID + ":agent-a01b71e80ea28b3ad"
	if parsed.ID != wantID {
		t.Errorf("ID = %q, want %q", parsed.ID, wantID)
	}
	// The collision this exists to prevent: the file's in-band sessionId is
	// the parent's uuid, and sessions.id is a primary key.
	if parsed.ID == probeParentUUID {
		t.Fatal("subagent session took its parent's id — it would overwrite the parent row")
	}
	if got := parsed.Metadata["parent_session_id"]; got != probeParentUUID {
		t.Errorf("parent_session_id = %v, want the bare parent uuid %q", got, probeParentUUID)
	}
	if got := parsed.Metadata["is_sidechain"]; got != true {
		t.Errorf("is_sidechain = %v, want true", got)
	}
	if got := parsed.Metadata["agent_id"]; got != "agent-a01b71e80ea28b3ad" {
		t.Errorf("agent_id = %v", got)
	}
	if got := parsed.Metadata["agent_type"]; got != "general-purpose" {
		t.Errorf("agent_type = %v, want general-purpose (from the meta.json sibling)", got)
	}
	if parsed.ProjectPath != "/Users/test/myproject" {
		t.Errorf("ProjectPath = %q — a subagent belongs to its parent's project", parsed.ProjectPath)
	}
	if len(parsed.Turns) != 2 {
		t.Errorf("Turns = %d, want 2", len(parsed.Turns))
	}
	if parsed.SourceName != "claude-code" {
		t.Errorf("SourceName = %q", parsed.SourceName)
	}
}

func TestParseParentUnaffected(t *testing.T) {
	_, parentFile, _ := writeSubagentTree(t)

	parsed, err := New().Parse(parentFile)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if parsed.ID != probeParentUUID {
		t.Errorf("ID = %q, want the bare uuid", parsed.ID)
	}
	if _, ok := parsed.Metadata["parent_session_id"]; ok {
		t.Error("a top-level session must not carry parent_session_id")
	}
	if got := parsed.Metadata["has_subagent"]; got != true {
		t.Errorf("has_subagent = %v, want true (the parent dispatched a Task)", got)
	}
}
