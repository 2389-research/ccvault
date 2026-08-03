// ABOUTME: Tests for JSONL parser functionality
// ABOUTME: Validates parsing of Claude Code conversation formats

package parser

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/2389-research/ccvault/pkg/models"
)

func TestParseTurn_UserMessage(t *testing.T) {
	input := `{"parentUuid":"85e5053c-9b1a-4d3a-aebe-ac044bd6170e","isSidechain":false,"userType":"external","cwd":"/Users/harper/project","sessionId":"0684b40f-4463-4492-83c6-3baa18bfb9ad","version":"2.1.29","gitBranch":"main","type":"user","message":{"role":"user","content":"Hello, can you help me?"},"uuid":"abc12345-1234-1234-1234-123456789abc","timestamp":"2026-02-02T20:48:10.345Z"}`

	turn, err := ParseTurn([]byte(input))
	if err != nil {
		t.Fatalf("ParseTurn failed: %v", err)
	}

	if turn == nil {
		t.Fatal("Expected turn, got nil")
	}

	if turn.ID != "abc12345-1234-1234-1234-123456789abc" {
		t.Errorf("Expected ID abc12345-1234-1234-1234-123456789abc, got %s", turn.ID)
	}

	if turn.Type != "user" {
		t.Errorf("Expected type user, got %s", turn.Type)
	}

	if turn.SessionID != "0684b40f-4463-4492-83c6-3baa18bfb9ad" {
		t.Errorf("Expected session ID 0684b40f-4463-4492-83c6-3baa18bfb9ad, got %s", turn.SessionID)
	}

	if turn.Content != "Hello, can you help me?" {
		t.Errorf("Expected content 'Hello, can you help me?', got %s", turn.Content)
	}

	if turn.ParentID != "85e5053c-9b1a-4d3a-aebe-ac044bd6170e" {
		t.Errorf("Expected parent ID 85e5053c-9b1a-4d3a-aebe-ac044bd6170e, got %s", turn.ParentID)
	}
}

func TestParseTurn_AssistantMessage(t *testing.T) {
	input := `{"type":"assistant","uuid":"def67890-1234-1234-1234-123456789abc","sessionId":"session123","timestamp":"2026-02-02T20:49:00.000Z","message":{"model":"claude-opus-4-5-20251101","id":"msg_123","type":"message","role":"assistant","content":[{"type":"text","text":"I can help you with that!"}],"usage":{"input_tokens":100,"output_tokens":50}}}`

	turn, err := ParseTurn([]byte(input))
	if err != nil {
		t.Fatalf("ParseTurn failed: %v", err)
	}

	if turn.Type != "assistant" {
		t.Errorf("Expected type assistant, got %s", turn.Type)
	}

	if turn.Content != "I can help you with that!" {
		t.Errorf("Expected content 'I can help you with that!', got %s", turn.Content)
	}

	if turn.InputTokens != 100 {
		t.Errorf("Expected 100 input tokens, got %d", turn.InputTokens)
	}

	if turn.OutputTokens != 50 {
		t.Errorf("Expected 50 output tokens, got %d", turn.OutputTokens)
	}
}

func TestParseTurn_SkipsNonTurns(t *testing.T) {
	// file-history-snapshot entries should be skipped
	input := `{"type":"file-history-snapshot","messageId":"123","snapshot":{}}`

	turn, err := ParseTurn([]byte(input))
	if err != nil {
		t.Fatalf("ParseTurn failed: %v", err)
	}

	if turn != nil {
		t.Error("Expected nil for file-history-snapshot, got turn")
	}
}

func TestParseTimestamp(t *testing.T) {
	tests := []struct {
		input    string
		expected time.Time
		wantErr  bool
	}{
		{
			input:    "2026-02-02T20:48:10.345Z",
			expected: time.Date(2026, 2, 2, 20, 48, 10, 345000000, time.UTC),
			wantErr:  false,
		},
		{
			input:    "2026-02-02T20:48:10Z",
			expected: time.Date(2026, 2, 2, 20, 48, 10, 0, time.UTC),
			wantErr:  false,
		},
		{
			input:   "",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			result, err := parseTimestamp(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Error("Expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}
			if !result.Equal(tc.expected) {
				t.Errorf("Expected %v, got %v", tc.expected, result)
			}
		})
	}
}

func TestParseSessionReader(t *testing.T) {
	input := `{"type":"file-history-snapshot","messageId":"msg1","snapshot":{}}
{"uuid":"turn1","sessionId":"session1","type":"user","timestamp":"2026-02-02T20:00:00.000Z","message":{"role":"user","content":"Hello"}}
{"uuid":"turn2","sessionId":"session1","type":"assistant","timestamp":"2026-02-02T20:01:00.000Z","message":{"model":"claude-opus-4-5-20251101","role":"assistant","content":[{"type":"text","text":"Hi there!"}],"usage":{"input_tokens":10,"output_tokens":5}}}`

	turns, session, _, err := ParseSessionReader(strings.NewReader(input), "/test/session.jsonl")
	if err != nil {
		t.Fatalf("ParseSessionReader failed: %v", err)
	}

	if len(turns) != 2 {
		t.Errorf("Expected 2 turns, got %d", len(turns))
	}

	if session.ID != "session1" {
		t.Errorf("Expected session ID session1, got %s", session.ID)
	}

	if session.TurnCount != 2 {
		t.Errorf("Expected turn count 2, got %d", session.TurnCount)
	}

	if session.Model != "claude-opus-4-5-20251101" {
		t.Errorf("Expected model claude-opus-4-5-20251101, got %s", session.Model)
	}

	if session.InputTokens != 10 {
		t.Errorf("Expected 10 input tokens, got %d", session.InputTokens)
	}

	if session.SourceFile != "/test/session.jsonl" {
		t.Errorf("Expected source file /test/session.jsonl, got %s", session.SourceFile)
	}
}

func TestIsValidUUID(t *testing.T) {
	tests := []struct {
		input string
		valid bool
	}{
		{"0684b40f-4463-4492-83c6-3baa18bfb9ad", true},
		{"ABC12345-1234-1234-1234-123456789ABC", true},
		{"not-a-uuid", false},
		{"0684b40f44634492-83c6-3baa18bfb9ad", false}, // Wrong dash positions
		{"", false},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			result := isValidUUID(tc.input)
			if result != tc.valid {
				t.Errorf("isValidUUID(%s) = %v, want %v", tc.input, result, tc.valid)
			}
		})
	}
}

func TestParseSessionReader_ExtractsCWD(t *testing.T) {
	// CWD field in JSONL turns should be extracted as the session's ProjectPath.
	// This is the ground truth for project paths, avoiding lossy decoding from
	// the encoded directory name (where all non-alphanumeric chars become dashes).
	input := `{"uuid":"turn1","sessionId":"sess1","type":"user","timestamp":"2026-02-02T20:00:00.000Z","cwd":"/Users/harper/canvas-plugins","message":{"role":"user","content":"Hello"}}
{"uuid":"turn2","sessionId":"sess1","type":"assistant","timestamp":"2026-02-02T20:01:00.000Z","cwd":"/Users/harper/canvas-plugins","message":{"model":"claude-opus-4-5-20251101","role":"assistant","content":[{"type":"text","text":"Hi!"}],"usage":{"input_tokens":10,"output_tokens":5}}}`

	_, session, _, err := ParseSessionReader(strings.NewReader(input), "/test/session.jsonl")
	if err != nil {
		t.Fatalf("ParseSessionReader failed: %v", err)
	}

	if session.ProjectPath != "/Users/harper/canvas-plugins" {
		t.Errorf("Expected ProjectPath '/Users/harper/canvas-plugins', got %q", session.ProjectPath)
	}
}

func TestParseSessionReader_CWDNotOverwrittenByEmpty(t *testing.T) {
	// Once CWD is set from the first turn, later turns without CWD should not clear it.
	input := `{"uuid":"turn1","sessionId":"sess1","type":"user","timestamp":"2026-02-02T20:00:00.000Z","cwd":"/Users/harper/buddy-web","message":{"role":"user","content":"Hello"}}
{"uuid":"turn2","sessionId":"sess1","type":"assistant","timestamp":"2026-02-02T20:01:00.000Z","message":{"model":"claude-opus-4-5-20251101","role":"assistant","content":[{"type":"text","text":"Hi!"}],"usage":{"input_tokens":10,"output_tokens":5}}}`

	_, session, _, err := ParseSessionReader(strings.NewReader(input), "/test/session.jsonl")
	if err != nil {
		t.Fatalf("ParseSessionReader failed: %v", err)
	}

	if session.ProjectPath != "/Users/harper/buddy-web" {
		t.Errorf("Expected ProjectPath '/Users/harper/buddy-web', got %q", session.ProjectPath)
	}
}

func TestParseSessionReader_CWDLocksToFirstValue(t *testing.T) {
	// If CWD changes mid-session (e.g. user cd's), the first value wins.
	input := `{"uuid":"turn1","sessionId":"sess1","type":"user","timestamp":"2026-02-02T20:00:00.000Z","cwd":"/Users/harper/project-a","message":{"role":"user","content":"Hello"}}
{"uuid":"turn2","sessionId":"sess1","type":"user","timestamp":"2026-02-02T20:01:00.000Z","cwd":"/Users/harper/project-b","message":{"role":"user","content":"Changed dir"}}`

	_, session, _, err := ParseSessionReader(strings.NewReader(input), "/test/session.jsonl")
	if err != nil {
		t.Fatalf("ParseSessionReader failed: %v", err)
	}

	if session.ProjectPath != "/Users/harper/project-a" {
		t.Errorf("Expected ProjectPath to lock to first value '/Users/harper/project-a', got %q", session.ProjectPath)
	}
}

func TestParseSessionReader_GitBranchExtracted(t *testing.T) {
	// Verify git branch extraction is preserved after consolidation of unmarshal calls.
	input := `{"uuid":"turn1","sessionId":"sess1","type":"user","timestamp":"2026-02-02T20:00:00.000Z","cwd":"/Users/harper/project","gitBranch":"feature/cool-thing","message":{"role":"user","content":"Hello"}}
{"uuid":"turn2","sessionId":"sess1","type":"assistant","timestamp":"2026-02-02T20:01:00.000Z","message":{"model":"claude-opus-4-5-20251101","role":"assistant","content":[{"type":"text","text":"Hi!"}],"usage":{"input_tokens":10,"output_tokens":5}}}`

	_, session, _, err := ParseSessionReader(strings.NewReader(input), "/test/session.jsonl")
	if err != nil {
		t.Fatalf("ParseSessionReader failed: %v", err)
	}

	if session.GitBranch != "feature/cool-thing" {
		t.Errorf("Expected GitBranch 'feature/cool-thing', got %q", session.GitBranch)
	}
}

func TestDecodeProjectPath(t *testing.T) {
	// decodeProjectPath is a lossy fallback: it replaces ALL dashes with slashes,
	// so paths with real dashes (e.g. "canvas-plugins") are decoded incorrectly.
	// The CWD field from JSONL is the authoritative source for project paths.
	tests := []struct {
		input    string
		expected string
	}{
		{"-Users-harper-project", "/Users/harper/project"},
		{"-Users-harper-Public-src-2389-ccvault", "/Users/harper/Public/src/2389/ccvault"},
		{"simple", "simple"},
		// Demonstrates the lossy behavior: "canvas-plugins" → "canvas/plugins" (wrong!)
		{"-Users-harper-canvas-plugins", "/Users/harper/canvas/plugins"},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			result := decodeProjectPath(tc.input)
			if result != tc.expected {
				t.Errorf("decodeProjectPath(%s) = %s, want %s", tc.input, result, tc.expected)
			}
		})
	}
}

func TestGetDisplayName(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"/Users/harper/Public/src/2389/ccvault", "src/2389/ccvault"},
		{"/short/path", "/short/path"},
		{"/a/b/c/d/e", "c/d/e"},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			result := GetDisplayName(tc.input)
			if result != tc.expected {
				t.Errorf("GetDisplayName(%s) = %s, want %s", tc.input, result, tc.expected)
			}
		})
	}
}

// TestParseSessionReader_OversizedLineDoesNotDropSession is the repro for
// https://github.com/2389-research/ccvault/issues/11.
//
// Claude Code writes PDF Read results as base64 image blocks inside a single
// JSONL line. A moderately sized PDF (~8 MB) produces lines of ~12 MB. When the
// parser was backed by bufio.Scanner with a 10 MB cap, one oversized line
// aborted the whole scan with bufio.ErrTooLong, dropping every turn in the
// file. The fix reads with bufio.Reader instead, which has no per-line cap.
func TestParseSessionReader_OversizedLineDoesNotDropSession(t *testing.T) {
	// ~12 MB of ASCII inside a valid JSON string. This mirrors the shape of a
	// base64-encoded PDF page block that Claude Code embeds in a user turn.
	oversized := strings.Repeat("A", 12*1024*1024)

	input := fmt.Sprintf(
		`{"uuid":"turn-1","sessionId":"sess-oversized","type":"user","timestamp":"2026-08-04T10:00:00.000Z","message":{"role":"user","content":"Hello"}}
{"uuid":"turn-huge","sessionId":"sess-oversized","type":"user","timestamp":"2026-08-04T10:00:01.000Z","message":{"role":"user","content":"%s"}}
{"uuid":"turn-3","sessionId":"sess-oversized","type":"assistant","timestamp":"2026-08-04T10:00:02.000Z","message":{"model":"claude-opus-4-5-20251101","role":"assistant","content":[{"type":"text","text":"World"}],"usage":{"input_tokens":10,"output_tokens":5}}}`,
		oversized,
	)

	turns, session, skipped, err := ParseSessionReader(strings.NewReader(input), "/test/oversized.jsonl")
	if err != nil {
		t.Fatalf("ParseSessionReader failed on file with oversized line: %v", err)
	}

	if len(turns) != 3 {
		t.Fatalf("expected 3 turns (oversized line preserved), got %d", len(turns))
	}
	if skipped != 0 {
		t.Errorf("expected 0 skipped lines (oversized ≠ malformed), got %d", skipped)
	}

	if turns[0].Content != "Hello" {
		t.Errorf("first turn content = %q, want %q", turns[0].Content, "Hello")
	}
	if turns[2].Content != "World" {
		t.Errorf("last turn content = %q, want %q", turns[2].Content, "World")
	}

	if session.ID != "sess-oversized" {
		t.Errorf("session.ID = %q, want %q", session.ID, "sess-oversized")
	}
	if session.TurnCount != 3 {
		t.Errorf("session.TurnCount = %d, want 3", session.TurnCount)
	}
}

// TestParseSessionReader_MalformedLinesAreSkippedAndCounted verifies that
// individual lines that fail JSON parsing are silently skipped (preserving the
// prior behavior) but that the count is reported to callers so sync can
// surface the fact that content was dropped.
func TestParseSessionReader_MalformedLinesAreSkippedAndCounted(t *testing.T) {
	input := `{"uuid":"turn-1","sessionId":"sess-mal","type":"user","timestamp":"2026-08-04T10:00:00.000Z","message":{"role":"user","content":"Hello"}}
this is not valid json at all
{"unclosed":
{"uuid":"turn-2","sessionId":"sess-mal","type":"assistant","timestamp":"2026-08-04T10:00:01.000Z","message":{"model":"claude","role":"assistant","content":[{"type":"text","text":"World"}],"usage":{"input_tokens":1,"output_tokens":1}}}`

	turns, session, skipped, err := ParseSessionReader(strings.NewReader(input), "/test/malformed.jsonl")
	if err != nil {
		t.Fatalf("ParseSessionReader failed: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("expected 2 turns (2 malformed lines skipped), got %d", len(turns))
	}
	if skipped != 2 {
		t.Errorf("expected 2 skipped lines, got %d", skipped)
	}
	if session.ID != "sess-mal" {
		t.Errorf("session.ID = %q, want %q", session.ID, "sess-mal")
	}
}

// --- readLineBounded ---

func TestReadLineBounded_NormalLines(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("hello\nworld\n"))

	line, oversized, err := readLineBounded(r, 100)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if oversized {
		t.Fatal("first line should not be oversized")
	}
	if string(line) != "hello" {
		t.Errorf("got %q, want hello", line)
	}

	line, oversized, err = readLineBounded(r, 100)
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if oversized {
		t.Fatal("second line should not be oversized")
	}
	if string(line) != "world" {
		t.Errorf("got %q, want world", line)
	}

	_, _, err = readLineBounded(r, 100)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("want EOF, got %v", err)
	}
}

func TestReadLineBounded_EmptyLine(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("\nhi\n"))
	line, oversized, err := readLineBounded(r, 100)
	if err != nil {
		t.Fatal(err)
	}
	if oversized {
		t.Fatal("empty line marked oversized")
	}
	if len(line) != 0 {
		t.Errorf("expected empty line, got %q", line)
	}
}

func TestReadLineBounded_NoTrailingNewline(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("hello"))
	line, oversized, err := readLineBounded(r, 100)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("want EOF, got %v", err)
	}
	if oversized {
		t.Fatal("unexpected oversized")
	}
	if string(line) != "hello" {
		t.Errorf("got %q, want hello", line)
	}
}

func TestReadLineBounded_CRLF(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("hello\r\nworld\r\n"))

	line, _, err := readLineBounded(r, 100)
	if err != nil {
		t.Fatal(err)
	}
	if string(line) != "hello" {
		t.Errorf("first line got %q, want hello", line)
	}

	line, _, err = readLineBounded(r, 100)
	if err != nil {
		t.Fatal(err)
	}
	if string(line) != "world" {
		t.Errorf("second line got %q, want world", line)
	}
}

func TestReadLineBounded_ExactCap(t *testing.T) {
	// A line of exactly maxBytes chars should be accepted.
	r := bufio.NewReader(strings.NewReader("hello\n"))
	line, oversized, err := readLineBounded(r, 5)
	if err != nil {
		t.Fatal(err)
	}
	if oversized {
		t.Fatal("exactly-at-cap should not be oversized")
	}
	if string(line) != "hello" {
		t.Errorf("got %q, want hello", line)
	}
}

func TestReadLineBounded_OversizedMiddleLineIsSkippedAndDrained(t *testing.T) {
	// Line 1 fits under cap, line 2 exceeds cap and must be drained cleanly
	// so line 3 reads correctly.
	input := "hi\n" + strings.Repeat("A", 20) + "\nend\n"
	r := bufio.NewReader(strings.NewReader(input))

	line, oversized, err := readLineBounded(r, 5)
	if err != nil {
		t.Fatal(err)
	}
	if oversized {
		t.Fatal("first line should not be oversized")
	}
	if string(line) != "hi" {
		t.Errorf("got %q", line)
	}

	line, oversized, err = readLineBounded(r, 5)
	if err != nil {
		t.Fatalf("drain unexpectedly errored: %v", err)
	}
	if !oversized {
		t.Fatal("second (20-char) line should be oversized under maxBytes=5")
	}
	if line != nil {
		t.Errorf("oversized should return nil line, got %q", line)
	}

	line, oversized, err = readLineBounded(r, 5)
	if err != nil {
		t.Fatal(err)
	}
	if oversized {
		t.Fatal("third line should not be oversized")
	}
	if string(line) != "end" {
		t.Errorf("got %q, want end", line)
	}
}

func TestReadLineBounded_LineLargerThanBufferButUnderCap(t *testing.T) {
	// The default bufio buffer is 4096 bytes. Force a line to span multiple
	// internal buffers to exercise the ErrBufferFull accumulator path.
	big := strings.Repeat("A", 20000)
	r := bufio.NewReader(strings.NewReader(big + "\n"))
	line, oversized, err := readLineBounded(r, 100000)
	if err != nil {
		t.Fatal(err)
	}
	if oversized {
		t.Fatal("under-cap line marked oversized")
	}
	if len(line) != 20000 {
		t.Errorf("got len=%d, want 20000", len(line))
	}
}

func TestReadLineBounded_OversizedAtEOFWithoutNewline(t *testing.T) {
	// A too-long line with no trailing newline should still be rejected
	// as oversized and surface io.EOF.
	r := bufio.NewReader(strings.NewReader(strings.Repeat("A", 100)))
	line, oversized, err := readLineBounded(r, 50)
	if !oversized {
		t.Fatal("expected oversized")
	}
	if line != nil {
		t.Errorf("expected nil line, got len=%d", len(line))
	}
	if !errors.Is(err, io.EOF) {
		t.Fatalf("want EOF, got %v", err)
	}
}

// --- strippedRawJSON ---

func TestStrippedRawJSON_ShapeAndFields(t *testing.T) {
	raw := &models.RawTurn{
		UUID:       "u-1234",
		ParentUUID: "u-0000",
		SessionID:  "sess-abc",
		Type:       "user",
		Timestamp:  "2026-08-04T10:00:00Z",
	}

	out := strippedRawJSON(raw, 12583168)

	if len(out) > 500 {
		t.Errorf("stripped placeholder too large: %d bytes (want < 500)", len(out))
	}

	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("placeholder is not valid JSON: %v", err)
	}

	if parsed["_ccvault_stripped"] != true {
		t.Errorf("_ccvault_stripped = %v, want true", parsed["_ccvault_stripped"])
	}
	if got, want := parsed["_ccvault_original_size"], float64(12583168); got != want {
		t.Errorf("_ccvault_original_size = %v, want %v", got, want)
	}
	if parsed["uuid"] != "u-1234" {
		t.Errorf("uuid = %v, want u-1234", parsed["uuid"])
	}
	if parsed["parentUuid"] != "u-0000" {
		t.Errorf("parentUuid = %v, want u-0000", parsed["parentUuid"])
	}
	if parsed["sessionId"] != "sess-abc" {
		t.Errorf("sessionId = %v, want sess-abc", parsed["sessionId"])
	}
	if parsed["type"] != "user" {
		t.Errorf("type = %v, want user", parsed["type"])
	}
	if parsed["timestamp"] != "2026-08-04T10:00:00Z" {
		t.Errorf("timestamp = %v, want 2026-08-04T10:00:00Z", parsed["timestamp"])
	}
}

func TestStrippedRawJSON_OmitsEmptyParentUUID(t *testing.T) {
	raw := &models.RawTurn{
		UUID:      "u-root",
		SessionID: "sess-1",
		Type:      "user",
		Timestamp: "2026-08-04T10:00:00Z",
	}
	out := strippedRawJSON(raw, 100)

	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	if _, present := parsed["parentUuid"]; present {
		t.Error("parentUuid should be omitted when empty")
	}
}

func TestStrippedRawJSON_UnmarshalsAsRawTurn(t *testing.T) {
	// Existing consumers (TUI, MCP, export) json.Unmarshal turns.raw_json into
	// models.RawTurn. The placeholder should decode into that shape without
	// error, even if the message body is absent.
	raw := &models.RawTurn{
		UUID:      "u-1234",
		SessionID: "sess-abc",
		Type:      "user",
		Timestamp: "2026-08-04T10:00:00Z",
	}
	out := strippedRawJSON(raw, 100)

	var back models.RawTurn
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("placeholder failed to decode as RawTurn: %v", err)
	}
	if back.UUID != raw.UUID {
		t.Errorf("UUID = %q, want %q", back.UUID, raw.UUID)
	}
	if back.Type != raw.Type {
		t.Errorf("Type = %q, want %q", back.Type, raw.Type)
	}
	if len(back.Message) != 0 {
		t.Errorf("placeholder should have no Message body, got %s", back.Message)
	}
}
