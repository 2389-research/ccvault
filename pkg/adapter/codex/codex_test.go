// ABOUTME: Tests for the Codex source adapter covering discovery and parsing.
// ABOUTME: Uses inline test data in temp directories to validate JSONL parsing logic.

package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/2389-research/ccvault/pkg/adapter"
)

func TestName(t *testing.T) {
	a := New()
	if a.Name() != "codex" {
		t.Fatalf("expected name %q, got %q", "codex", a.Name())
	}
}

func TestImplementsSourceAdapter(t *testing.T) {
	var _ adapter.SourceAdapter = New()
}

func TestDiscover(t *testing.T) {
	// Create a temp directory mimicking ~/.codex/sessions/YYYY/MM/DD/
	root := t.TempDir()
	sessionsDir := filepath.Join(root, "sessions", "2026", "03", "11")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Create a valid session file
	fname := "rollout-2026-03-11T11-24-50-019cddb7-1979-7983-b176-785af4de7cbf.jsonl"
	sessionMeta := map[string]any{
		"timestamp": "2026-03-11T16:25:01.231Z",
		"type":      "session_meta",
		"payload": map[string]any{
			"id":  "019cddb7-1979-7983-b176-785af4de7cbf",
			"cwd": "/Users/someone/project",
		},
	}
	data, _ := json.Marshal(sessionMeta)
	if err := os.WriteFile(filepath.Join(sessionsDir, fname), append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create an unrelated file that should be ignored
	if err := os.WriteFile(filepath.Join(sessionsDir, "notes.txt"), []byte("ignore me"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := New()
	files, err := a.Discover(root)
	if err != nil {
		t.Fatalf("Discover error: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	if files[0].ProjectPath != "/Users/someone/project" {
		t.Errorf("expected project path %q, got %q", "/Users/someone/project", files[0].ProjectPath)
	}
}

func TestDiscoverMultipleFiles(t *testing.T) {
	root := t.TempDir()

	// Two different date directories
	dir1 := filepath.Join(root, "sessions", "2026", "03", "11")
	dir2 := filepath.Join(root, "sessions", "2026", "04", "01")
	if err := os.MkdirAll(dir1, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir2, 0o755); err != nil {
		t.Fatal(err)
	}

	writeMinimalSession(t, dir1, "rollout-2026-03-11T11-24-50-aaaa-bbbb.jsonl", "/project/a")
	writeMinimalSession(t, dir2, "rollout-2026-04-01T09-00-00-cccc-dddd.jsonl", "/project/b")

	a := New()
	files, err := a.Discover(root)
	if err != nil {
		t.Fatalf("Discover error: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}
}

func TestDiscoverMissingDir(t *testing.T) {
	root := t.TempDir()
	a := New()
	files, err := a.Discover(root)
	if err != nil {
		t.Fatalf("expected no error for missing sessions dir, got: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("expected 0 files, got %d", len(files))
	}
}

func TestParse(t *testing.T) {
	dir := t.TempDir()
	fpath := filepath.Join(dir, "rollout-2026-03-11T11-24-50-019cddb7-1979-7983-b176-785af4de7cbf.jsonl")

	lines := []map[string]any{
		{
			"timestamp": "2026-03-11T16:25:01.231Z",
			"type":      "session_meta",
			"payload": map[string]any{
				"id":  "019cddb7-1979-7983-b176-785af4de7cbf",
				"cwd": "/Users/dylanr/work/project",
				"git": map[string]any{
					"branch": "feature/cool-stuff",
				},
			},
		},
		// Developer message — should be skipped
		{
			"timestamp": "2026-03-11T16:25:01.232Z",
			"type":      "response_item",
			"payload": map[string]any{
				"type": "message",
				"role": "developer",
				"content": []map[string]any{
					{"type": "input_text", "text": "system instructions"},
				},
			},
		},
		// Turn context with model
		{
			"timestamp": "2026-03-11T16:25:01.232Z",
			"type":      "turn_context",
			"payload": map[string]any{
				"turn_id": "turn-001",
				"model":   "gpt-5.4",
			},
		},
		// User message
		{
			"timestamp": "2026-03-11T16:25:01.232Z",
			"type":      "response_item",
			"payload": map[string]any{
				"type": "message",
				"role": "user",
				"content": []map[string]any{
					{"type": "input_text", "text": "What is this project?"},
				},
			},
		},
		// Assistant message
		{
			"timestamp": "2026-03-11T16:25:07.982Z",
			"type":      "response_item",
			"payload": map[string]any{
				"type": "message",
				"role": "assistant",
				"content": []map[string]any{
					{"type": "output_text", "text": "This is a Go project."},
				},
			},
		},
		// Function call (tool use)
		{
			"timestamp": "2026-03-11T16:25:07.985Z",
			"type":      "response_item",
			"payload": map[string]any{
				"type":      "function_call",
				"name":      "exec_command",
				"arguments": `{"cmd":"ls","workdir":"/Users/dylanr/work"}`,
				"call_id":   "call_abc123",
			},
		},
		// Function call output
		{
			"timestamp": "2026-03-11T16:25:08.000Z",
			"type":      "response_item",
			"payload": map[string]any{
				"type": "function_call_output",
			},
		},
		// Token count event
		{
			"timestamp": "2026-03-11T16:25:08.133Z",
			"type":      "event_msg",
			"payload": map[string]any{
				"type": "token_count",
				"info": map[string]any{
					"total_token_usage": map[string]any{
						"input_tokens":  8870,
						"output_tokens": 332,
					},
				},
			},
		},
		// Reasoning block — should be skipped
		{
			"timestamp": "2026-03-11T16:25:09.000Z",
			"type":      "response_item",
			"payload": map[string]any{
				"type": "reasoning",
			},
		},
	}

	writeJSONLFile(t, fpath, lines)

	a := New()
	session, err := a.Parse(fpath)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	// Check session metadata
	if session.ID != "019cddb7-1979-7983-b176-785af4de7cbf" {
		t.Errorf("unexpected session ID: %s", session.ID)
	}
	if session.ProjectPath != "/Users/dylanr/work/project" {
		t.Errorf("unexpected project path: %s", session.ProjectPath)
	}
	if session.GitBranch != "feature/cool-stuff" {
		t.Errorf("unexpected git branch: %s", session.GitBranch)
	}
	if session.Model != "gpt-5.4" {
		t.Errorf("unexpected model: %s", session.Model)
	}
	if session.SourceName != "codex" {
		t.Errorf("unexpected source name: %s", session.SourceName)
	}

	// Should have 2 turns: user + assistant (developer skipped)
	if len(session.Turns) != 2 {
		t.Fatalf("expected 2 turns, got %d", len(session.Turns))
	}

	// Check user turn
	userTurn := session.Turns[0]
	if userTurn.Type != "user" {
		t.Errorf("expected user turn, got %s", userTurn.Type)
	}
	if userTurn.Content != "What is this project?" {
		t.Errorf("unexpected user content: %s", userTurn.Content)
	}

	// Check assistant turn
	assistantTurn := session.Turns[1]
	if assistantTurn.Type != "assistant" {
		t.Errorf("expected assistant turn, got %s", assistantTurn.Type)
	}
	if assistantTurn.Content != "This is a Go project." {
		t.Errorf("unexpected assistant content: %s", assistantTurn.Content)
	}

	// Tool uses should be on the assistant turn (function_call comes after it)
	if len(assistantTurn.ToolUses) != 1 {
		t.Fatalf("expected 1 tool use, got %d", len(assistantTurn.ToolUses))
	}
	if assistantTurn.ToolUses[0].ToolName != "exec_command" {
		t.Errorf("unexpected tool name: %s", assistantTurn.ToolUses[0].ToolName)
	}

	// Token counts should be on the assistant turn
	if assistantTurn.InputTokens != 8870 {
		t.Errorf("expected input tokens 8870, got %d", assistantTurn.InputTokens)
	}
	if assistantTurn.OutputTokens != 332 {
		t.Errorf("expected output tokens 332, got %d", assistantTurn.OutputTokens)
	}
}

func TestParseMultipleContentBlocks(t *testing.T) {
	dir := t.TempDir()
	fpath := filepath.Join(dir, "session.jsonl")

	lines := []map[string]any{
		{
			"timestamp": "2026-03-11T16:25:01.231Z",
			"type":      "session_meta",
			"payload": map[string]any{
				"id":  "test-session-1",
				"cwd": "/tmp/test",
			},
		},
		{
			"timestamp": "2026-03-11T16:25:02.000Z",
			"type":      "response_item",
			"payload": map[string]any{
				"type": "message",
				"role": "assistant",
				"content": []map[string]any{
					{"type": "output_text", "text": "First part."},
					{"type": "output_text", "text": "Second part."},
				},
			},
		},
	}

	writeJSONLFile(t, fpath, lines)

	a := New()
	session, err := a.Parse(fpath)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	if len(session.Turns) != 1 {
		t.Fatalf("expected 1 turn, got %d", len(session.Turns))
	}
	if session.Turns[0].Content != "First part.\n\nSecond part." {
		t.Errorf("unexpected content: %q", session.Turns[0].Content)
	}
}

func TestParseEmptyFile(t *testing.T) {
	dir := t.TempDir()
	fpath := filepath.Join(dir, "empty.jsonl")
	if err := os.WriteFile(fpath, []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}

	a := New()
	session, err := a.Parse(fpath)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if session.ID != "" {
		t.Errorf("expected empty session ID, got %s", session.ID)
	}
	if len(session.Turns) != 0 {
		t.Errorf("expected 0 turns, got %d", len(session.Turns))
	}
}

// writeMinimalSession creates a minimal session file with just session_meta.
func writeMinimalSession(t *testing.T, dir, fname, cwd string) {
	t.Helper()
	line := map[string]any{
		"timestamp": "2026-03-11T16:25:01.231Z",
		"type":      "session_meta",
		"payload": map[string]any{
			"id":  "test-id",
			"cwd": cwd,
		},
	}
	data, _ := json.Marshal(line)
	if err := os.WriteFile(filepath.Join(dir, fname), append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeJSONLFile writes a slice of maps as JSONL to the given path.
func writeJSONLFile(t *testing.T, path string, lines []map[string]any) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	enc := json.NewEncoder(f)
	for _, line := range lines {
		if err := enc.Encode(line); err != nil {
			t.Fatal(err)
		}
	}
}
