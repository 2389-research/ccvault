// ABOUTME: Tests for the Nanoclaw source adapter covering parent + subagent JSONLs.
// ABOUTME: Builds nanoclaw-shaped fixture trees in temp dirs so we exercise Discover + Parse together.

package nanoclaw

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/2389-research/ccvault/pkg/adapter"
)

func TestNanoclawAdapter_Name(t *testing.T) {
	a := New()
	if got := a.Name(); got != "nanoclaw" {
		t.Errorf("Name() = %q, want %q", got, "nanoclaw")
	}
}

func TestNanoclawAdapter_ImplementsInterface(t *testing.T) {
	var _ adapter.SourceAdapter = New()
}

func TestNanoclawAdapter_Registration(t *testing.T) {
	a, err := adapter.Get("nanoclaw")
	if err != nil {
		t.Fatalf("adapter.Get(%q) error: %v", "nanoclaw", err)
	}
	if a.Name() != "nanoclaw" {
		t.Errorf("registered adapter Name() = %q, want %q", a.Name(), "nanoclaw")
	}
}

func TestNanoclawAdapter_Discover_MissingRoot(t *testing.T) {
	a := New()
	files, err := a.Discover(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("Discover() error: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("len(files) = %d, want 0", len(files))
	}
}

func TestNanoclawAdapter_Discover_EmptyGroups(t *testing.T) {
	// Groups without .claude/ (freshly created but never used) should be
	// skipped silently — matches how ~/work/tools/nanoclaw/data/sessions/
	// looks for a fresh install.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "reed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "mo"), 0o755); err != nil {
		t.Fatal(err)
	}

	a := New()
	files, err := a.Discover(root)
	if err != nil {
		t.Fatalf("Discover() error: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("len(files) = %d, want 0", len(files))
	}
}

func TestNanoclawAdapter_Discover_ParentAndSubagent(t *testing.T) {
	root, parentPath, subPath := writeNanoclawTree(t, "reed", "11111111-2222-3333-4444-555555555555", "agent-abc123")

	a := New()
	files, err := a.Discover(root)
	if err != nil {
		t.Fatalf("Discover() error: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("len(files) = %d, want 2", len(files))
	}

	paths := map[string]bool{}
	for _, f := range files {
		if f.ProjectPath != "nanoclaw/reed" {
			t.Errorf("ProjectPath = %q, want %q", f.ProjectPath, "nanoclaw/reed")
		}
		if f.ModTime.IsZero() {
			t.Errorf("ModTime is zero for %s", f.Path)
		}
		paths[f.Path] = true
	}
	if !paths[parentPath] {
		t.Errorf("parent session not discovered: %s", parentPath)
	}
	if !paths[subPath] {
		t.Errorf("subagent session not discovered: %s", subPath)
	}
}

func TestNanoclawAdapter_Discover_IgnoresMetaJSON(t *testing.T) {
	// A sibling agent-*.meta.json must not be treated as a session — the
	// discovery filter looks for .jsonl only, but this test pins the
	// behavior in place against future refactors.
	root, _, _ := writeNanoclawTree(t, "reed", "11111111-2222-3333-4444-555555555555", "agent-abc123")

	a := New()
	files, err := a.Discover(root)
	if err != nil {
		t.Fatalf("Discover() error: %v", err)
	}
	for _, f := range files {
		if strings.HasSuffix(f.Path, subagentMetaSuffix) {
			t.Errorf("meta.json leaked into discovery: %s", f.Path)
		}
	}
}

func TestNanoclawAdapter_Parse_ParentSession(t *testing.T) {
	_, parentPath, _ := writeNanoclawTree(t, "reed", "11111111-2222-3333-4444-555555555555", "agent-abc123")

	a := New()
	parsed, err := a.Parse(parentPath)
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	if parsed.ID != "nanoclaw:11111111-2222-3333-4444-555555555555" {
		t.Errorf("ID = %q, want %q", parsed.ID, "nanoclaw:11111111-2222-3333-4444-555555555555")
	}
	if parsed.SourceName != "nanoclaw" {
		t.Errorf("SourceName = %q, want %q", parsed.SourceName, "nanoclaw")
	}
	if parsed.ProjectPath != "nanoclaw/reed" {
		t.Errorf("ProjectPath = %q, want %q", parsed.ProjectPath, "nanoclaw/reed")
	}
	if parsed.DisplayName != "nanoclaw/reed" {
		t.Errorf("DisplayName = %q, want %q", parsed.DisplayName, "nanoclaw/reed")
	}
	if parsed.Metadata["nanoclaw_group"] != "reed" {
		t.Errorf("nanoclaw_group = %v, want %q", parsed.Metadata["nanoclaw_group"], "reed")
	}
	// Turn count from the fixture: user + assistant + scheduled task + tool-result-error = 4
	if len(parsed.Turns) != 4 {
		t.Fatalf("len(Turns) = %d, want 4", len(parsed.Turns))
	}
}

func TestNanoclawAdapter_Parse_ScheduledTaskReclassified(t *testing.T) {
	_, parentPath, _ := writeNanoclawTree(t, "reed", "11111111-2222-3333-4444-555555555555", "agent-abc123")

	a := New()
	parsed, err := a.Parse(parentPath)
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	// The scheduled-task turn is the third one in the fixture.
	scheduled := parsed.Turns[2]
	if !strings.HasPrefix(scheduled.Content, scheduledTaskPrefix) {
		t.Fatalf("expected fixture turn 2 to be the scheduled task, got content=%q", scheduled.Content)
	}
	if scheduled.Type != "system" {
		t.Errorf("scheduled task Type = %q, want %q", scheduled.Type, "system")
	}
	if v, ok := parsed.Metadata["has_scheduled_tasks"]; !ok || v != true {
		t.Errorf("has_scheduled_tasks = %v, want true", v)
	}
}

func TestNanoclawAdapter_Parse_ErrorDetection(t *testing.T) {
	_, parentPath, _ := writeNanoclawTree(t, "reed", "11111111-2222-3333-4444-555555555555", "agent-abc123")

	a := New()
	parsed, err := a.Parse(parentPath)
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	if v, ok := parsed.Metadata["has_error"]; !ok || v != true {
		t.Errorf("has_error = %v, want true", v)
	}
	// The error turn is the fourth one — its HasError flag should be set.
	if !parsed.Turns[3].HasError {
		t.Error("expected Turns[3].HasError to be true")
	}
}

func TestNanoclawAdapter_Parse_SubagentSession(t *testing.T) {
	_, _, subPath := writeNanoclawTree(t, "reed", "11111111-2222-3333-4444-555555555555", "agent-abc123")

	a := New()
	parsed, err := a.Parse(subPath)
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	// ID must include BOTH parent UUID and agent ID — the parser returns the
	// parent's sessionId inside a subagent file, so a plain "nanoclaw:<sid>"
	// would collide with the parent session on the sessions PK.
	wantID := "nanoclaw:11111111-2222-3333-4444-555555555555:agent-abc123"
	if parsed.ID != wantID {
		t.Errorf("ID = %q, want %q", parsed.ID, wantID)
	}

	if parsed.Metadata["is_sidechain"] != true {
		t.Errorf("is_sidechain = %v, want true", parsed.Metadata["is_sidechain"])
	}
	if parsed.Metadata["agent_id"] != "agent-abc123" {
		t.Errorf("agent_id = %v, want %q", parsed.Metadata["agent_id"], "agent-abc123")
	}
	if parsed.Metadata["agent_type"] != "general-purpose" {
		t.Errorf("agent_type = %v, want %q", parsed.Metadata["agent_type"], "general-purpose")
	}
	if parsed.Metadata["parent_session_id"] != "nanoclaw:11111111-2222-3333-4444-555555555555" {
		t.Errorf("parent_session_id = %v, want %q", parsed.Metadata["parent_session_id"], "nanoclaw:11111111-2222-3333-4444-555555555555")
	}
	if parsed.Metadata["nanoclaw_group"] != "reed" {
		t.Errorf("nanoclaw_group = %v, want %q", parsed.Metadata["nanoclaw_group"], "reed")
	}

	// Subagents inherit the same project path as the parent so they roll up
	// together in the UI.
	if parsed.ProjectPath != "nanoclaw/reed" {
		t.Errorf("ProjectPath = %q, want %q", parsed.ProjectPath, "nanoclaw/reed")
	}

	// The subagent fixture opens with a plain [SCHEDULED TASK - ...] string as
	// a user turn — but scheduled-task reclassification is a parent-only
	// convention (nanoclaw injects them into top-level sessions, not into
	// subagents), so it must stay a user turn here.
	if parsed.Turns[0].Type != "user" {
		t.Errorf("subagent turn 0 Type = %q, want %q (scheduled reclass should not apply to subagents)", parsed.Turns[0].Type, "user")
	}
	if _, ok := parsed.Metadata["has_scheduled_tasks"]; ok {
		t.Error("has_scheduled_tasks should not be set on subagent sessions")
	}
}

func TestNanoclawAdapter_Parse_SubagentWithoutMeta(t *testing.T) {
	// A subagent JSONL without a sibling meta.json must still parse cleanly —
	// meta.json is optional context, not a hard requirement.
	root := t.TempDir()
	group := "mo"
	parentUUID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	agentID := "agent-nometa"

	projectDir := filepath.Join(root, group, ".claude", "projects", "-workspace-group")
	subDir := filepath.Join(projectDir, parentUUID, "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Parent must exist so ScanClaudeHome doesn't blow up on missing dirs.
	parentPath := filepath.Join(projectDir, parentUUID+".jsonl")
	writeJSONLLines(t, parentPath, minimalParentLines(parentUUID))
	subPath := filepath.Join(subDir, agentID+".jsonl")
	writeJSONLLines(t, subPath, minimalSubagentLines(parentUUID, agentID))

	a := New()
	parsed, err := a.Parse(subPath)
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	if parsed.ID != "nanoclaw:"+parentUUID+":"+agentID {
		t.Errorf("ID = %q, want %q", parsed.ID, "nanoclaw:"+parentUUID+":"+agentID)
	}
	if _, ok := parsed.Metadata["agent_type"]; ok {
		t.Error("agent_type should be absent when meta.json is missing")
	}
	if parsed.Metadata["is_sidechain"] != true {
		t.Errorf("is_sidechain = %v, want true", parsed.Metadata["is_sidechain"])
	}
}

func TestNanoclawAdapter_isSubagentPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/foo/.claude/projects/-x/uuid.jsonl", false},
		{"/foo/.claude/projects/-x/uuid/subagents/agent-abc.jsonl", true},
		{"/foo/.claude/projects/-x/subagents/agent-abc.jsonl", true},
		// A path whose *filename* contains the word subagents but isn't inside
		// one should still be treated as a parent — the dispatcher only cares
		// about ancestor directories.
		{"/foo/.claude/projects/-x/subagents.jsonl", false},
	}
	for _, c := range cases {
		if got := isSubagentPath(c.path); got != c.want {
			t.Errorf("isSubagentPath(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

// --- fixture helpers ---

// writeNanoclawTree lays down a minimal but realistic nanoclaw sessions tree
// and returns (root, parent-session-path, subagent-session-path). The parent
// session contains: user turn, assistant turn, scheduled-task user turn, and
// a tool_result error turn. The subagent has: scheduled-task-looking user
// turn (to prove reclassification does NOT apply to subagents), and an
// assistant turn.
func writeNanoclawTree(t *testing.T, group, parentUUID, agentID string) (root, parentPath, subPath string) {
	t.Helper()
	root = t.TempDir()
	projectDir := filepath.Join(root, group, ".claude", "projects", "-workspace-group")
	subDir := filepath.Join(projectDir, parentUUID, "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}

	parentPath = filepath.Join(projectDir, parentUUID+".jsonl")
	writeJSONLLines(t, parentPath, parentSessionLines(parentUUID))

	subPath = filepath.Join(subDir, agentID+".jsonl")
	writeJSONLLines(t, subPath, minimalSubagentLines(parentUUID, agentID))

	// Sibling meta.json — optional but the primary fixture exercises the
	// happy path where agentType is populated.
	metaPath := filepath.Join(subDir, agentID+subagentMetaSuffix)
	if err := os.WriteFile(metaPath, []byte(`{"agentType":"general-purpose"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	return root, parentPath, subPath
}

func parentSessionLines(sessionID string) []string {
	return []string{
		// 1. plain user turn
		`{"uuid":"t1","sessionId":"` + sessionID + `","type":"user","timestamp":"2026-05-26T14:30:00Z","message":{"role":"user","content":"hello"},"cwd":"/workspace/group"}`,
		// 2. assistant reply
		`{"uuid":"t2","sessionId":"` + sessionID + `","parentUuid":"t1","type":"assistant","timestamp":"2026-05-26T14:30:05Z","message":{"id":"msg-1","model":"claude-sonnet-4-6","role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":5,"output_tokens":2}}}`,
		// 3. scheduled-task injection — should be reclassified to system
		`{"uuid":"t3","sessionId":"` + sessionID + `","parentUuid":"t2","type":"user","timestamp":"2026-05-26T14:35:00Z","message":{"role":"user","content":"[SCHEDULED TASK - daily-check] run health probe"},"cwd":"/workspace/group"}`,
		// 4. tool_result error turn — pins has_error metadata + Turns[3].HasError
		`{"uuid":"t4","sessionId":"` + sessionID + `","parentUuid":"t3","type":"user","timestamp":"2026-05-26T14:35:10Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu-1","content":"boom","is_error":true}]}}`,
	}
}

func minimalParentLines(sessionID string) []string {
	return []string{
		`{"uuid":"p1","sessionId":"` + sessionID + `","type":"user","timestamp":"2026-05-26T14:30:00Z","message":{"role":"user","content":"hi"},"cwd":"/workspace/group"}`,
	}
}

func minimalSubagentLines(parentUUID, _ string) []string {
	// The sessionId inside a subagent file points at the PARENT session — that
	// is the invariant the adapter has to work around for ID uniqueness.
	return []string{
		`{"uuid":"s1","sessionId":"` + parentUUID + `","isSidechain":true,"type":"user","timestamp":"2026-05-26T14:36:22Z","message":{"role":"user","content":"[SCHEDULED TASK - shouldnt-reclassify] research foo"},"cwd":"/workspace/group"}`,
		`{"uuid":"s2","sessionId":"` + parentUUID + `","parentUuid":"s1","isSidechain":true,"type":"assistant","timestamp":"2026-05-26T14:36:25Z","message":{"id":"msg-s1","model":"claude-sonnet-4-6","role":"assistant","content":[{"type":"text","text":"done"}],"usage":{"input_tokens":3,"output_tokens":1}}}`,
	}
}

func writeJSONLLines(t *testing.T, path string, lines []string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}
