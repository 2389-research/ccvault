// ABOUTME: Tests that the codex adapter reports turns whose transcript timestamp its layout could not read.
// ABOUTME: Guards #146 for codex, whose single RFC3339Nano parse drops the error and leaves the turn at the zero time.

package codex

import (
	"path/filepath"
	"testing"
)

// TestParse_ReportsUnparsableTimestamps pins codex's half of #146.
//
// codex parses its line timestamps with one layout and discards the error, so
// a line it cannot read becomes a turn at the zero time — January 1 year 1,
// which sorts to the very start of the archive and falls outside every date
// filter, while looking exactly like a turn someone recorded at the epoch.
func TestParse_ReportsUnparsableTimestamps(t *testing.T) {
	dir := t.TempDir()
	fpath := filepath.Join(dir, "rollout-2026-03-11T11-24-50-019cddb7-1979-7983-b176-785af4de7cbf.jsonl")

	const bad = "11/03/2026 16:25:01"

	writeJSONLFile(t, fpath, []map[string]any{
		{
			"timestamp": "2026-03-11T16:25:01.231Z",
			"type":      "session_meta",
			"payload": map[string]any{
				"id":  "019cddb7-1979-7983-b176-785af4de7cbf",
				"cwd": "/Users/dylanr/work/project",
			},
		},
		{
			"timestamp": "2026-03-11T16:25:02.000Z",
			"type":      "response_item",
			"payload": map[string]any{
				"type": "message",
				"role": "user",
				"content": []map[string]any{
					{"type": "input_text", "text": "readable"},
				},
			},
		},
		{
			"timestamp": bad,
			"type":      "response_item",
			"payload": map[string]any{
				"type": "message",
				"role": "assistant",
				"content": []map[string]any{
					{"type": "output_text", "text": "unreadable clock"},
				},
			},
		},
	})

	session, err := New().Parse(fpath)
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

	// A clean session must say nothing at all, so the key's presence is the
	// signal and an empty archive stays quiet.
	writeMinimalSession(t, dir, "rollout-clean-019cddb7-1979-7983-b176-785af4de7cbe.jsonl", "/tmp/clean")
	clean, err := New().Parse(filepath.Join(dir, "rollout-clean-019cddb7-1979-7983-b176-785af4de7cbe.jsonl"))
	if err != nil {
		t.Fatalf("Parse clean: %v", err)
	}
	if _, ok := clean.Metadata["turns_with_unparsable_timestamp"]; ok {
		t.Errorf("clean session reports turns_with_unparsable_timestamp = %v",
			clean.Metadata["turns_with_unparsable_timestamp"])
	}
}
