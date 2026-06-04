// ABOUTME: Tests for the Hex source adapter that parses Hex agentic loop session files.
// ABOUTME: Validates interface compliance, Name(), Discover(), Parse(), and registration behavior.

package hex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/2389-research/ccvault/pkg/adapter"
)

func TestHexAdapter_Name(t *testing.T) {
	a := New()
	if got := a.Name(); got != "hex" {
		t.Errorf("Name() = %q, want %q", got, "hex")
	}
}

func TestHexAdapter_ImplementsInterface(t *testing.T) {
	var _ adapter.SourceAdapter = New()
}

func TestHexAdapter_Discover(t *testing.T) {
	root := t.TempDir()
	sessionsDir := filepath.Join(root, "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Create two session files and one non-JSON file
	session1 := `{"id":"sess-1","title":"test 1","created_at":"2026-01-12T11:48:34Z","updated_at":"2026-01-12T11:48:50Z","messages":[],"favorite":false}`
	session2 := `{"id":"sess-2","title":"test 2","created_at":"2026-01-13T10:00:00Z","updated_at":"2026-01-13T10:05:00Z","messages":[],"favorite":true}`

	if err := os.WriteFile(filepath.Join(sessionsDir, "sess-1.json"), []byte(session1), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionsDir, "sess-2.json"), []byte(session2), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionsDir, "notes.txt"), []byte("not a session"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := New()
	files, err := a.Discover(root)
	if err != nil {
		t.Fatalf("Discover() error: %v", err)
	}

	if len(files) != 2 {
		t.Fatalf("Discover() returned %d files, want 2", len(files))
	}

	wantProjectPath := projectPathForRoot(root)
	for _, sf := range files {
		if sf.ProjectPath != wantProjectPath {
			t.Errorf("ProjectPath = %q, want %q", sf.ProjectPath, wantProjectPath)
		}
		if sf.ModTime.IsZero() {
			t.Error("ModTime is zero")
		}
		if filepath.Ext(sf.Path) != ".json" {
			t.Errorf("Path %q does not have .json extension", sf.Path)
		}
	}
}

func TestHexAdapter_Discover_MissingDir(t *testing.T) {
	root := t.TempDir()
	// No sessions/ subdirectory exists

	a := New()
	files, err := a.Discover(root)
	if err != nil {
		t.Fatalf("Discover() error: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("Discover() returned %d files, want 0", len(files))
	}
}

func TestHexAdapter_Parse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test-session.json")

	sessionJSON := `{
		"id": "abc-123",
		"title": "Test conversation",
		"created_at": "2026-01-12T11:48:34.247188-06:00",
		"updated_at": "2026-01-12T11:48:50.530819-06:00",
		"messages": [
			{
				"role": "user",
				"content": "hello there",
				"timestamp": "2026-01-12T11:48:34.247188-06:00"
			},
			{
				"role": "assistant",
				"content": "hi back",
				"timestamp": "2026-01-12T11:48:50.530819-06:00"
			}
		],
		"favorite": false
	}`

	if err := os.WriteFile(path, []byte(sessionJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	a := New()
	parsed, err := a.Parse(path)
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	if parsed.ID != "hex:abc-123" {
		t.Errorf("ID = %q, want %q", parsed.ID, "hex:abc-123")
	}
	// ProjectPath is intentionally empty on the parsed session; sync layer
	// fills it from the SessionFile produced by Discover().
	if parsed.ProjectPath != "" {
		t.Errorf("ProjectPath = %q, want empty", parsed.ProjectPath)
	}
	if parsed.SourceName != "hex" {
		t.Errorf("SourceName = %q, want %q", parsed.SourceName, "hex")
	}
	if parsed.Model != "" {
		t.Errorf("Model = %q, want empty", parsed.Model)
	}

	// Check timestamps
	expectedStart, err := time.Parse(time.RFC3339Nano, "2026-01-12T11:48:34.247188-06:00")
	if err != nil {
		t.Fatalf("parse expectedStart: %v", err)
	}
	expectedEnd, err := time.Parse(time.RFC3339Nano, "2026-01-12T11:48:50.530819-06:00")
	if err != nil {
		t.Fatalf("parse expectedEnd: %v", err)
	}
	if !parsed.StartedAt.Equal(expectedStart) {
		t.Errorf("StartedAt = %v, want %v", parsed.StartedAt, expectedStart)
	}
	if !parsed.EndedAt.Equal(expectedEnd) {
		t.Errorf("EndedAt = %v, want %v", parsed.EndedAt, expectedEnd)
	}

	// Check turns
	if len(parsed.Turns) != 2 {
		t.Fatalf("len(Turns) = %d, want 2", len(parsed.Turns))
	}

	// First turn
	turn0 := parsed.Turns[0]
	if turn0.ID != "hex:abc-123-0" {
		t.Errorf("Turns[0].ID = %q, want %q", turn0.ID, "hex:abc-123-0")
	}
	if turn0.ParentID != "" {
		t.Errorf("Turns[0].ParentID = %q, want empty", turn0.ParentID)
	}
	if turn0.Type != "user" {
		t.Errorf("Turns[0].Type = %q, want %q", turn0.Type, "user")
	}
	if turn0.Content != "hello there" {
		t.Errorf("Turns[0].Content = %q, want %q", turn0.Content, "hello there")
	}
	if turn0.RawJSON == nil {
		t.Error("Turns[0].RawJSON is nil")
	}

	// Second turn
	turn1 := parsed.Turns[1]
	if turn1.ID != "hex:abc-123-1" {
		t.Errorf("Turns[1].ID = %q, want %q", turn1.ID, "hex:abc-123-1")
	}
	if turn1.ParentID != "hex:abc-123-0" {
		t.Errorf("Turns[1].ParentID = %q, want %q", turn1.ParentID, "hex:abc-123-0")
	}
	if turn1.Type != "assistant" {
		t.Errorf("Turns[1].Type = %q, want %q", turn1.Type, "assistant")
	}
	if turn1.Content != "hi back" {
		t.Errorf("Turns[1].Content = %q, want %q", turn1.Content, "hi back")
	}

	// Verify RawJSON is valid JSON
	var raw map[string]interface{}
	if err := json.Unmarshal(turn1.RawJSON, &raw); err != nil {
		t.Errorf("Turns[1].RawJSON is not valid JSON: %v", err)
	}

	// No tool uses, errors, or tokens
	if len(turn1.ToolUses) != 0 {
		t.Errorf("len(Turns[1].ToolUses) = %d, want 0", len(turn1.ToolUses))
	}
	if turn1.HasError {
		t.Error("Turns[1].HasError should be false")
	}
}

func TestHexAdapter_Registration(t *testing.T) {
	a, err := adapter.Get("hex")
	if err != nil {
		t.Fatalf("adapter.Get(\"hex\") error: %v", err)
	}
	if a.Name() != "hex" {
		t.Errorf("registered adapter Name() = %q, want %q", a.Name(), "hex")
	}
}

func TestHexAdapter_Parse_EmptyMessages(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty-session.json")

	sessionJSON := `{
		"id": "empty-123",
		"title": "Empty conversation",
		"created_at": "2026-01-12T11:48:34Z",
		"updated_at": "2026-01-12T11:48:34Z",
		"messages": [],
		"favorite": false
	}`

	if err := os.WriteFile(path, []byte(sessionJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	a := New()
	parsed, err := a.Parse(path)
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	if parsed.ID != "hex:empty-123" {
		t.Errorf("ID = %q, want %q", parsed.ID, "hex:empty-123")
	}
	if len(parsed.Turns) != 0 {
		t.Errorf("len(Turns) = %d, want 0", len(parsed.Turns))
	}
	if parsed.SourceName != "hex" {
		t.Errorf("SourceName = %q, want %q", parsed.SourceName, "hex")
	}
}
