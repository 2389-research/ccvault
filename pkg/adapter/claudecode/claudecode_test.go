// ABOUTME: Tests for the Claude Code source adapter that wraps the existing parser and scanner.
// ABOUTME: Validates interface compliance, Name(), Discover(), and Parse() behavior.

package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/2389-research/ccvault/pkg/adapter"
)

func TestClaudeCodeAdapter_ImplementsInterface(t *testing.T) {
	var _ adapter.SourceAdapter = New()
}

func TestClaudeCodeAdapter_Name(t *testing.T) {
	a := New()
	if got := a.Name(); got != "claude-code" {
		t.Errorf("Name() = %q, want %q", got, "claude-code")
	}
}

func TestClaudeCodeAdapter_Discover(t *testing.T) {
	// Build a minimal Claude Code directory structure in a temp dir:
	// <root>/projects/<encoded-path>/<uuid>.jsonl
	root := t.TempDir()
	projectDir := filepath.Join(root, "projects", "-Users-test-myproject")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}

	sessionID := "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	sessionFile := filepath.Join(projectDir, sessionID+".jsonl")
	if err := os.WriteFile(sessionFile, []byte(`{"uuid":"a1b2c3d4-e5f6-7890-abcd-ef1234567890","sessionId":"sess1","type":"user","timestamp":"2025-01-01T00:00:00Z"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := New()
	files, err := a.Discover(root)
	if err != nil {
		t.Fatalf("Discover() error: %v", err)
	}

	if len(files) != 1 {
		t.Fatalf("Discover() returned %d files, want 1", len(files))
	}

	sf := files[0]
	if sf.Path != sessionFile {
		t.Errorf("Path = %q, want %q", sf.Path, sessionFile)
	}
	if sf.ProjectPath == "" {
		t.Error("ProjectPath is empty")
	}
	if sf.ModTime.IsZero() {
		t.Error("ModTime is zero")
	}
}

func TestClaudeCodeAdapter_Parse(t *testing.T) {
	// Write a minimal session file with user and assistant turns
	dir := t.TempDir()
	path := filepath.Join(dir, "test-session.jsonl")

	lines := []string{
		`{"uuid":"turn-1","sessionId":"sess-1","type":"user","timestamp":"2025-01-01T00:00:00Z","message":{"role":"user","content":"hello"},"cwd":"/tmp/project"}`,
		`{"uuid":"turn-2","sessionId":"sess-1","parentUuid":"turn-1","type":"assistant","timestamp":"2025-01-01T00:01:00Z","message":{"id":"msg-1","model":"claude-3","role":"assistant","content":[{"type":"text","text":"hi there"}],"usage":{"input_tokens":10,"output_tokens":5}}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := New()
	parsed, err := a.Parse(path)
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	if parsed.ID != "sess-1" {
		t.Errorf("ID = %q, want %q", parsed.ID, "sess-1")
	}
	if parsed.Model != "claude-3" {
		t.Errorf("Model = %q, want %q", parsed.Model, "claude-3")
	}
	if parsed.ProjectPath != "/tmp/project" {
		t.Errorf("ProjectPath = %q, want %q", parsed.ProjectPath, "/tmp/project")
	}
	if len(parsed.Turns) != 2 {
		t.Fatalf("len(Turns) = %d, want 2", len(parsed.Turns))
	}

	// Check first turn
	if parsed.Turns[0].Type != "user" {
		t.Errorf("Turns[0].Type = %q, want %q", parsed.Turns[0].Type, "user")
	}

	// Check second turn
	if parsed.Turns[1].Type != "assistant" {
		t.Errorf("Turns[1].Type = %q, want %q", parsed.Turns[1].Type, "assistant")
	}
	if parsed.Turns[1].InputTokens != 10 {
		t.Errorf("Turns[1].InputTokens = %d, want 10", parsed.Turns[1].InputTokens)
	}
}

func TestClaudeCodeAdapter_Parse_ErrorDetection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "error-session.jsonl")

	// Real Claude Code tool errors land in a user message containing a tool_result
	// block with is_error: true.
	lines := []string{
		`{"uuid":"turn-1","sessionId":"sess-err","type":"user","timestamp":"2025-01-01T00:00:00Z","message":{"role":"user","content":"hello"}}`,
		`{"uuid":"turn-2","sessionId":"sess-err","parentUuid":"turn-1","type":"user","timestamp":"2025-01-01T00:01:00Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu-1","content":"bash: command not found","is_error":true}]}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := New()
	parsed, err := a.Parse(path)
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	// The error turn should be flagged
	if !parsed.Turns[1].HasError {
		t.Error("expected Turns[1].HasError to be true")
	}

	// Session metadata should reflect the error
	if v, ok := parsed.Metadata["has_error"]; !ok || v != true {
		t.Errorf("Metadata[has_error] = %v, want true", v)
	}
}

func TestClaudeCodeAdapter_Parse_ErrorDetection_FalsePositive(t *testing.T) {
	// User text mentioning "is_error":true should NOT be flagged: the previous
	// substring-match implementation produced false positives here.
	dir := t.TempDir()
	path := filepath.Join(dir, "talky-session.jsonl")

	lines := []string{
		`{"uuid":"turn-1","sessionId":"sess-talk","type":"user","timestamp":"2025-01-01T00:00:00Z","message":{"role":"user","content":"can you set \"is_error\":true on that response?"}}`,
		`{"uuid":"turn-2","sessionId":"sess-talk","parentUuid":"turn-1","type":"assistant","timestamp":"2025-01-01T00:01:00Z","message":{"id":"msg-1","model":"claude-3","role":"assistant","content":[{"type":"text","text":"sure"}],"usage":{"input_tokens":10,"output_tokens":5}}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := New()
	parsed, err := a.Parse(path)
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	for i, tr := range parsed.Turns {
		if tr.HasError {
			t.Errorf("Turns[%d].HasError = true, want false (false positive from substring match)", i)
		}
	}
	if v, ok := parsed.Metadata["has_error"]; ok && v == true {
		t.Error("Metadata[has_error] should not be set when no real tool error exists")
	}
}

func TestClaudeCodeAdapter_Parse_SubagentDetection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subagent-session.jsonl")

	taskInput, _ := json.Marshal(map[string]string{"description": "do something"})
	lines := []string{
		`{"uuid":"turn-1","sessionId":"sess-sub","type":"user","timestamp":"2025-01-01T00:00:00Z","message":{"role":"user","content":"hello"}}`,
		`{"uuid":"turn-2","sessionId":"sess-sub","type":"assistant","timestamp":"2025-01-01T00:01:00Z","message":{"id":"msg-1","model":"claude-3","role":"assistant","content":[{"type":"tool_use","id":"tu-1","name":"Task","input":` + string(taskInput) + `}],"usage":{"input_tokens":10,"output_tokens":5}}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := New()
	parsed, err := a.Parse(path)
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	if v, ok := parsed.Metadata["has_subagent"]; !ok || v != true {
		t.Errorf("Metadata[has_subagent] = %v, want true", v)
	}

	// Verify tool use was captured
	if len(parsed.Turns[1].ToolUses) == 0 {
		t.Fatal("expected tool uses on turn 2")
	}
	if parsed.Turns[1].ToolUses[0].ToolName != "Task" {
		t.Errorf("ToolUses[0].ToolName = %q, want %q", parsed.Turns[1].ToolUses[0].ToolName, "Task")
	}
}

func TestClaudeCodeAdapter_Registration(t *testing.T) {
	// The init() function should have registered the adapter
	a, err := adapter.Get("claude-code")
	if err != nil {
		t.Fatalf("adapter.Get(\"claude-code\") error: %v", err)
	}
	if a.Name() != "claude-code" {
		t.Errorf("registered adapter Name() = %q, want %q", a.Name(), "claude-code")
	}
}
