// ABOUTME: Tests for the Jeff source adapter covering discovery and parsing.
// ABOUTME: Uses inline test data in temp directories to validate JSONL parsing logic.

package jeff

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/2389-research/ccvault/pkg/adapter"
)

func TestJeffAdapter_Name(t *testing.T) {
	a := New()
	if a.Name() != "jeff" {
		t.Fatalf("expected name %q, got %q", "jeff", a.Name())
	}
}

func TestJeffAdapter_ImplementsInterface(t *testing.T) {
	var _ adapter.SourceAdapter = New()
}

func TestJeffAdapter_Discover(t *testing.T) {
	root := t.TempDir()
	sessionsDir := filepath.Join(root, "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Create two valid session files
	for _, name := range []string{"20260224_195605.jsonl", "20260225_100000.jsonl"} {
		line := map[string]any{
			"timestamp":       "2026-02-24T19:56:05.125559Z",
			"entry_type":      "session_start",
			"conversation_id": "d41f67af-0000-0000-0000-000000000000",
			"data": map[string]any{
				"model":      "claude-sonnet-4-5",
				"session_id": "20260224_195605",
			},
		}
		data, _ := json.Marshal(line)
		if err := os.WriteFile(filepath.Join(sessionsDir, name), append(data, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Create a non-JSONL file that should be ignored
	if err := os.WriteFile(filepath.Join(sessionsDir, "notes.txt"), []byte("ignore me"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := New()
	files, err := a.Discover(root)
	if err != nil {
		t.Fatalf("Discover error: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}

	for _, f := range files {
		if f.ProjectPath != "jeff" {
			t.Errorf("expected project path %q, got %q", "jeff", f.ProjectPath)
		}
	}
}

func TestJeffAdapter_DiscoverMissingDir(t *testing.T) {
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

func TestJeffAdapter_Parse(t *testing.T) {
	dir := t.TempDir()
	fpath := filepath.Join(dir, "20260224_195605.jsonl")

	lines := []map[string]any{
		{
			"timestamp":       "2026-02-24T19:56:05.125559Z",
			"entry_type":      "session_start",
			"conversation_id": "d41f67af-1234-5678-9abc-def012345678",
			"data": map[string]any{
				"model":      "claude-sonnet-4-5",
				"session_id": "20260224_195605",
			},
		},
		{
			"timestamp":       "2026-02-24T19:56:10.000000Z",
			"entry_type":      "user_message",
			"conversation_id": "d41f67af-1234-5678-9abc-def012345678",
			"data": map[string]any{
				"content": "Summarize the Q4 report",
			},
		},
		{
			"timestamp":       "2026-02-24T19:56:15.000000Z",
			"entry_type":      "assistant_message",
			"conversation_id": "d41f67af-1234-5678-9abc-def012345678",
			"data": map[string]any{
				"content": "Here is a summary of the Q4 report.",
			},
		},
		{
			"timestamp":       "2026-02-24T19:56:16.000000Z",
			"entry_type":      "tool_request",
			"conversation_id": "d41f67af-1234-5678-9abc-def012345678",
			"data": map[string]any{
				"tool_name": "search_drive",
				"params":    map[string]any{"query": "Q4 report"},
				"tool_id":   "tool-001",
			},
		},
		{
			"timestamp":       "2026-02-24T19:56:17.000000Z",
			"entry_type":      "tool_approved",
			"conversation_id": "d41f67af-1234-5678-9abc-def012345678",
			"data": map[string]any{
				"tool_id": "tool-001",
			},
		},
		{
			"timestamp":       "2026-02-24T19:56:18.000000Z",
			"entry_type":      "tool_result",
			"conversation_id": "d41f67af-1234-5678-9abc-def012345678",
			"data": map[string]any{
				"success":        true,
				"output_preview": "Found 3 results",
				"tool_id":        "tool-001",
			},
		},
		{
			"timestamp":       "2026-02-24T19:56:20.000000Z",
			"entry_type":      "error",
			"conversation_id": "d41f67af-1234-5678-9abc-def012345678",
			"data": map[string]any{
				"message": "rate limit exceeded",
			},
		},
		{
			"timestamp":       "2026-02-24T19:56:25.000000Z",
			"entry_type":      "session_end",
			"conversation_id": "d41f67af-1234-5678-9abc-def012345678",
			"data":            map[string]any{},
		},
	}

	writeJSONLFile(t, fpath, lines)

	a := New()
	session, err := a.Parse(fpath)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	// Check session metadata
	if session.ID != "d41f67af-1234-5678-9abc-def012345678" {
		t.Errorf("unexpected session ID: %s", session.ID)
	}
	if session.ProjectPath != "jeff" {
		t.Errorf("unexpected project path: %s", session.ProjectPath)
	}
	if session.Model != "claude-sonnet-4-5" {
		t.Errorf("unexpected model: %s", session.Model)
	}
	if session.SourceName != "jeff" {
		t.Errorf("unexpected source name: %s", session.SourceName)
	}

	// Check time range
	if session.StartedAt.IsZero() {
		t.Error("expected non-zero StartedAt")
	}
	if session.EndedAt.IsZero() {
		t.Error("expected non-zero EndedAt")
	}
	if !session.EndedAt.After(session.StartedAt) {
		t.Error("expected EndedAt after StartedAt")
	}

	// Should have 2 turns: user + assistant (other entry types are not turns)
	if len(session.Turns) != 2 {
		t.Fatalf("expected 2 turns, got %d", len(session.Turns))
	}

	// Check user turn
	userTurn := session.Turns[0]
	if userTurn.Type != "user" {
		t.Errorf("expected user turn, got %s", userTurn.Type)
	}
	if userTurn.Content != "Summarize the Q4 report" {
		t.Errorf("unexpected user content: %s", userTurn.Content)
	}
	if userTurn.RawJSON == nil {
		t.Error("expected RawJSON on user turn")
	}

	// Check assistant turn
	assistantTurn := session.Turns[1]
	if assistantTurn.Type != "assistant" {
		t.Errorf("expected assistant turn, got %s", assistantTurn.Type)
	}
	if assistantTurn.Content != "Here is a summary of the Q4 report." {
		t.Errorf("unexpected assistant content: %s", assistantTurn.Content)
	}
	if assistantTurn.RawJSON == nil {
		t.Error("expected RawJSON on assistant turn")
	}

	// Tool uses should be on the assistant turn
	if len(assistantTurn.ToolUses) != 1 {
		t.Fatalf("expected 1 tool use, got %d", len(assistantTurn.ToolUses))
	}
	if assistantTurn.ToolUses[0].ToolName != "search_drive" {
		t.Errorf("unexpected tool name: %s", assistantTurn.ToolUses[0].ToolName)
	}

	// HasError metadata should be set due to error entry
	if v, ok := session.Metadata["has_error"]; !ok || v != true {
		t.Errorf("expected has_error=true in metadata, got %v", session.Metadata)
	}
}

func TestJeffAdapter_ParseEmptyFile(t *testing.T) {
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

func TestJeffAdapter_Registration(t *testing.T) {
	a, err := adapter.Get("jeff")
	if err != nil {
		t.Fatalf("expected jeff adapter to be registered, got error: %v", err)
	}
	if a.Name() != "jeff" {
		t.Errorf("expected name %q, got %q", "jeff", a.Name())
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
