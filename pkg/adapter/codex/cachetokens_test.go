// ABOUTME: Tests that the codex adapter reports cached_input_tokens separately from fresh input tokens.
// ABOUTME: Guards #155 for codex, whose cached count is a SUBSET of input_tokens rather than a sibling of it.

package codex

import (
	"path/filepath"
	"testing"
)

// TestParse_SplitsCachedInputTokens pins codex's half of #155.
//
// Codex's token_count event reports a cumulative total_token_usage whose
// cached_input_tokens is *contained in* input_tokens — a real event reads
// {"input_tokens":8870,"cached_input_tokens":7040,"output_tokens":332,
// "total_tokens":9202}, and 8870+332 = 9202, so the cached figure is not
// added on top. Anthropic's shape is the opposite: cache_read_input_tokens
// sits beside input_tokens and both are added.
//
// cache_read_tokens has to mean the same thing in the column whichever source
// filled it, so the adapter subtracts the cached part out of input and reports
// it as a cache read. The session's total is unchanged by the split; what
// changes is that the cached share stops being invisible.
func TestParse_SplitsCachedInputTokens(t *testing.T) {
	dir := t.TempDir()
	fpath := filepath.Join(dir, "rollout-2026-03-11T11-24-50-019cddb7-1979-7983-b176-785af4de7cbf.jsonl")

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
			"timestamp": "2026-03-11T16:25:01.232Z",
			"type":      "response_item",
			"payload": map[string]any{
				"type": "message",
				"role": "assistant",
				"content": []map[string]any{
					{"type": "output_text", "text": "first answer"},
				},
			},
		},
		{
			"timestamp": "2026-03-11T16:25:08.133Z",
			"type":      "event_msg",
			"payload": map[string]any{
				"type": "token_count",
				"info": map[string]any{
					"total_token_usage": map[string]any{
						"input_tokens":        8870,
						"cached_input_tokens": 7040,
						"output_tokens":       332,
						"total_tokens":        9202,
					},
				},
			},
		},
		{
			"timestamp": "2026-03-11T16:25:10.000Z",
			"type":      "response_item",
			"payload": map[string]any{
				"type": "message",
				"role": "assistant",
				"content": []map[string]any{
					{"type": "output_text", "text": "second answer"},
				},
			},
		},
		// The second cumulative reading. Deltas, not totals, land on the turn:
		// 19477-8870 fresh+cached input, 16128-7040 cached.
		{
			"timestamp": "2026-03-11T16:25:16.045Z",
			"type":      "event_msg",
			"payload": map[string]any{
				"type": "token_count",
				"info": map[string]any{
					"total_token_usage": map[string]any{
						"input_tokens":        19477,
						"cached_input_tokens": 16128,
						"output_tokens":       672,
						"total_tokens":        20149,
					},
				},
			},
		},
	})

	session, err := New().Parse(fpath)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(session.Turns) != 2 {
		t.Fatalf("expected 2 turns, got %d", len(session.Turns))
	}

	for i, want := range []struct {
		input, cacheRead, output int64
	}{
		{input: 8870 - 7040, cacheRead: 7040, output: 332},
		{input: (19477 - 8870) - (16128 - 7040), cacheRead: 16128 - 7040, output: 672 - 332},
	} {
		turn := session.Turns[i]
		if turn.InputTokens != want.input {
			t.Errorf("turn %d InputTokens = %d, want %d (fresh input only)", i, turn.InputTokens, want.input)
		}
		if turn.CacheReadTokens != want.cacheRead {
			t.Errorf("turn %d CacheReadTokens = %d, want %d", i, turn.CacheReadTokens, want.cacheRead)
		}
		if turn.OutputTokens != want.output {
			t.Errorf("turn %d OutputTokens = %d, want %d", i, turn.OutputTokens, want.output)
		}
		// Codex reports no cache-creation figure at all, so claiming one would
		// be invention.
		if turn.CacheWriteTokens != 0 {
			t.Errorf("turn %d CacheWriteTokens = %d, want 0; codex records no cache-creation count",
				i, turn.CacheWriteTokens)
		}
	}

	// The split must be total-preserving: input+cacheRead across both turns is
	// the final cumulative input_tokens codex reported.
	var input, cacheRead int64
	for _, turn := range session.Turns {
		input += turn.InputTokens
		cacheRead += turn.CacheReadTokens
	}
	if input+cacheRead != 19477 {
		t.Errorf("input(%d) + cacheRead(%d) = %d, want 19477 — the split changed the total",
			input, cacheRead, input+cacheRead)
	}
}
