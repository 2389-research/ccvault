// ABOUTME: Tests for how the pi adapter reads pi's token counters onto turns.
// ABOUTME: Covers the four disjoint counters, the subsets that must not be added, and spend not on a message.

package pi

import (
	"testing"

	"github.com/2389-research/ccvault/pkg/adapter"
)

func TestParse_AssistantUsageFillsTheFourCounters(t *testing.T) {
	got := parseLines(t, alpha,
		`{"type":"message","id":"e1","parentId":null,"timestamp":"2025-12-09T00:54:05.000Z","message":{"role":"assistant","content":[{"type":"text","text":"hi"}],"stopReason":"stop","timestamp":1765233665294,"usage":{"input":2775,"output":141,"cacheRead":9000,"cacheWrite":1200,"totalTokens":13116,"cost":{"input":0.013,"output":0.003,"cacheRead":0,"cacheWrite":0,"total":0.017}}}}`,
	)

	if len(got.Turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(got.Turns))
	}
	turn := got.Turns[0]
	if turn.InputTokens != 2775 {
		t.Errorf("InputTokens = %d, want 2775", turn.InputTokens)
	}
	if turn.OutputTokens != 141 {
		t.Errorf("OutputTokens = %d, want 141", turn.OutputTokens)
	}
	if turn.CacheReadTokens != 9000 {
		t.Errorf("CacheReadTokens = %d, want 9000", turn.CacheReadTokens)
	}
	if turn.CacheWriteTokens != 1200 {
		t.Errorf("CacheWriteTokens = %d, want 1200", turn.CacheWriteTokens)
	}
}

// TestParse_ReasoningAndHourCacheAreNotAddedAgain pins the one way this
// mapping could inflate a session. pi documents reasoning as a subset of
// output and cacheWrite1h as a subset of cacheWrite, and the session row sums
// all four counters, so adding either would charge those tokens twice —
// which is the shape of #155.
func TestParse_ReasoningAndHourCacheAreNotAddedAgain(t *testing.T) {
	got := parseLines(t, alpha,
		`{"type":"message","id":"e1","parentId":null,"timestamp":"2025-12-09T00:54:05.000Z","message":{"role":"assistant","content":[{"type":"text","text":"hi"}],"stopReason":"stop","timestamp":1765233665294,"usage":{"input":100,"output":500,"cacheRead":0,"cacheWrite":80,"cacheWrite1h":80,"reasoning":400,"totalTokens":680,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}}}}`,
	)

	totals := got.TokenTotals()
	if totals.Output != 500 {
		t.Errorf("Output = %d, want 500 — reasoning is already inside it", totals.Output)
	}
	if totals.CacheWrite != 80 {
		t.Errorf("CacheWrite = %d, want 80 — cacheWrite1h is already inside it", totals.CacheWrite)
	}
	if totals.Total() != 680 {
		t.Errorf("Total() = %d, want 680 to match pi's own totalTokens", totals.Total())
	}
}

// TestParse_AUsageEntryBecomesItsOwnTurn covers spend pi records without a
// message to hang it on — cache warming, for instance.
//
// It is stored as a turn rather than folded into the turn before it, and that
// is the whole point: folding is what the codex adapter does with its
// token_count lines, and because those counts never reach turns.raw_json,
// every legacy codex session re-derives to zero and can never be repaired. A
// turn of its own keeps the numbers in the archive's own record of them.
func TestParse_AUsageEntryBecomesItsOwnTurn(t *testing.T) {
	got := parseLines(t, alpha,
		`{"type":"message","id":"e1","parentId":null,"timestamp":"2025-12-09T00:54:00.000Z","message":{"role":"user","content":[{"type":"text","text":"hi"}],"timestamp":1765233665292}}`,
		`{"type":"usage","id":"e2","parentId":"e1","timestamp":"2025-12-09T00:54:01.000Z","kind":"cache_warm","provider":"anthropic","model":"claude-opus-4-5","usage":{"input":12,"output":0,"cacheRead":0,"cacheWrite":34000,"totalTokens":34012,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0.04}}}`,
	)

	if len(got.Turns) != 2 {
		t.Fatalf("got %d turns, want 2", len(got.Turns))
	}
	usageTurn := got.Turns[1]
	if usageTurn.Type != "system" {
		t.Errorf("Type = %q, want %q", usageTurn.Type, "system")
	}
	if usageTurn.CacheWriteTokens != 34000 || usageTurn.InputTokens != 12 {
		t.Errorf("usage turn counters = input %d / cacheWrite %d, want 12 / 34000",
			usageTurn.InputTokens, usageTurn.CacheWriteTokens)
	}
	if len(usageTurn.RawJSON) == 0 {
		t.Error("usage turn has no RawJSON, so the counters could never be re-derived from the archive")
	}
	if usageTurn.Content != "cache_warm" {
		t.Errorf("Content = %q, want %q", usageTurn.Content, "cache_warm")
	}
	if got.Turns[0].CacheWriteTokens != 0 {
		t.Errorf("the preceding turn picked up %d cache-write tokens; the usage entry's spend is its own",
			got.Turns[0].CacheWriteTokens)
	}
}

func TestParse_AUsageEntryNoteIsKept(t *testing.T) {
	got := parseLines(t, alpha,
		`{"type":"usage","id":"e1","parentId":null,"timestamp":"2025-12-09T00:54:01.000Z","kind":"cache_warm","provider":"anthropic","model":"claude-opus-4-5","note":"refreshed before compaction","usage":{"input":0,"output":0,"cacheRead":0,"cacheWrite":10,"totalTokens":10,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}}}`,
	)
	if len(got.Turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(got.Turns))
	}
	if got.Turns[0].Content != "cache_warm: refreshed before compaction" {
		t.Errorf("Content = %q, want %q", got.Turns[0].Content, "cache_warm: refreshed before compaction")
	}
}

// TestParse_CompactionBecomesATurnCarryingItsSummaryAndSpend keeps both
// halves of a compaction: the summary is the only record of what the
// conversation before it said, and the usage is what generating it cost.
func TestParse_CompactionBecomesATurnCarryingItsSummaryAndSpend(t *testing.T) {
	got := parseLines(t, alpha,
		`{"type":"compaction","id":"e1","parentId":null,"timestamp":"2025-12-09T00:54:01.000Z","summary":"we refactored the loop","firstKeptEntryId":"e0","tokensBefore":120000,"usage":{"input":118000,"output":900,"cacheRead":0,"cacheWrite":0,"totalTokens":118900,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}}}`,
	)

	if len(got.Turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(got.Turns))
	}
	turn := got.Turns[0]
	if turn.Type != "system" {
		t.Errorf("Type = %q, want %q", turn.Type, "system")
	}
	if turn.Content != "we refactored the loop" {
		t.Errorf("Content = %q, want the summary", turn.Content)
	}
	if turn.InputTokens != 118000 || turn.OutputTokens != 900 {
		t.Errorf("counters = input %d / output %d, want 118000 / 900", turn.InputTokens, turn.OutputTokens)
	}
}

// TestParse_AV1CompactionHasNoUsageAndStillBecomesATurn is the shape in pi's
// own fixture: the older compaction entries carry a summary and
// firstKeptEntryIndex but no usage at all, and zero counters there are an
// absence rather than free work.
func TestParse_AV1CompactionHasNoUsageAndStillBecomesATurn(t *testing.T) {
	got := parseLines(t, alphaV1,
		`{"type":"compaction","timestamp":"2025-12-08T23:22:54.411Z","summary":"# Context Checkpoint","firstKeptEntryIndex":279,"tokensBefore":138567}`,
	)

	if len(got.Turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(got.Turns))
	}
	if got.Turns[0].Content != "# Context Checkpoint" {
		t.Errorf("Content = %q, want the summary", got.Turns[0].Content)
	}
	if got.TokenTotals().Total() != 0 {
		t.Errorf("Total() = %d, want 0 for a compaction that recorded no usage", got.TokenTotals().Total())
	}
}

func TestParse_BranchSummaryBecomesATurnCarryingItsSummaryAndSpend(t *testing.T) {
	got := parseLines(t, alpha,
		`{"type":"branch_summary","id":"e1","parentId":null,"timestamp":"2025-12-09T00:54:01.000Z","fromId":"e0","summary":"the branch tried generics","usage":{"input":40,"output":60,"cacheRead":0,"cacheWrite":0,"totalTokens":100,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}}}`,
	)

	if len(got.Turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(got.Turns))
	}
	if got.Turns[0].Content != "the branch tried generics" {
		t.Errorf("Content = %q, want the summary", got.Turns[0].Content)
	}
	if got.TokenTotals().Total() != 100 {
		t.Errorf("Total() = %d, want 100", got.TokenTotals().Total())
	}
}

// TestParse_SessionTotalIsTheSumOfEveryKindOfSpend is the number the session
// row carries, and it has to count the spend that is not on an assistant
// message as well as the spend that is.
func TestParse_SessionTotalIsTheSumOfEveryKindOfSpend(t *testing.T) {
	got := parseLines(t, alpha,
		`{"type":"message","id":"e1","parentId":null,"timestamp":"2025-12-09T00:54:05.000Z","message":{"role":"assistant","content":[{"type":"text","text":"hi"}],"stopReason":"stop","timestamp":1765233665294,"usage":{"input":10,"output":20,"cacheRead":30,"cacheWrite":40,"totalTokens":100,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}}}}`,
		`{"type":"usage","id":"e2","parentId":"e1","timestamp":"2025-12-09T00:54:06.000Z","kind":"cache_warm","provider":"anthropic","model":"claude-opus-4-5","usage":{"input":1,"output":2,"cacheRead":3,"cacheWrite":4,"totalTokens":10,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}}}`,
	)

	want := adapter.TokenTotals{Input: 11, Output: 22, CacheRead: 33, CacheWrite: 44}
	if got := got.TokenTotals(); got != want {
		t.Errorf("TokenTotals() = %+v, want %+v", got, want)
	}
}
