// ABOUTME: Tests for the export package
// ABOUTME: Verifies markdown export functionality

package export

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/2389-research/ccvault/pkg/models"
)

func TestNewMarkdownExporter_Defaults(t *testing.T) {
	e := NewMarkdownExporter()

	if !e.includeToolResults {
		t.Error("Default should include tool results")
	}
	if !e.includeThinking {
		t.Error("Default should include thinking")
	}
	if e.includeRawJSON {
		t.Error("Default should not include raw JSON")
	}
}

func TestNewMarkdownExporter_WithOptions(t *testing.T) {
	e := NewMarkdownExporter(
		WithToolResults(false),
		WithThinking(false),
		WithRawJSON(true),
	)

	if e.includeToolResults {
		t.Error("Should not include tool results")
	}
	if e.includeThinking {
		t.Error("Should not include thinking")
	}
	if !e.includeRawJSON {
		t.Error("Should include raw JSON")
	}
}

func TestExport_BasicSession(t *testing.T) {
	e := NewMarkdownExporter()

	session := &models.Session{
		ID:           "test-session-123",
		Model:        "claude-opus-4",
		StartedAt:    time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
		InputTokens:  1000,
		OutputTokens: 2000,
	}

	turns := []models.Turn{
		{
			ID:        "turn-1",
			SessionID: "test-session-123",
			Type:      "user",
			Timestamp: time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
			Content:   "Hello, can you help me?",
		},
		{
			ID:        "turn-2",
			SessionID: "test-session-123",
			Type:      "assistant",
			Timestamp: time.Date(2024, 1, 15, 10, 30, 5, 0, time.UTC),
			Content:   "Of course! I'd be happy to help.",
		},
	}

	var buf bytes.Buffer
	err := e.Export(&buf, session, turns, "/path/to/project")

	if err != nil {
		t.Fatalf("Export failed: %v", err)
	}

	output := buf.String()

	// Check for required sections
	if !strings.Contains(output, "# Conversation:") {
		t.Error("Missing conversation header")
	}
	if !strings.Contains(output, "## Metadata") {
		t.Error("Missing metadata section")
	}
	if !strings.Contains(output, "## Conversation") {
		t.Error("Missing conversation section")
	}
	if !strings.Contains(output, "test-session-123") {
		t.Error("Missing session ID in output")
	}
	if !strings.Contains(output, "/path/to/project") {
		t.Error("Missing project path in output")
	}
	if !strings.Contains(output, "claude-opus-4") {
		t.Error("Missing model in output")
	}
	if !strings.Contains(output, "User") {
		t.Error("Missing user turn")
	}
	if !strings.Contains(output, "Assistant") {
		t.Error("Missing assistant turn")
	}
	if !strings.Contains(output, "Hello, can you help me?") {
		t.Error("Missing user content")
	}
	if !strings.Contains(output, "I'd be happy to help") {
		t.Error("Missing assistant content")
	}
}

func TestExport_NoProjectPath(t *testing.T) {
	e := NewMarkdownExporter()

	session := &models.Session{
		ID:        "test-session-123",
		Model:     "claude-opus-4",
		StartedAt: time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
	}

	var buf bytes.Buffer
	err := e.Export(&buf, session, nil, "")

	if err != nil {
		t.Fatalf("Export failed: %v", err)
	}

	output := buf.String()

	// Should not have project row if no project path
	if strings.Contains(output, "| **Project**") {
		t.Error("Should not have project row without project path")
	}
}

func TestFormatTokens(t *testing.T) {
	tests := []struct {
		input    int64
		expected string
	}{
		{0, "0"},
		{100, "100"},
		{999, "999"},
		{1000, "1.0K"},
		{1500, "1.5K"},
		{999999, "1000.0K"},
		{1000000, "1.0M"},
		{1500000, "1.5M"},
	}

	for _, tt := range tests {
		result := formatTokens(tt.input)
		if result != tt.expected {
			t.Errorf("formatTokens(%d) = %s, expected %s", tt.input, result, tt.expected)
		}
	}
}

func TestExtractUserContent_StringContent(t *testing.T) {
	rawJSON := []byte(`{"message": {"content": "Hello world"}}`)

	content := extractUserContent(rawJSON)

	if content != "Hello world" {
		t.Errorf("Expected 'Hello world', got '%s'", content)
	}
}

func TestExtractUserContent_ArrayContent(t *testing.T) {
	rawJSON := []byte(`{"message": {"content": [{"type": "text", "text": "Part 1"}, {"type": "text", "text": "Part 2"}]}}`)

	content := extractUserContent(rawJSON)

	if content != "Part 1\nPart 2" {
		t.Errorf("Expected 'Part 1\\nPart 2', got '%s'", content)
	}
}

func TestExtractUserContent_InvalidJSON(t *testing.T) {
	rawJSON := []byte(`invalid json`)

	content := extractUserContent(rawJSON)

	if content != "" {
		t.Errorf("Expected empty string for invalid JSON, got '%s'", content)
	}
}

func TestExport_WithSource(t *testing.T) {
	e := NewMarkdownExporter()

	session := &models.Session{
		ID:        "test-session-456",
		Model:     "claude-opus-4",
		StartedAt: time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
		Source:    "codex",
	}

	var buf bytes.Buffer
	err := e.Export(&buf, session, nil, "")

	if err != nil {
		t.Fatalf("Export failed: %v", err)
	}

	output := buf.String()

	if !strings.Contains(output, "| **Source** | codex |") {
		t.Error("Missing source row in metadata")
	}
}

func TestExport_WithoutSource(t *testing.T) {
	e := NewMarkdownExporter()

	session := &models.Session{
		ID:        "test-session-789",
		Model:     "claude-opus-4",
		StartedAt: time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
	}

	var buf bytes.Buffer
	err := e.Export(&buf, session, nil, "")

	if err != nil {
		t.Fatalf("Export failed: %v", err)
	}

	output := buf.String()

	// Should not have source row if source is empty
	if strings.Contains(output, "| **Source**") {
		t.Error("Should not have source row without source value")
	}
}

func TestExport_WithDuration(t *testing.T) {
	e := NewMarkdownExporter()

	session := &models.Session{
		ID:        "test-session-123",
		Model:     "claude-opus-4",
		StartedAt: time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
		EndedAt:   time.Date(2024, 1, 15, 11, 0, 0, 0, time.UTC),
	}

	var buf bytes.Buffer
	err := e.Export(&buf, session, nil, "")

	if err != nil {
		t.Fatalf("Export failed: %v", err)
	}

	output := buf.String()

	// Should show ended time and duration
	if !strings.Contains(output, "**Ended**") {
		t.Error("Missing ended time")
	}
	if !strings.Contains(output, "**Duration**") {
		t.Error("Missing duration")
	}
	if !strings.Contains(output, "30m") {
		t.Error("Duration should be 30 minutes")
	}
}
