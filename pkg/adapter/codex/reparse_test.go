// ABOUTME: Tests that ParseTranscript applies codex's subset split, not just Parse.
// ABOUTME: The backfill re-derives through ParseTranscript, so this is the path that must not copy cached_input_tokens.

package codex

import (
	"bytes"
	"os"
	"testing"

	"github.com/2389-research/ccvault/pkg/adapter"
)

// TestParseTranscript_AppliesTheSubsetSplit is the case a second
// implementation of the per-source rule would have got wrong.
//
// codex's cached_input_tokens is *contained in* input_tokens — a real reading
// is input 9554 / cached 7552 / output 451 / total 10005, and 9554+451 =
// 10005 — the opposite of Claude Code's shape, where the cache counters sit
// beside input_tokens and are added to it. The session row sums all four
// columns, so copying codex's cached figure across would inflate every codex
// session by the amount it reports.
//
// Re-deriving a session from the lines the archive stored has to reach exactly
// the same answer as the sync that first read them, which is why it runs
// through the adapter rather than through a re-statement of the rule in SQL.
func TestParseTranscript_AppliesTheSubsetSplit(t *testing.T) {
	transcript := bytes.Join([][]byte{
		[]byte(`{"timestamp":"2026-03-11T16:25:01.231Z","type":"session_meta","payload":{"id":"019cddb7-1979-7983-b176-785af4de7cbf","cwd":"/Users/test/codexproject"}}`),
		[]byte(`{"timestamp":"2026-03-11T16:25:01.232Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"an answer"}]}}`),
		[]byte(`{"timestamp":"2026-03-11T16:25:08.133Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":9554,"cached_input_tokens":7552,"output_tokens":451,"total_tokens":10005}}}}`),
		nil,
	}, []byte("\n"))

	session, err := New().ParseTranscript(
		"/Users/test/.codex/sessions/2026/03/11/rollout-2026-03-11T11-24-50-019cddb7-1979-7983-b176-785af4de7cbf.jsonl",
		bytes.NewReader(transcript))
	if err != nil {
		t.Fatalf("ParseTranscript: %v", err)
	}

	totals := session.TokenTotals()
	if totals.CacheRead != 7552 {
		t.Errorf("CacheRead = %d, want 7552", totals.CacheRead)
	}
	if totals.Input != 9554-7552 {
		t.Errorf("Input = %d, want %d — the cached share has to be moved out of input, not copied beside it",
			totals.Input, 9554-7552)
	}
	if totals.Output != 451 {
		t.Errorf("Output = %d, want 451", totals.Output)
	}
	// codex reports no cache-creation figure at all, so claiming one would be
	// invention.
	if totals.CacheWrite != 0 {
		t.Errorf("CacheWrite = %d, want 0; codex records no cache-creation count", totals.CacheWrite)
	}
	// The split is total-preserving: codex's own total_tokens, unchanged.
	if totals.Total() != 10005 {
		t.Errorf("Total() = %d, want 10005 — the split moved the total", totals.Total())
	}
}

// TestParseTranscript_MatchesParse keeps the two entry points honest about
// being one implementation.
func TestParseTranscript_MatchesParse(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/rollout-2026-03-11T11-24-50-019cddb7-1979-7983-b176-785af4de7cbf.jsonl"

	writeJSONLFile(t, path, []map[string]any{
		{
			"timestamp": "2026-03-11T16:25:01.231Z",
			"type":      "session_meta",
			"payload":   map[string]any{"id": "019cddb7-1979-7983-b176-785af4de7cbf", "cwd": "/Users/test/p"},
		},
		{
			"timestamp": "2026-03-11T16:25:01.232Z",
			"type":      "response_item",
			"payload": map[string]any{
				"type": "message", "role": "assistant",
				"content": []map[string]any{{"type": "output_text", "text": "hi"}},
			},
		},
		{
			"timestamp": "2026-03-11T16:25:08.133Z",
			"type":      "event_msg",
			"payload": map[string]any{
				"type": "token_count",
				"info": map[string]any{"total_token_usage": map[string]any{
					"input_tokens": 100, "cached_input_tokens": 60, "output_tokens": 7, "total_tokens": 107,
				}},
			},
		},
	})

	fromFile, err := New().Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture back: %v", err)
	}
	fromReader, err := New().ParseTranscript(path, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("ParseTranscript: %v", err)
	}

	if fromFile.ID != fromReader.ID {
		t.Errorf("ID: file %q, reader %q", fromFile.ID, fromReader.ID)
	}
	if fromFile.ProjectPath != fromReader.ProjectPath {
		t.Errorf("ProjectPath: file %q, reader %q", fromFile.ProjectPath, fromReader.ProjectPath)
	}
	if len(fromFile.Turns) != len(fromReader.Turns) {
		t.Fatalf("turns: file %d, reader %d", len(fromFile.Turns), len(fromReader.Turns))
	}
	if got, want := fromReader.TokenTotals(), fromFile.TokenTotals(); got != want {
		t.Errorf("TokenTotals: file %+v, reader %+v", want, got)
	}
	// Compile-time proof the adapter is usable as the backfill uses it.
	var _ adapter.TranscriptReparser = New()
}
