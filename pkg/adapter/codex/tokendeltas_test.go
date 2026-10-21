// ABOUTME: Tests that codex token deltas survive a token_count event arriving before any assistant turn, and that a forked rollout keeps its own session id.
// ABOUTME: Guards #166 — the lastAssistantIdx guard discarded real spend, and the last session_meta line collapsed 12 real rollouts onto one ancestor id.

package codex

import (
	"path/filepath"
	"testing"
)

// metaLine is the ordinary single-session_meta opening line.
func metaLine(id, cwd string) map[string]any {
	return map[string]any{
		"timestamp": "2026-03-11T16:25:01.231Z",
		"type":      "session_meta",
		"payload":   map[string]any{"id": id, "cwd": cwd},
	}
}

// tokenCountLine is one cumulative total_token_usage reading. Codex reports
// cached_input_tokens as a share of input_tokens, not beside it.
func tokenCountLine(ts string, input, cached, output int) map[string]any {
	return map[string]any{
		"timestamp": ts,
		"type":      "event_msg",
		"payload": map[string]any{
			"type": "token_count",
			"info": map[string]any{
				"total_token_usage": map[string]any{
					"input_tokens":        input,
					"cached_input_tokens": cached,
					"output_tokens":       output,
					"total_tokens":        input + output,
				},
			},
		},
	}
}

func assistantLine(ts, text string) map[string]any {
	return map[string]any{
		"timestamp": ts,
		"type":      "response_item",
		"payload": map[string]any{
			"type":    "message",
			"role":    "assistant",
			"content": []map[string]any{{"type": "output_text", "text": text}},
		},
	}
}

func userLine(ts, text string) map[string]any {
	return map[string]any{
		"timestamp": ts,
		"type":      "response_item",
		"payload": map[string]any{
			"type":    "message",
			"role":    "user",
			"content": []map[string]any{{"type": "input_text", "text": text}},
		},
	}
}

func toolCallLine(ts, name, callID, args string) map[string]any {
	return map[string]any{
		"timestamp": ts,
		"type":      "response_item",
		"payload": map[string]any{
			"type":      "function_call",
			"name":      name,
			"call_id":   callID,
			"arguments": args,
		},
	}
}

// TestParse_TokenCountBeforeAnyAssistantTurnIsNotDropped is #166's first half.
//
// Codex only materialises a turn for a response_item/message, so a model
// response that was nothing but tool calls produces no turn at all. A run that
// works through tools for a long stretch before it says anything therefore
// emits token_count events while lastAssistantIdx is still -1, and the guard
// threw those deltas away. Measured across 148 real rollouts on this machine,
// 20 lost deltas this way and the worst — 2,302 lines, one assistant message,
// 366 token_count events — lost 365 of them, 36.7M of 36.9M tokens.
//
// The spend is real either way, and the counters it feeds live on the session
// row, so the deltas are held and applied to the first assistant turn that
// appears. Lumpy per-turn, exact per-session; attributing to a turn that does
// not exist is the only option that is not available.
func TestParse_TokenCountBeforeAnyAssistantTurnIsNotDropped(t *testing.T) {
	dir := t.TempDir()
	fpath := filepath.Join(dir, "rollout-2026-03-11T11-24-50-019cddb7-1979-7983-b176-785af4de7cbf.jsonl")

	writeJSONLFile(t, fpath, []map[string]any{
		metaLine("019cddb7-1979-7983-b176-785af4de7cbf", "/Users/dylanr/work/project"),
		userLine("2026-03-11T16:25:02.000Z", "go read the repo"),
		// Two tool-calling rounds, each billed, neither producing a turn.
		toolCallLine("2026-03-11T16:25:03.000Z", "shell", "c1", `{"command":"ls"}`),
		tokenCountLine("2026-03-11T16:25:04.000Z", 9554, 7552, 451),
		toolCallLine("2026-03-11T16:25:05.000Z", "shell", "c2", `{"command":"cat go.mod"}`),
		tokenCountLine("2026-03-11T16:25:06.000Z", 21000, 17000, 900),
		// The run finally says something, and is billed for that too.
		assistantLine("2026-03-11T16:25:07.000Z", "here is what the repo does"),
		tokenCountLine("2026-03-11T16:25:08.000Z", 30000, 24000, 1200),
	})

	session, err := New().Parse(fpath)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	var input, cacheRead, output int64
	for _, turn := range session.Turns {
		input += turn.InputTokens
		cacheRead += turn.CacheReadTokens
		output += turn.OutputTokens
	}

	// The final cumulative reading is the whole of the rollout's spend, so the
	// split must reproduce it exactly.
	if input+cacheRead != 30000 {
		t.Errorf("input(%d) + cacheRead(%d) = %d, want 30000 — deltas arriving before the first assistant turn were dropped",
			input, cacheRead, input+cacheRead)
	}
	if output != 1200 {
		t.Errorf("output = %d, want 1200", output)
	}

	// The subset semantics #155 established must survive the rescue: codex's
	// cached figure is contained in input_tokens, so it is moved out of input
	// rather than added alongside it. Copying it would double-count by 24,000.
	if cacheRead != 24000 {
		t.Errorf("cacheRead = %d, want 24000 (the cumulative cached share, moved out of input not copied)", cacheRead)
	}
	if input != 30000-24000 {
		t.Errorf("input = %d, want %d (fresh input only)", input, 30000-24000)
	}
	if cacheRead > input+cacheRead {
		t.Errorf("cacheRead(%d) exceeds total input(%d) — the subset was treated as a sibling", cacheRead, input+cacheRead)
	}

	// Held deltas land on the assistant turn, not on the user turn that
	// preceded them: the spend paid for the model's work.
	var assistantIdx = -1
	for i, turn := range session.Turns {
		if turn.Type == "assistant" {
			assistantIdx = i
		}
	}
	if assistantIdx < 0 {
		t.Fatalf("no assistant turn parsed from %d turns", len(session.Turns))
	}
	if session.Turns[assistantIdx].InputTokens+session.Turns[assistantIdx].CacheReadTokens != 30000 {
		t.Errorf("the assistant turn carries %d input+cacheRead, want the whole 30000",
			session.Turns[assistantIdx].InputTokens+session.Turns[assistantIdx].CacheReadTokens)
	}
}

// A rollout that never produces an assistant turn would still have nowhere to
// put its deltas. None of the 148 real rollouts on this machine is that shape —
// every one with a token_count event has at least one assistant message — but
// the fallback keeps the session total right rather than silently zero, which
// is the whole complaint in #166.
func TestParse_TokenCountWithNoAssistantTurnAtAllLandsOnTheLastTurn(t *testing.T) {
	dir := t.TempDir()
	fpath := filepath.Join(dir, "rollout-2026-03-11T11-24-50-019cddb7-1979-7983-b176-785af4de7cbf.jsonl")

	writeJSONLFile(t, fpath, []map[string]any{
		metaLine("019cddb7-1979-7983-b176-785af4de7cbf", "/Users/dylanr/work/project"),
		userLine("2026-03-11T16:25:02.000Z", "go read the repo"),
		toolCallLine("2026-03-11T16:25:03.000Z", "shell", "c1", `{"command":"ls"}`),
		tokenCountLine("2026-03-11T16:25:04.000Z", 9554, 7552, 451),
	})

	session, err := New().Parse(fpath)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(session.Turns) == 0 {
		t.Fatalf("no turns parsed")
	}

	var input, cacheRead, output int64
	for _, turn := range session.Turns {
		input += turn.InputTokens
		cacheRead += turn.CacheReadTokens
		output += turn.OutputTokens
	}
	if input+cacheRead != 9554 || output != 451 {
		t.Errorf("input+cacheRead = %d, output = %d; want 9554 and 451 — the session's only spend was discarded",
			input+cacheRead, output)
	}
	if cacheRead != 7552 {
		t.Errorf("cacheRead = %d, want 7552", cacheRead)
	}
}

// TestParse_ForkedRolloutKeepsItsOwnSessionID is #166's second half, and the
// reason one rollout "produced no session row at all".
//
// A codex rollout that was forked — resumed, or spawned as a subagent — opens
// with its own session_meta and then replays the ancestor's, which names the
// ancestor's id. Parse assigned sessionID on every session_meta, so the last
// one won and the rollout took the ancestor's identity. On this machine 12
// distinct rollouts all resolve to ancestor 01a0c9c5-024f-7620-86dc-b0a234c95409
// that way; syncing them writes 12 files into one session row and the last file
// synced overwrites the rest, so 11 rollouts leave no row of their own. Not
// undercounted — gone.
//
// Discover already reads only the first session_meta and keys the file on that
// id, so first-wins is also what makes Parse agree with the half of the adapter
// that was right.
func TestParse_ForkedRolloutKeepsItsOwnSessionID(t *testing.T) {
	dir := t.TempDir()
	fpath := filepath.Join(dir, "rollout-2026-09-11T13-54-37-01a091d2-5ba3-79a2-9379-b1267cd825f8.jsonl")

	// The shape a real forked rollout has: line 0 is this rollout, line 1 is
	// the replayed ancestor it was forked from, carrying an older cwd.
	writeJSONLFile(t, fpath, []map[string]any{
		{
			"timestamp": "2026-09-11T18:54:37.511Z",
			"type":      "session_meta",
			"payload": map[string]any{
				"id":               "01a091d2-5ba3-79a2-9379-b1267cd825f8",
				"forked_from_id":   "01a090ee-9c75-79a2-88d0-94b40b3920e3",
				"parent_thread_id": "01a090ee-9c75-79a2-88d0-94b40b3920e3",
				"cwd":              "/Users/dylanr/work/2389/2389.ai",
				"git":              map[string]any{"branch": "fork-branch"},
			},
		},
		{
			"timestamp": "2026-09-11T18:54:37.511Z",
			"type":      "session_meta",
			"payload": map[string]any{
				"id":  "01a090ee-9c75-79a2-88d0-94b40b3920e3",
				"cwd": "/Users/dylanr/work/2389/ancestor",
				"git": map[string]any{"branch": "ancestor-branch"},
			},
		},
		assistantLine("2026-09-11T18:54:40.000Z", "working on it"),
	})

	session, err := New().Parse(fpath)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if session.ID != "01a091d2-5ba3-79a2-9379-b1267cd825f8" {
		t.Errorf("session ID = %q, want %q — the replayed ancestor's session_meta took the rollout's identity",
			session.ID, "01a091d2-5ba3-79a2-9379-b1267cd825f8")
	}
	// Discover keys the file on the first session_meta's cwd; Parse must agree,
	// or the session lands in a project the discovery pass never named.
	if session.ProjectPath != "/Users/dylanr/work/2389/2389.ai" {
		t.Errorf("ProjectPath = %q, want %q", session.ProjectPath, "/Users/dylanr/work/2389/2389.ai")
	}
	if session.GitBranch != "fork-branch" {
		t.Errorf("GitBranch = %q, want %q", session.GitBranch, "fork-branch")
	}
	// Turn ids are derived from the session id, so the wrong id also mislabels
	// every turn in the rollout.
	if len(session.Turns) != 1 {
		t.Fatalf("expected 1 turn, got %d", len(session.Turns))
	}
	if want := "01a091d2-5ba3-79a2-9379-b1267cd825f8-turn-1"; session.Turns[0].ID != want {
		t.Errorf("turn ID = %q, want %q", session.Turns[0].ID, want)
	}
}

// TestParse_CumulativeCounterResetIsNotANegativeDelta covers the third way the
// token_count branch loses to real data, found while measuring the other two.
//
// total_token_usage is cumulative, but it restarts inside a single rollout when
// the context is cleared. A real reading drops from 767,935 to 40,766 mid-file.
// The delta then goes negative, and the branch's guard only zeroed the *cached*
// delta, so the negative input delta was added to the turn: one synced session
// held input_tokens = -399,066. A counter that went down has restarted, so the
// new reading is itself the delta.
func TestParse_CumulativeCounterResetIsNotANegativeDelta(t *testing.T) {
	dir := t.TempDir()
	fpath := filepath.Join(dir, "rollout-2026-03-11T11-24-50-019cddb7-1979-7983-b176-785af4de7cbf.jsonl")

	writeJSONLFile(t, fpath, []map[string]any{
		metaLine("019cddb7-1979-7983-b176-785af4de7cbf", "/Users/dylanr/work/project"),
		assistantLine("2026-03-11T16:25:02.000Z", "before the clear"),
		tokenCountLine("2026-03-11T16:25:03.000Z", 66095, 49280, 1286),
		assistantLine("2026-03-11T16:25:04.000Z", "after the clear"),
		// The counter restarts: 40766 is spend since the reset, not a decrease.
		tokenCountLine("2026-03-11T16:25:05.000Z", 40766, 9600, 627),
	})

	session, err := New().Parse(fpath)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	var input, cacheRead, output int64
	for _, turn := range session.Turns {
		input += turn.InputTokens
		cacheRead += turn.CacheReadTokens
		output += turn.OutputTokens
		if turn.InputTokens < 0 || turn.CacheReadTokens < 0 || turn.OutputTokens < 0 {
			t.Errorf("turn %s has a negative counter: input=%d cacheRead=%d output=%d",
				turn.ID, turn.InputTokens, turn.CacheReadTokens, turn.OutputTokens)
		}
	}

	if want := int64(66095 + 40766); input+cacheRead != want {
		t.Errorf("input(%d) + cacheRead(%d) = %d, want %d — the two cumulative runs sum to the rollout's spend",
			input, cacheRead, input+cacheRead, want)
	}
	if want := int64(49280 + 9600); cacheRead != want {
		t.Errorf("cacheRead = %d, want %d", cacheRead, want)
	}
	if want := int64(1286 + 627); output != want {
		t.Errorf("output = %d, want %d", output, want)
	}
}
