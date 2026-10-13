// ABOUTME: Tests that the jeff adapter reports turns whose transcript timestamp its layout could not read.
// ABOUTME: Guards #146 for jeff, whose single RFC3339Nano parse drops the error and leaves the turn at the zero time.

package jeff

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestParse_ReportsUnparsableTimestamps pins jeff's half of #146. See the
// codex test of the same name for why the count and the sample are what the
// three un-consolidated layout lists actually needed.
func TestParse_ReportsUnparsableTimestamps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "20260526_143000.jsonl")

	const bad = "26 May 2026 14:30:01"

	writeJeffLines(t, path, []map[string]any{
		{
			"timestamp":       "2026-05-26T14:30:00Z",
			"entry_type":      "session_start",
			"conversation_id": "conv-1",
			"data":            map[string]any{"model": "gemini-3-pro", "session_id": "conv-1"},
		},
		{
			"timestamp":       "2026-05-26T14:30:00.500Z",
			"entry_type":      "user_message",
			"conversation_id": "conv-1",
			"data":            map[string]any{"content": "readable"},
		},
		{
			"timestamp":       bad,
			"entry_type":      "assistant_message",
			"conversation_id": "conv-1",
			"data":            map[string]any{"content": "unreadable clock"},
		},
	})

	session, err := New().Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(session.Turns) != 2 {
		t.Fatalf("len(Turns) = %d, want 2 — an unreadable timestamp must not drop the turn", len(session.Turns))
	}

	if got := session.Metadata["turns_with_unparsable_timestamp"]; got != 1 {
		t.Errorf("turns_with_unparsable_timestamp = %v, want 1", got)
	}
	if got := session.Metadata["unparsable_timestamp_sample"]; got != bad {
		t.Errorf("unparsable_timestamp_sample = %v, want %q", got, bad)
	}
}

// TestParse_CleanTranscriptReportsNoUnparsableTimestamps keeps the counter
// quiet on good input, which is the only way it stays worth reading.
func TestParse_CleanTranscriptReportsNoUnparsableTimestamps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "20260526_143000.jsonl")

	writeJeffLines(t, path, []map[string]any{
		{
			"timestamp":       "2026-05-26T14:30:00Z",
			"entry_type":      "user_message",
			"conversation_id": "conv-2",
			"data":            map[string]any{"content": "hello"},
		},
	})

	session, err := New().Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, ok := session.Metadata["turns_with_unparsable_timestamp"]; ok {
		t.Errorf("clean session reports turns_with_unparsable_timestamp = %v",
			session.Metadata["turns_with_unparsable_timestamp"])
	}
}

func writeJeffLines(t *testing.T, path string, lines []map[string]any) {
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
